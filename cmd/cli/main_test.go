package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/danielmalka/go-knowrag/internal/clicmd"
	"github.com/danielmalka/go-knowrag/internal/config"
	"github.com/danielmalka/go-knowrag/internal/ingest/lock"
)

// rootHelp renders the top-level help the way an operator sees it, from a configuration that is
// empty on purpose: help has to work on a host nobody has configured yet.
func rootHelp(t *testing.T) string {
	t.Helper()

	var out bytes.Buffer
	root := newRootCmd(&config.Config{})
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("knowrag --help: %v", err)
	}
	return out.String()
}

// TestRootHelp_DeclaresThePrivilegedTool covers the acceptance criterion that `--help` says what
// this binary is. It is not decoration: nothing in any single subcommand's help reveals that
// --tenant is unrestricted and that one flag reaches content no MCP client can request, so the
// top-level text is the only place an operator learns what handing over this binary hands over.
func TestRootHelp_DeclaresThePrivilegedTool(t *testing.T) {
	help := rootHelp(t)

	for _, want := range []string{
		"privileged",
		"administrative",
		config.AdminQdrantAPIKeyEnv,
		"--include-private",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("the top-level help does not mention %q:\n%s", want, help)
		}
	}

	for _, unwanted := range []string{
		// The two wordings this replaced, refused rather than merely superseded. Each listed what
		// was wired up and each went stale the moment a command joined the tree; left in place they
		// would keep telling an operator that a command they just ran does not exist. A test that
		// only checks the new sentence is present would pass with all three in the file.
		"Schema provisioning and ingestion are wired up",
		"Schema provisioning, ingestion and search are wired up",
		// Naming the MCP server's runtime key here would send an operator to export the wrong
		// variable, and the wrong one is the scoped one — the failure would be a permission error
		// against a key that is doing its job.
		"MCP_QDRANT_API_KEY",
	} {
		if strings.Contains(help, unwanted) {
			t.Errorf("the top-level help still carries %q:\n%s", unwanted, help)
		}
	}
}

// TestRootHelp_ListsEveryCommand keeps the help honest about the command tree, which is the half
// the prose above cannot state: the text may describe a tool that searches while the tree it
// documents has no search command in it.
//
// It reads cobra's generated command list rather than the whole help text, and that is the whole
// assertion. Searching the full text for "search" passes on the Long paragraph alone, which is
// prose about the tool — so a run with the command removed from the tree would still be green,
// telling the reader the command exists because a sentence mentions it.
func TestRootHelp_ListsEveryCommand(t *testing.T) {
	help := rootHelp(t)

	const header = "Available Commands:"
	start := strings.Index(help, header)
	if start < 0 {
		t.Fatalf("the help has no %q block, so there is no command list to read:\n%s", header, help)
	}
	list := help[start+len(header):]
	if end := strings.Index(list, "Flags:"); end >= 0 {
		list = list[:end]
	}

	for _, want := range []string{"schema", "ingest", "search", "stats", "eval", "golden"} {
		if !strings.Contains(list, want) {
			t.Errorf("the command list does not offer %q:\n%s", want, list)
		}
	}
}

// mcpRuntimeCredentialEnv is the variable the MCP server reads for its scoped runtime credential
// (cmd/mcp-server/config.go). It is spelled here rather than imported because the point of the test
// below is that this package cannot reach that value — importing the constant from a package main
// is impossible anyway, and a copy is what a future rename should break.
const mcpRuntimeCredentialEnv = "MCP_QDRANT_API_KEY" // #nosec G101 -- a variable name, not a credential

// TestCLI_NeverReadsTheMCPRuntimeCredential proves the separation ADR-002 §2.4 asks for, in the
// direction that can fail silently.
//
// The CLI is privileged by declaration: it takes any --tenant it is given. Its credential therefore
// has to be the administrative one, and a fallback to the MCP server's scoped runtime key would not
// break anything visibly — it would work, against a key chosen for a narrower job, and the
// separation would exist only in the documentation. The check is on source rather than on
// behaviour because a fallback is a path, and a behavioural test only ever covers the paths
// somebody thought of.
//
// It is scoped to string literals, not to the text of the file: a comment naming the variable is
// how a reader learns the two are different, and internal/config's does exactly that.
func TestCLI_NeverReadsTheMCPRuntimeCredential(t *testing.T) {
	root := moduleRoot(t)

	outside, inside := literalUsers(t, root, mcpRuntimeCredentialEnv)
	for _, file := range outside {
		t.Errorf("%s names %s as a string literal. That variable is the MCP server's scoped "+
			"runtime credential; everything the CLI links must read the administrative key (%s) "+
			"and must not fall back to it",
			file, mcpRuntimeCredentialEnv, config.AdminQdrantAPIKeyEnv)
	}
	// The non-vacuity half, and the reason it is an assertion rather than a comment: a walk with a
	// wrong root, a misspelled literal or a skip list that swallowed the tree passes silently and
	// forever. The variable does exist, in exactly one package, so a scan that finds it nowhere is a
	// scan that is looking at nothing.
	if len(inside) == 0 {
		t.Fatalf("the scan found %s nowhere in the module, not even in cmd/mcp-server, which reads "+
			"it — so this test is passing because it is looking at nothing", mcpRuntimeCredentialEnv)
	}
}

