package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The guards that keep the PR 6 cutover true after the next edit. schema.DefaultProfile (BGE-M3) stays
// as the library fallback until the cleanup PR deletes that profile, so an entry point that read it, or
// built an embedder, a manifest or a search config without a profile, would still compile and would
// quietly serve the old model. The entry points are scanned, not the library: the fallback is
// legitimate there and in tests.
//
// They resolve a package by its IMPORT PATH, never by the identifier a file happens to use: an alias
// (`modelSchema ".../internal/schema"`) or a dot-import would walk past a check keyed on the literal
// names `schema` and `embed`. A dot-import of a watched package is refused outright in an entry point,
// because after it a call has no qualifier left to resolve.

const modulePath = "github.com/danielmalka/go-knowrag"

// libraryDefaults are the calls that read the library's fallback profile, by package import path.
var libraryDefaults = map[string][]string{
	modulePath + "/internal/schema": {"DefaultProfile", "Manifest"},
	modulePath + "/internal/embed":  {"Expected", "DefaultDenseDim", "NewServiceEmbedder"},
}

const retrievalPath = modulePath + "/internal/retrieval"

// entryPointRoots are the directories whose non-test Go files are entry points.
var entryPointRoots = []string{"..", "../../internal/clicmd", "../../internal/eval"}

func TestNoEntryPointReadsTheLibraryDefaultProfile(t *testing.T) {
	var hits []string
	for _, root := range entryPointRoots {
		for _, file := range goFiles(t, root) {
			hits = append(hits, libraryDefaultLeaks(parseFile(t, file, nil), file)...)
		}
	}
	if len(hits) > 0 {
		t.Errorf("entry points must resolve their profile (schema.ResolveProfile / cfg.Profile) and pass it "+
			"explicitly; these read the library fallback:\n  %s", strings.Join(hits, "\n  "))
	}
}

func TestEveryEntryPointSearchConfigNamesItsProfile(t *testing.T) {
	var hits []string
	sites := 0
	for _, root := range entryPointRoots {
		for _, file := range goFiles(t, root) {
			n, leaks := searchConfigLeaks(parseFile(t, file, nil), file)
			sites += n
			hits = append(hits, leaks...)
		}
	}
	if len(hits) > 0 {
		t.Errorf("every search config an entry point builds must have its Profile set:\n  %s", strings.Join(hits, "\n  "))
	}
	// Non-vacuity: the scan has to actually find the searchers this repository builds (cmd/cli search,
	// the MCP server, measure-search). A guard that matched nothing would stay green after a rename.
	if sites < 3 {
		t.Errorf("the search-config guard found %d site(s), expected at least 3: it is no longer looking at the code", sites)
	}
}

// --- the guards, over parsed files so a test can feed them any source ------------------------------

func TestGuards_ResolveByImportPathNotByIdentifier(t *testing.T) {
	cases := []struct {
		name, src string
		want      int // leaks libraryDefaultLeaks must report
	}{
		{"plain selector", `package p
import "github.com/danielmalka/go-knowrag/internal/schema"
var _ = schema.DefaultProfile`, 1},
		{"aliased import", `package p
import modelSchema "github.com/danielmalka/go-knowrag/internal/schema"
var _ = modelSchema.DefaultProfile`, 1},
		{"dot import is refused", `package p
import . "github.com/danielmalka/go-knowrag/internal/schema"
var _ = DefaultProfile`, 1},
		{"aliased embed", `package p
import e "github.com/danielmalka/go-knowrag/internal/embed"
var _ = e.NewServiceEmbedder`, 1},
		{"dot import of embed", `package p
import . "github.com/danielmalka/go-knowrag/internal/embed"
var _ = Expected`, 1},
		{"profile-aware calls are fine", `package p
import m "github.com/danielmalka/go-knowrag/internal/schema"
import "github.com/danielmalka/go-knowrag/internal/embed"
var _ = m.ResolveProfile
var _ = m.ManifestFor
var _ = embed.NewServiceEmbedderFor`, 0},
		{"a local named schema that is not the package is not flagged", `package p
type s struct{ DefaultProfile int }
var schema s
var _ = schema.DefaultProfile`, 0},
		{"a different package aliased as schema is not flagged", `package p
import schema "example.com/other/schema"
var _ = schema.DefaultProfile`, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := libraryDefaultLeaks(parseFile(t, "x.go", []byte(c.src)), "x.go"); len(got) != c.want {
				t.Errorf("libraryDefaultLeaks = %v, want %d leak(s)", got, c.want)
			}
		})
	}
}

