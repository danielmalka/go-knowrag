package main

import (
	"bytes"
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestServer_StdioHandshakeListAndCall_OverPipes drives the server the way Hermes will: real
// newline-delimited JSON-RPC over a reader and a writer, with the handshake, tools/list and
// tools/call all crossing the wire.
//
// mcp.IOTransport is byte-for-byte what mcp.StdioTransport does — StdioTransport is that transport
// with os.Stdin and os.Stdout wired in — so this exercises the production path without the test
// having to own the process's real file descriptors.
//
// It also captures everything the server wrote and asserts every line is a JSON-RPC frame. That is
// the stdio invariant stated as an assertion rather than as a comment: one stray write on this
// stream and the client blocks forever on a message it cannot parse.
func TestServer_StdioHandshakeListAndCall_OverPipes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clientToServer := newPipe()
	serverToClient := newPipe()
	captured := &lockedBuffer{}

	serverSession, err := newServer(testConfig(), &fakeSearcher{results: sampleResults()}).Connect(ctx,
		&mcp.IOTransport{
			Reader: clientToServer.r,
			Writer: writeCloser{Writer: io.MultiWriter(serverToClient.w, captured), Closer: serverToClient.w},
		}, nil)
	if err != nil {
		t.Fatalf("connecting the server over pipes: %v", err)
	}
	defer func() { _ = serverSession.Close() }()

	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "hermes-stand-in", Version: "0"}, nil).
		Connect(ctx, &mcp.IOTransport{Reader: serverToClient.r, Writer: clientToServer.w}, nil)
	if err != nil {
		t.Fatalf("handshake over pipes failed: %v", err)
	}
	defer func() { _ = clientSession.Close() }()

	list, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list over pipes: %v", err)
	}
	names := make([]string, 0, len(list.Tools))
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
	}
	if len(list.Tools) != 2 {
		t.Fatalf("tools/list returned %v, want %s and %s", names, toolName, getNoteToolName)
	}
	for _, want := range []string{toolName, getNoteToolName} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("tools/list returned %v, missing %s", names, want)
		}
	}

	res, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      toolName,
		Arguments: json.RawMessage(`{"query":"ingress"}`),
	})
	if err != nil {
		t.Fatalf("tools/call over pipes: %v", err)
	}
	if res.IsError {
		t.Fatalf("tools/call returned an error result: %s", resultText(t, res))
	}
	if !strings.Contains(resultText(t, res), "Kubernetes ingress notes.") {
		t.Errorf("the call did not return the chunk: %s", resultText(t, res))
	}

	note, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      getNoteToolName,
		Arguments: json.RawMessage(`{"uid":"` + sampleNoteUID + `"}`),
	})
	if err != nil {
		t.Fatalf("get_note over pipes: %v", err)
	}
	if note.IsError {
		t.Fatalf("get_note returned an error result: %s", resultText(t, note))
	}
	if !strings.Contains(resultText(t, note), "Kubernetes ingress notes.") {
		t.Errorf("get_note did not return the chunk: %s", resultText(t, note))
	}

	assertOnlyJSONRPC(t, captured.String())
}

// assertOnlyJSONRPC fails if anything on the server's output stream is not a JSON-RPC frame.
func assertOnlyJSONRPC(t *testing.T, out string) {
	t.Helper()
	if strings.TrimSpace(out) == "" {
		t.Fatal("the server wrote nothing — this test is asserting against an empty stream")
	}
	for i, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var frame struct {
			JSONRPC string `json:"jsonrpc"`
		}
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("line %d of the server's stdout stream is not JSON, which corrupts the protocol: %q", i, line)
		}
		if frame.JSONRPC != "2.0" {
			t.Fatalf("line %d is JSON but not a JSON-RPC frame: %q", i, line)
		}
	}
}