// mcpServerDir is the one package allowed to name the runtime credential's variable.
const mcpServerDir = "cmd/mcp-server"

// literalUsers walks the module and splits the non-test Go files that contain literal as a string
// constant into those outside cmd/mcp-server and those inside it.
//
// `_test.go` files are skipped for the same reason internal/archtest skips them: a test may
// legitimately name a variable the package it tests must never read — internal/store's does, to
// assert that an error message names neither entrypoint's key.
func literalUsers(t *testing.T, root, literal string) (outside, inside []string) {
	t.Helper()

	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)

		if d.IsDir() {
			name := d.Name()
			if rel != "." && (name == "vendor" || name == "testdata" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		// Parsed rather than grepped: a comment that names the variable is documentation, and the
		// invariant is about code that reads it.
		file, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		found := false
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if value, uerr := strconv.Unquote(lit.Value); uerr == nil && value == literal {
				found = true
			}
			return true
		})
		if !found {
			return nil
		}
		if strings.HasPrefix(rel, mcpServerDir+"/") {
			inside = append(inside, rel)
		} else {
			outside = append(outside, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return outside, inside
}

// moduleRoot returns the repository root and refuses to guess: a wrong root is how the scan above
// would go vacuous, so the go.mod check is part of the assertion rather than a convenience.
func moduleRoot(t *testing.T) string {
	t.Helper()

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving the module root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("no go.mod at %s — the walk root is wrong, so this test proves nothing: %v", root, err)
	}
	return root
}

// TestExitCodeFor_ARefusedCommandLineIsAUsageError covers the half the category mechanism cannot
// reach on its own.
//
// Cobra refuses a bad command line before any RunE runs, so those failures carry no category and
// used to land on the code that means "the run broke" — telling a scheduler to retry an invocation
// that will fail identically every time. The whole tree is driven here rather than a synthetic
// command, because what is under test is that every real command's RunE marks itself the way the
// mapping expects.
func TestExitCodeFor_ARefusedCommandLineIsAUsageError(t *testing.T) {
	tests := map[string][]string{
		"an unknown subcommand":         {"bogus"},
		"an unknown flag":               {"search", "text", "--bogus"},
		"a missing argument":            {"search"},
		"neither eval mode":             {"eval"},
		"both eval modes":               {"eval", "--golden", "--isolation"},
		"an argument a command refuses": {"stats", "extra"},
	}

	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			root := newRootCmd(&config.Config{})
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs(args)

			cmd, err := root.ExecuteC()
			if err == nil {
				t.Fatalf("knowrag %v was accepted", args)
			}
			if got := exitCodeFor(cmd, err); got != exitUsage {
				t.Errorf("knowrag %v exits %d, want %d — the command line is what has to change, "+
					"and %d reads as a failure worth retrying", args, got, exitUsage, got)
			}
		})
	}
}

// commandPackages are the two directories every `knowrag` subcommand is built in.
var commandPackages = []string{"cmd/cli", "internal/clicmd"}

// TestEveryRunE_SilencesUsageFirst is the invariant exitCodeFor rests on, checked mechanically
// instead of remembered.
//
// The mapping reads SilenceUsage to tell "cobra refused this command line" from "the command ran
// and failed", and that only works while every RunE sets the flag before it can fail. Nothing in
// cobra enforces it and nothing at run time notices it missing: a new subcommand that forgets is a
// subcommand whose every failure — an unreachable Qdrant included — exits on the usage code, which
// tells a scheduler to stop retrying an outage.
//
// It reads source rather than behaviour because the alternative is running each command for real,
// and the ones worth checking are exactly the ones that open sockets. Requiring it as the *first*
// statement is stricter than the invariant needs and is what makes it checkable at all: "before
// anything that can fail" is not a property a parser can decide.
func TestEveryRunE_SilencesUsageFirst(t *testing.T) {
	root := moduleRoot(t)

	found := 0
	for _, pkg := range commandPackages {
		for _, r := range runEBodies(t, filepath.Join(root, pkg)) {
			found++
			if !silencesUsageFirst(r.body) {
				t.Errorf("%s: this RunE does not set `cmd.SilenceUsage = true` as its first "+
					"statement. Without it every failure of this command exits on the usage code, "+
					"because exitCodeFor cannot tell it from a command line cobra refused", r.where)
			}
		}
	}
	// Non-vacuity, and it is an assertion rather than a comment for the reason the credential scan
	// above states: a walker with a wrong root or a matcher that stopped matching passes silently
	// and forever. There are five commands with a RunE in this tree; the floor is below that so
	// adding one is not a failure, and far enough above zero to catch a walk that found nothing.
	if found < 5 {
		t.Fatalf("the walk found %d RunE function(s) in %v — there are more than that, so this test "+
			"is passing because it is looking at nothing", found, commandPackages)
	}
}

