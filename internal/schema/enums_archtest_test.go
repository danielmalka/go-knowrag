package schema

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TestNoConcurrentEnumDeclaration guards T7's single-declaration-site property: it is not enough
// that internal/schema is the only package that can *construct* a valid NoteType/Status/Visibility/
// Vault/Area (the unexported field already guarantees that) — a different package could still
// declare its own, unrelated const/var block that happens to spell out the same string values
// (e.g. a local `const StatusDraft = "draft"`), which would silently reintroduce the parallel list
// T7 exists to prevent. This test scans every other package in the module for that pattern.
//
// Threshold heuristic and its known ceiling: a file "declares" one of the six canonical sets below
// if it contains string literals, as values in top-level const/var declarations, matching at least
// half that set's members (2*matches >= len(set)). Half is a deliberate slack, not a tight bound: it
// catches a file that redeclares most-but-not-all of an enum (e.g. someone who copies four of
// Status's four values but renames one) without firing on a file that merely happens to use one or
// two of the same words for unrelated reasons. It does NOT catch every shadow enum: a set built from
// computed strings (concatenation, fmt.Sprintf, a loop) never appears as a literal AST node here, and
// a set packed into a single slice or map literal is not a top-level const/var VALUE in the sense
// this walker looks for (ValueSpec, not slice/map elements) — both evade detection. Closing those
// gaps needs a semantic check (e.g. computing string values or tracking composite literals), which is
// a bigger dependency-free task than this one is scoped to take on.
//
// The Vault set has only 2 members, so 2*matches >= 2 means a single matching literal is enough to
// flag a file. That is deliberate rather than a rounding accident: "malkalife"/"malkaway" are
// distinctive enough that one accidental match in unrelated code is not a realistic risk, so the
// stricter threshold buys real detection instead of false positives.
//
// Scope: every .go file in the module except internal/schema itself (already excluded by directory)
// and anything under vendor/ or a dot-directory. Other packages' _test.go files ARE scanned — a
// shadow enum hiding in test fixtures is still a shadow enum, and excluding _test.go files globally
// would make the fixture-based RED/GREEN proof below impossible to demonstrate honestly.
func TestNoConcurrentEnumDeclaration(t *testing.T) {
	sets := enumSets()

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}

	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == "vendor" || (name != "." && len(name) > 0 && name[0] == '.') {
				return filepath.SkipDir
			}
			// Skip internal/schema itself: it is the one package allowed to declare these values.
			if path != root {
				rel, relErr := filepath.Rel(root, path)
				if relErr == nil && rel == filepath.Join("internal", "schema") {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}

		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			// A file that doesn't parse isn't a Go source file this test can reason about; other
			// tests (build, vet) already catch that failure mode.
			return nil
		}

		literals := topLevelStringLiterals(file)
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		for _, es := range sets {
			matches := matchCount(literals, es.values)
			if 2*matches >= len(es.values) {
				t.Errorf(
					"file %s declares %d/%d values of enum %s as its own const/var literals — "+
						"import schema.%s* instead of redeclaring the value set",
					rel, matches, len(es.values), es.name, es.name,
				)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk module: %v", err)
	}
}

type enumSet struct {
	name   string
	values []string
}

// enumSets builds the six canonical sets to check against from internal/schema's own registered
// maps (the same maps Valid()/Parse* read), not from a retyped literal list — retyping the values
// here would make this test a second declaration site, exactly what T7/T8 exist to prevent.
func enumSets() []enumSet {
	return []enumSet{
		{"NoteType", keysOf(noteTypeSet)},
		{"Status", keysOf(statusSet)},
		{"Visibility", keysOf(visibilitySet)},
		{"Vault", keysOf(vaultSet)},
		{"Area(MalkaLife)", keysOf(areaSetByVault[VaultMalkaLife])},
		{"Area(MalkaWay)", keysOf(areaSetByVault[VaultMalkaWay])},
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// topLevelStringLiterals collects every string literal that appears as a value in a top-level
// const or var declaration in file.
func topLevelStringLiterals(file *ast.File) []string {
	var out []string
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || (gd.Tok != token.CONST && gd.Tok != token.VAR) {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, val := range vs.Values {
				lit, ok := val.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					continue
				}
				out = append(out, s)
			}
		}
	}
	return out
}

func matchCount(literals, values []string) int {
	set := make(map[string]struct{}, len(literals))
	for _, l := range literals {
		set[l] = struct{}{}
	}
	count := 0
	for _, v := range values {
		if _, ok := set[v]; ok {
			count++
		}
	}
	return count
}