// TestNoPackageInThisBinaryWritesToStdout is the same invariant checked statically, and it is the
// one that catches the realistic mistake: a debug print added to any package this binary links,
// not just to this one. Hunting that down at run time means watching a client hang with no error
// anywhere, so it is worth catching at `go test` time instead.
//
// It walks the module-local packages that cmd/mcp-server actually depends on, so it neither misses
// internal/retrieval nor complains about cmd/cli, which writes to stdout by design.
func TestNoPackageInThisBinaryWritesToStdout(t *testing.T) {
	dirs := moduleLocalDeps(t)
	if len(dirs) < 3 {
		t.Fatalf("resolved only %d module-local dependencies — this test is looking at almost nothing", len(dirs))
	}

	var scanned int
	for _, dir := range dirs {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatalf("globbing %s: %v", dir, err)
		}
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			scanned++
			for _, offence := range stdoutWrites(t, file) {
				t.Errorf("%s: %s — stdout carries the MCP protocol; write diagnostics to stderr "+
					"(slog) instead, or the client hangs on a frame it cannot parse", file, offence)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no files — the walk is wrong, so this test proves nothing")
	}
}

// stdoutWrites reports every expression in file that writes to the process's stdout. It reads the
// AST rather than the text so a comment mentioning fmt.Printf is not a finding.
func stdoutWrites(t *testing.T, file string) []string {
	t.Helper()

	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}

	var found []string
	ast.Inspect(parsed, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.SelectorExpr:
			if pkg, ok := node.X.(*ast.Ident); ok && pkg.Name == "os" && node.Sel.Name == "Stdout" {
				found = append(found, "references os.Stdout")
			}
		case *ast.CallExpr:
			switch fun := node.Fun.(type) {
			case *ast.Ident:
				// The builtins print/println go to stderr, but they are debug leftovers either
				// way and there is no reason for one to survive review in this binary.
				if fun.Name == "print" || fun.Name == "println" {
					found = append(found, "calls the builtin "+fun.Name)
				}
			case *ast.SelectorExpr:
				pkg, ok := fun.X.(*ast.Ident)
				if ok && pkg.Name == "fmt" && strings.HasPrefix(fun.Sel.Name, "Print") {
					found = append(found, "calls fmt."+fun.Sel.Name+", which writes to stdout")
				}
			}
		}
		return true
	})
	return found
}

// moduleLocalDeps returns the source directories of every package in this module that
// cmd/mcp-server links, including itself.
func moduleLocalDeps(t *testing.T) []string {
	t.Helper()

	const modulePath = "github.com/danielmalka/go-knowrag"
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}\t{{.Dir}}", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}

	var dirs []string
	var sawSelf, sawRetrieval bool
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		importPath, dir, ok := strings.Cut(line, "\t")
		if !ok || !strings.HasPrefix(importPath, modulePath) {
			continue
		}
		sawSelf = sawSelf || strings.HasSuffix(importPath, "cmd/mcp-server")
		sawRetrieval = sawRetrieval || strings.HasSuffix(importPath, "internal/retrieval")
		dirs = append(dirs, dir)
	}
	if !sawSelf || !sawRetrieval {
		t.Fatalf("the dependency walk missed cmd/mcp-server (%v) or internal/retrieval (%v) — "+
			"it is not looking at the code it claims to check", sawSelf, sawRetrieval)
	}
	return dirs
}

// TestStdoutCheckerIsNotVacuous proves the scanner above can fail, which a green architecture-style
// test is most likely to be lying about.
func TestStdoutCheckerIsNotVacuous(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "violation.go")
	const src = `package p

import (
	"fmt"
	"os"
)

// fmt.Println in a comment must not count.
func f() {
	fmt.Println("debug")
	fmt.Fprintln(os.Stdout, "debug")
}
`
	if err := os.WriteFile(file, []byte(src), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}
	if got := stdoutWrites(t, file); len(got) < 2 {
		t.Fatalf("the scanner found %v in a file that prints to stdout twice", got)
	}
}

// pipe is one direction of the connection.
type pipe struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func newPipe() pipe {
	r, w := io.Pipe()
	return pipe{r: r, w: w}
}

// writeCloser lets the tee'd MultiWriter still close the underlying pipe.
type writeCloser struct {
	io.Writer
	io.Closer
}

// lockedBuffer collects the server's output stream from the transport's goroutine while the test
// reads it from its own.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}