// runE is one `RunE:` field found in a command literal, with enough location to fix it.
type runE struct {
	where string
	body  *ast.FuncLit
}

func runEBodies(t *testing.T, dir string) []runE {
	t.Helper()

	fset := token.NewFileSet()
	pkgFiles, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("globbing %s: %v", dir, err)
	}

	var out []runE
	for _, path := range pkgFiles {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parsing %s: %v", path, perr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			kv, ok := n.(*ast.KeyValueExpr)
			if !ok {
				return true
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok || key.Name != "RunE" {
				return true
			}
			lit, ok := kv.Value.(*ast.FuncLit)
			if !ok {
				// A RunE assigned from a named function is not wrong, but this walker cannot follow
				// it — and a check that quietly skips what it cannot read is the vacuum this test
				// exists to avoid.
				t.Errorf("%s: RunE is not a function literal, so this test cannot read it",
					fset.Position(kv.Pos()))
				return true
			}
			out = append(out, runE{where: fset.Position(kv.Pos()).String(), body: lit})
			return true
		})
	}
	return out
}

// silencesUsageFirst reports whether the literal's first statement is `<param>.SilenceUsage = true`,
// where <param> is whatever the body named cobra's command argument.
func silencesUsageFirst(lit *ast.FuncLit) bool {
	if len(lit.Body.List) == 0 || len(lit.Type.Params.List) == 0 ||
		len(lit.Type.Params.List[0].Names) == 0 {
		return false
	}
	param := lit.Type.Params.List[0].Names[0].Name

	assign, ok := lit.Body.List[0].(*ast.AssignStmt)
	if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
		return false
	}
	sel, ok := assign.Lhs[0].(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "SilenceUsage" {
		return false
	}
	recv, ok := sel.X.(*ast.Ident)
	if !ok || recv.Name != param {
		return false
	}
	value, ok := assign.Rhs[0].(*ast.Ident)
	return ok && value.Name == "true"
}

// TestExitCodeFor_AFailureInsideACommandKeepsItsCategory is the other direction, and the reason the
// case above cannot simply be "cobra returned an error". A command that got as far as running and
// then failed answers for its own category; only a command line cobra never accepted is a usage
// error.
//
// The command it runs against is synthetic and carries SilenceUsage already set, which is the half
// this test cannot prove on its own: that the real commands set it is
// TestEveryRunE_SilencesUsageFirst's job, and this one would be an empty assertion without it.
func TestExitCodeFor_AFailureInsideACommandKeepsItsCategory(t *testing.T) {
	// SilenceUsage set, exactly as every RunE in this tree sets it before doing anything else.
	ran := &cobra.Command{Use: "ran"}
	ran.SilenceUsage = true

	tests := map[string]struct {
		err  error
		want int
	}{
		"a broken backend":    {err: clicmd.Backend(errors.New("qdrant is unreachable")), want: exitFailure},
		"a gate that failed":  {err: clicmd.Assertion("recall regressed"), want: exitAssertion},
		"a refusal it raised": {err: clicmd.Usage("--tenant is required"), want: exitUsage},
		"an interrupted run":  {err: errInterrupted, want: exitInterrupted},
		"the ingestion lock":  {err: fmt.Errorf("wrapped: %w", lock.ErrHeld), want: exitLockHeld},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := exitCodeFor(ran, tc.err); got != tc.want {
				t.Errorf("exitCodeFor(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// TestExitFor_MapsEveryCategoryToItsOwnCode is the exit-code contract. The categories exist so a
// scheduler can branch, which they only do if no two of them land on the same number.
func TestExitFor_MapsEveryCategoryToItsOwnCode(t *testing.T) {
	codes := map[int]clicmd.Category{}
	for _, category := range []clicmd.Category{
		clicmd.CategoryUsage, clicmd.CategoryBackend, clicmd.CategoryAssertion,
	} {
		code := exitFor(category)
		if code == exitOK {
			t.Errorf("category %q exits 0, which reports a failure as a clean run", category)
		}
		if other, taken := codes[code]; taken {
			t.Errorf("categories %q and %q both exit %d, so nothing downstream can tell them apart",
				other, category, code)
		}
		codes[code] = category
	}

	// The codes this file already owned must stay theirs: a category landing on one of them would
	// make `search` indistinguishable from a run the lock refused or an operator interrupted.
	for _, taken := range []int{exitLockHeld, exitInterrupted} {
		if category, collides := codes[taken]; collides {
			t.Errorf("category %q exits %d, which already means something else in this file",
				category, taken)
		}
	}
}