func TestGuards_SearchConfigMustSetItsProfile(t *testing.T) {
	const head = `package p
import r "github.com/danielmalka/go-knowrag/internal/retrieval"
`
	cases := []struct {
		name, src string
		wantSites int
		wantLeaks int
	}{
		{"assigned then Profile set", head + `func f() { cfg := r.DefaultConfig(); cfg.Profile = nil; _ = cfg }`, 1, 0},
		{"Profile set through a different field is a leak", head + `func f() { cfg := r.DefaultConfig(); cfg.MaxChunksPerUID = 2; _ = cfg }`, 1, 1},
		{"the substring in a comment does not count", head + `func f() { cfg := r.DefaultConfig(); /* cfg.Profile = x */ _ = cfg }`, 1, 1},
		{"Profile set on another variable is a leak", head + `func f() { cfg, other := r.DefaultConfig(), r.DefaultConfig(); other.Profile = nil; _, _ = cfg, other }`, 2, 1},
		{"used inline", head + `func f() { _ = g(r.DefaultConfig()) }; func g(any) int { return 0 }`, 1, 1},
		{"returned directly", head + `func f() any { return r.DefaultConfig() }`, 1, 1},
		{"composite literal without Profile", head + `func f() any { return r.Config{PrefetchMultiplier: 1} }`, 1, 1},
		{"empty composite literal", head + `func f() any { return r.Config{} }`, 1, 1},
		{"composite literal with Profile", head + `func f() any { return r.Config{Profile: nil} }`, 1, 0},
		{"zero value variable left alone", head + `func f() any { var cfg r.Config; return cfg }`, 1, 1},
		{"zero value variable with Profile", head + `func f() any { var cfg r.Config; cfg.Profile = nil; return cfg }`, 1, 0},
		{"package-level default", head + `var cfg = r.DefaultConfig()`, 1, 1},
		{"dot import is refused", `package p
import . "github.com/danielmalka/go-knowrag/internal/retrieval"
func f() any { return DefaultConfig() }`, 0, 1},
		{"unrelated package with the same alias", `package p
import r "example.com/other/retrieval"
func f() any { return r.DefaultConfig() }`, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sites, leaks := searchConfigLeaks(parseFile(t, "x.go", []byte(c.src)), "x.go")
			if sites != c.wantSites || len(leaks) != c.wantLeaks {
				t.Errorf("sites=%d leaks=%v, want sites=%d and %d leak(s)", sites, leaks, c.wantSites, c.wantLeaks)
			}
		})
	}
}

// --- implementation -------------------------------------------------------------------------------

func parseFile(t *testing.T, name string, src []byte) *ast.File {
	t.Helper()
	var source any // nil means "read the file", which a nil []byte stored in an interface would not
	if src != nil {
		source = src
	}
	f, err := parser.ParseFile(token.NewFileSet(), name, source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %s: %v", name, err)
	}
	return f
}

// importNames returns the local names a file gives the package at path, and whether it dot-imports it.
// A blank import binds nothing, so it is neither.
func importNames(f *ast.File, path string) (names []string, dot bool) {
	for _, spec := range f.Imports {
		got, err := strconv.Unquote(spec.Path.Value)
		if err != nil || got != path {
			continue
		}
		switch {
		case spec.Name == nil:
			names = append(names, path[strings.LastIndex(path, "/")+1:])
		case spec.Name.Name == ".":
			dot = true
		case spec.Name.Name != "_":
			names = append(names, spec.Name.Name)
		}
	}
	return names, dot
}

// qualified reports whether expr is `<pkgName>.<one of sels>` for one of the local package names.
func qualified(expr ast.Expr, pkgNames []string, sels ...string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && slices.Contains(pkgNames, id.Name) && (len(sels) == 0 || slices.Contains(sels, sel.Sel.Name))
}

