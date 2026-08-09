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
