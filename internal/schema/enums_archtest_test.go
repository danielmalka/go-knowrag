package schema

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

// TestNoEnumLiteralsDeclaredOutsideSchema guards T7's single-declaration-site property against the
// one hole the type system leaves open: it is not enough that internal/schema is the only package
// that can *construct* a valid NoteType/Status/Visibility/Vault/Area (the unexported field already
// guarantees that, and since the canonical values are accessor functions rather than exported vars,
// nothing outside can reassign them either) — a different package could still declare its own,
// unrelated const/var block spelling out the same string values (e.g. a local
// `const StatusDraft = "draft"`, or `var statuses = []string{"draft", …}`), silently reintroducing
// the parallel list T7 exists to prevent. This test scans every other package in the module for
// that pattern.
//
// The name says what is actually proven, no more: *declared literals*, *outside schema*. It is a
// heuristic with a real ceiling, spelled out below, not a proof that no shadow enum can exist.
//
// What it looks at: string constants reachable from the values of a top-level const or var
// declaration — direct literals, literals nested at any depth in a composite literal
// (`[]string{…}`, `map[string]T{…}`, struct literals, including map keys), and syntactic constant
// concatenation (`"dra" + "ft"`). Matches are aggregated **per package**, not per file, so
// splitting an enum across two files in the same package does not dilute it under the threshold.
//
// Threshold: a package "declares" one of the three canonical sets if its declared literals match at
// least half that set's members (2*matches >= len(set)). Half is deliberate slack, not a tight
// bound: it catches a package that redeclares most-but-not-all of an enum without firing on one
// that happens to use one or two of the same words for unrelated reasons.
//
// Known ceiling, none of which this test detects:
//   - Literals inside function bodies. Scanning them would fire on every legitimate test fixture
//     table, so the scan stops at top-level declarations by design.
//   - Values computed at run time: fmt.Sprintf, strings.Join, a loop, or a named constant reached
//     by identifier rather than written inline. Folding those needs go/types-level constant
//     evaluation; the fold here is syntactic.
//   - A set deliberately spread below the half threshold, or across two packages.
//   - Declaration sites that are not Go source at all (YAML, JSON, SQL fixtures).
//
// The worst case moved with D-26: the Vault set (2 members) is gone, and it was the only set where
// 2*matches >= len(set) let a single matching literal flag a package. Visibility is now the smallest
// survivor at 3 members, so 2*matches >= 3 needs matches >= 2 — a package must repeat at least two of
// "private"/"internal"/"shareable" (or two of Status's four values) as its own literals before this
// fires. That is a stricter bar than the one Vault used to clear, not a looser one: catching a
// two-word coincidence in unrelated code is still implausible, so the half-of-set rule keeps its
// job — real detection — without the single-literal trigger that only the smallest set ever needed.
//
// Scope: every .go file in the module except internal/schema itself (already excluded by
// directory) and anything under vendor/ or a dot-directory. Other packages' _test.go files ARE
// scanned — a shadow enum hiding in test fixtures is still a shadow enum, and excluding _test.go
// files globally would make the fixture-based RED/GREEN proof impossible to demonstrate honestly.
func TestNoEnumLiteralsDeclaredOutsideSchema(t *testing.T) {
	sets := enumSets()

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}

	// Literals accumulate per directory (= per package) before the threshold is applied, so the
	// same enum split 4-and-4 across two files still trips the check.
	byPackage := map[string][]string{}

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

		dir, relErr := filepath.Rel(root, filepath.Dir(path))
		if relErr != nil {
			dir = filepath.Dir(path)
		}
		byPackage[dir] = append(byPackage[dir], declaredStringLiterals(file)...)
		return nil
	})
	if err != nil {
		t.Fatalf("walk module: %v", err)
	}

	for _, dir := range slices.Sorted(maps.Keys(byPackage)) {
		for _, es := range sets {
			matches := matchCount(byPackage[dir], es.values)
			if 2*matches >= len(es.values) {
				t.Errorf(
					"package %s declares %d/%d values of enum %s as its own const/var literals — "+
						"call schema.%s*() instead of redeclaring the value set",
					dir, matches, len(es.values), es.name, es.name,
				)
			}
		}
	}
}

type enumSet struct {
	name   string
	values []string
}

// enumSets builds the three canonical sets to check against from internal/schema's own registered
// maps (the same maps Valid()/Parse* read), not from a retyped literal list — retyping the values
// here would make this test a second declaration site, exactly what T7/T8 exist to prevent.
//
// `Vault` and `Area` used to be here too. D-26 moved them out of internal/schema entirely — a vault
// roster is installation configuration now (internal/config), not a compile-time enum — so there is
// no vault/area set left for this scan to protect.
func enumSets() []enumSet {
	return []enumSet{
		{"NoteType", keysOf(noteTypeSet)},
		{"Status", keysOf(statusSet)},
		{"Visibility", keysOf(visibilitySet)},
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// declaredStringLiterals collects every string constant reachable from the values of a top-level
// const or var declaration in file.
func declaredStringLiterals(file *ast.File) []string {
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
				collectStrings(val, &out)
			}
		}
	}
	return out
}

// collectStrings appends every string constant e yields, descending into composite literals so a
// value set packed into `[]string{…}` or a `map[string]T{…}` is seen the same as one written out as
// individual consts. Map and struct keys are collected too: `map[string]int{"draft": 1}` hides the
// value set in the keys just as effectively.
func collectStrings(e ast.Expr, out *[]string) {
	if s, ok := constString(e); ok {
		*out = append(*out, s)
		return
	}
	cl, ok := e.(*ast.CompositeLit)
	if !ok {
		return
	}
	for _, elt := range cl.Elts {
		if kv, ok := elt.(*ast.KeyValueExpr); ok {
			collectStrings(kv.Key, out)
			collectStrings(kv.Value, out)
			continue
		}
		collectStrings(elt, out)
	}
}

// constString folds e to a string if it is a string literal or a `+` chain of them. The fold is
// syntactic, not go/types constant evaluation: `"dra" + "ft"` folds, a named constant referenced by
// identifier does not — see the ceiling documented on the test above.
func constString(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		return s, err == nil
	case *ast.ParenExpr:
		return constString(v.X)
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		l, lok := constString(v.X)
		r, rok := constString(v.Y)
		if !lok || !rok {
			return "", false
		}
		return l + r, true
	}
	return "", false
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