func libraryDefaultLeaks(f *ast.File, file string) []string {
	var hits []string
	for path, sels := range libraryDefaults {
		names, dot := importNames(f, path)
		if dot {
			hits = append(hits, file+": dot-import of "+path+" (an entry point must qualify what it uses)")
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if qualified(asExpr(n), names, sels...) {
				hits = append(hits, file+": "+path[strings.LastIndex(path, "/")+1:]+"."+n.(*ast.SelectorExpr).Sel.Name)
			}
			return true
		})
	}
	slices.Sort(hits)
	return hits
}

func asExpr(n ast.Node) ast.Expr {
	e, _ := n.(ast.Expr)
	return e
}

// searchConfigLeaks finds every search config the file builds (a retrieval.DefaultConfig() call, a
// retrieval.Config literal or a retrieval.Config variable) and reports the ones whose Profile field is
// never set. A config that is not stored in a variable cannot have its field set, so it is a leak too.
// sites counts what it looked at, so a test can prove the scan is not vacuous.
func searchConfigLeaks(f *ast.File, file string) (sites int, leaks []string) {
	names, dot := importNames(f, retrievalPath)
	if dot {
		leaks = append(leaks, file+": dot-import of "+retrievalPath+" (an entry point must qualify what it uses)")
	}
	if len(names) == 0 {
		return 0, leaks
	}
	scan := func(body ast.Node, what string) {
		s, l := scopeConfigLeaks(body, names, file, what)
		sites += s
		leaks = append(leaks, l...)
	}
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
			scan(fn.Body, fn.Name.Name)
			continue
		}
		scan(decl, "package level")
	}
	return sites, leaks
}

// scopeConfigLeaks checks one function body (or one package-level declaration).
func scopeConfigLeaks(body ast.Node, names []string, file, where string) (sites int, leaks []string) {
	vars := map[string]bool{}     // config variables declared here
	setters := map[string]bool{}  // variables whose Profile field is assigned here
	stored := map[ast.Expr]bool{} // config-producing expressions that landed in a variable
	isConfigExpr := func(e ast.Expr) bool {
		switch x := e.(type) {
		case *ast.CallExpr:
			return qualified(x.Fun, names, "DefaultConfig")
		case *ast.CompositeLit:
			return qualified(x.Type, names, "Config")
		}
		return false
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range x.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "Profile" {
					if id, ok := sel.X.(*ast.Ident); ok {
						setters[id.Name] = true
					}
				}
				if id, ok := lhs.(*ast.Ident); ok && i < len(x.Rhs) && len(x.Lhs) == len(x.Rhs) && isConfigExpr(x.Rhs[i]) {
					stored[x.Rhs[i]] = true
					vars[id.Name] = !literalSetsProfile(x.Rhs[i])
				}
			}
		case *ast.ValueSpec:
			zeroConfig := x.Type != nil && qualified(x.Type, names, "Config") && len(x.Values) == 0
			for i, id := range x.Names {
				switch {
				case zeroConfig:
					vars[id.Name] = true
					sites++
				case i < len(x.Values) && isConfigExpr(x.Values[i]):
					stored[x.Values[i]] = true
					vars[id.Name] = !literalSetsProfile(x.Values[i])
				}
			}
		}
		return true
	})
	ast.Inspect(body, func(n ast.Node) bool {
		e := asExpr(n)
		if e == nil || !isConfigExpr(e) {
			return true
		}
		sites++
		if literalSetsProfile(e) {
			return true
		}
		if !stored[e] {
			leaks = append(leaks, file+": "+where+": a search config is built without a Profile and not kept in a variable that could set one")
		}
		return true
	})
	for _, name := range slices.Sorted(maps.Keys(vars)) {
		if !setters[name] {
			leaks = append(leaks, file+": "+where+": search config variable "+name+" never has its Profile set")
		}
	}
	return sites, dedupe(leaks)
}

// literalSetsProfile is true for a retrieval.Config literal with a `Profile:` key.
func literalSetsProfile(e ast.Expr) bool {
	lit, ok := e.(*ast.CompositeLit)
	if !ok {
		return false
	}
	for _, el := range lit.Elts {
		if kv, ok := el.(*ast.KeyValueExpr); ok {
			if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Profile" {
				return true
			}
		}
	}
	return false
}

func dedupe(in []string) []string {
	slices.Sort(in)
	return slices.Compact(in)
}
