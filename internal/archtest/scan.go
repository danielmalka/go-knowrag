// Package archtest holds the module's architecture invariants and the source walk that checks
// them. It has no production callers: it exists so the invariants are verified by `go test` in CI
// rather than by remembering them during review.
package archtest

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Violation is one file that breaks an invariant, located precisely enough to fix without
// searching: a path relative to the walk root, and the line the offending import sits on.
type Violation struct {
	File string
	Line int
}

func (v Violation) String() string { return fmt.Sprintf("%s:%d", v.File, v.Line) }

// ModuleImports maps every package in the module to the module-internal packages it imports.
//
// It exists because FindImporters answers "who imports X directly", and a rule of the form "this
// package must not be able to reach X" is not that question: reaching happens through the chain, and
// the chain is what a hand-written list of directories to walk keeps failing to describe. The graph
// is derived from the source on every run, so there is no list to fall out of date and no line whose
// deletion narrows the check.
//
// Same exclusions as FindImporters and for the same reasons: `_test.go`, `vendor/`, dot-directories
// and `testdata/`. Test files are out because the rule is about what the shipped package can reach —
// a test may legitimately import a searcher to build a fixture. Imports outside this module are out
// because the invariants here are all about this module's own layering.
//
// A package with no Go files of its own simply does not appear, which is what a caller wants: it
// cannot be the source of an edge either.
func ModuleImports(root, modulePath string) (map[string][]string, error) {
	fset := token.NewFileSet()
	graph := map[string][]string{}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (name == "vendor" || name == "testdata" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		rel, relErr := filepath.Rel(root, filepath.Dir(path))
		if relErr != nil {
			return relErr
		}
		pkg := modulePath
		if rel != "." {
			pkg = modulePath + "/" + filepath.ToSlash(rel)
		}

		file, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			return fmt.Errorf("parsing %s: %w", path, perr)
		}
		for _, imp := range file.Imports {
			p, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil || !strings.HasPrefix(p, modulePath+"/") {
				continue
			}
			if !slices.Contains(graph[pkg], p) {
				graph[pkg] = append(graph[pkg], p)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return graph, nil
}

// PathTo returns an import chain from start to target, start first and target last, or nil when
// target is unreachable.
//
// The chain rather than a yes/no, because the answer a failing architecture test has to give is
// "which edge brought this in" — `goldenauthor -> eval -> retrieval` names the one import to remove,
// where "reaches retrieval" sends the reader to build the graph by hand. Breadth-first, so the chain
// printed is a shortest one and two runs over the same tree print the same one (the neighbour lists
// come from a directory walk, which is ordered).
func PathTo(graph map[string][]string, start, target string) []string {
	if start == target {
		return []string{start}
	}
	prev := map[string]string{start: ""}
	queue := []string{start}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range graph[cur] {
			if _, seen := prev[next]; seen {
				continue
			}
			prev[next] = cur
			if next == target {
				var chain []string
				for at := next; at != ""; at = prev[at] {
					chain = append(chain, at)
				}
				slices.Reverse(chain)
				return chain
			}
			queue = append(queue, next)
		}
	}
	return nil
}

// FindImporters returns every non-test Go file under root that imports importPath.
//
// Skipped by construction, not by option: `_test.go` files (a test may legitimately talk to a
// client the package it tests never touches), plus `vendor/`, dot-directories and `testdata/`.
// testdata is skipped because that is where the deliberately-violating fixture lives — a checker
// that found its own fixture during the real-tree run would report a violation nobody can fix, and
// one that had no fixture at all could not be proven non-vacuous. Anything else to exclude is
// named by the caller in skipDirs, as a slash-separated path relative to root.
func FindImporters(root, importPath string, skipDirs []string) ([]Violation, error) {
	fset := token.NewFileSet()
	var found []Violation

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
			if rel == "." {
				return nil
			}
			name := d.Name()
			if name == "vendor" || name == "testdata" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			if slices.Contains(skipDirs, rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		// ImportsOnly: the invariant is about the import graph, so a file whose body does not
		// parse still gets its imports checked, and the walk stays cheap on a growing tree.
		file, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			return fmt.Errorf("parsing %s: %w", rel, perr)
		}
		for _, imp := range file.Imports {
			p, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil || p != importPath {
				continue
			}
			found = append(found, Violation{File: rel, Line: fset.Position(imp.Pos()).Line})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}
