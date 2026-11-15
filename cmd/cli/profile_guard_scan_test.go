package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The scanners behind the profile guards (profile_guard_test.go). They identify things by OBJECT, not
// by name: go/types binds every identifier to the declaration it refers to, so
//
//   - a package is the one at an import PATH, whatever alias the file gives it, and
//   - a config variable and the `x.Profile = …` that sets it are the same variable only when
//     types.Info says they are, which is what makes an inner variable that shadows an outer one
//     (`cfg := …` in a nested block) a different variable instead of the same map key.
//
// The type-check is deliberately shallow. Every import resolves to an empty stand-in package, so no
// dependency is compiled or even found, and the checker reports "undefined" for each member it cannot
// see; those errors are ignored. What survives is exactly what these guards need: which declaration
// each identifier means (Defs, Uses) and which import each package qualifier is (PkgName). The
// alternative, golang.org/x/tools/go/packages, is only a hash in go.sum here, not a dependency, and
// pulling it in for a test would add a module to a public repository.

// checkedFile is a parsed file with the object bindings go/types found for it.
type checkedFile struct {
	name string
	file *ast.File
	info *types.Info
}

func checkFile(t *testing.T, name string, src []byte) checkedFile {
	t.Helper()
	fset := token.NewFileSet()
	var source any // nil means "read the file", which a nil []byte stored in an interface would not
	if src != nil {
		source = src
	}
	f, err := parser.ParseFile(fset, name, source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %s: %v", name, err)
	}
	info := &types.Info{Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}}
	conf := types.Config{
		Importer:                 standInImporter{},
		Error:                    func(error) {}, // members of stand-in packages are undefined, by design
		DisableUnusedImportCheck: true,
	}
	_, _ = conf.Check("p", fset, []*ast.File{f}, info)
	return checkedFile{name: name, file: f, info: info}
}

// standInImporter answers every import with an empty, complete package named after the last path
// element. That is enough for the checker to create a PkgName for each import.
type standInImporter struct{}

func (standInImporter) Import(path string) (*types.Package, error) {
	pkg := types.NewPackage(path, path[strings.LastIndex(path, "/")+1:])
	pkg.MarkComplete()
	return pkg, nil
}

// dotImports reports whether the file dot-imports the package at path. A dot-import leaves a call with
// no qualifier to resolve, so an entry point is not allowed one for a watched package.
func (c checkedFile) dotImports(path string) bool {
	for _, spec := range c.file.Imports {
		if got, err := strconv.Unquote(spec.Path.Value); err == nil && got == path && spec.Name != nil && spec.Name.Name == "." {
			return true
		}
	}
	return false
}

// pkgOf is the import path the identifier qualifies, or "" when it is not a package name (a local
// variable called `schema`, a field, anything else).
func (c checkedFile) pkgOf(id *ast.Ident) string {
	if pn, ok := c.info.Uses[id].(*types.PkgName); ok {
		return pn.Imported().Path()
	}
	return ""
}

// isPkgMember reports whether expr is `<qualifier>.<one of sels>` with the qualifier bound to the
// package at path. With no sels, any member matches.
func (c checkedFile) isPkgMember(expr ast.Expr, path string, sels ...string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && c.pkgOf(id) == path && (len(sels) == 0 || slices.Contains(sels, sel.Sel.Name))
}

// objOf is the variable an identifier declares or refers to.
func (c checkedFile) objOf(id *ast.Ident) types.Object {
	if obj := c.info.Defs[id]; obj != nil {
		return obj
	}
	return c.info.Uses[id]
}

func libraryDefaultLeaks(c checkedFile) []string {
	var hits []string
	for path, sels := range libraryDefaults {
		if c.dotImports(path) {
			hits = append(hits, c.name+": dot-import of "+path+" (an entry point must qualify what it uses)")
		}
		ast.Inspect(c.file, func(n ast.Node) bool {
			if e, ok := n.(ast.Expr); ok && c.isPkgMember(e, path, sels...) {
				hits = append(hits, c.name+": "+path[strings.LastIndex(path, "/")+1:]+"."+e.(*ast.SelectorExpr).Sel.Name)
			}
			return true
		})
	}
	slices.Sort(hits)
	return hits
}

// searchConfigLeaks finds every search config the file builds (a retrieval.DefaultConfig() call, a
// retrieval.Config literal or a retrieval.Config variable) and reports the ones whose Profile field is
// never set. A config that is not stored in a variable cannot have its field set, so it is a leak too.
// sites counts what it looked at, so a test can prove the scan is not vacuous.
//
// Variables are objects: `cfg.Profile = …` sets the variable its `cfg` is bound to, and only that one.
func searchConfigLeaks(c checkedFile) (sites int, leaks []string) {
	if c.dotImports(retrievalPath) {
		leaks = append(leaks, c.name+": dot-import of "+retrievalPath+" (an entry point must qualify what it uses)")
	}
	configs := map[types.Object]string{} // variable -> where it was declared (function name)
	setters := map[types.Object]bool{}   // variables whose Profile field is assigned
	stored := map[ast.Expr]bool{}        // config expressions that landed in a variable

	for _, decl := range c.file.Decls {
		where := "package level"
		if fn, ok := decl.(*ast.FuncDecl); ok {
			where = fn.Name.Name
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.AssignStmt:
				c.collectAssign(x, where, configs, setters, stored)
			case *ast.ValueSpec:
				sites += c.collectValueSpec(x, where, configs, stored)
			}
			return true
		})
		ast.Inspect(decl, func(n ast.Node) bool {
			if e, ok := n.(ast.Expr); ok && c.isConfigExpr(e) {
				sites++
				if !c.literalSetsProfile(e) && !stored[e] {
					leaks = append(leaks, c.name+": "+where+": a search config is built without a Profile and not kept in a variable that could set one")
				}
			}
			return true
		})
	}
	for _, obj := range slices.SortedFunc(maps.Keys(configs), func(a, b types.Object) int { return strings.Compare(a.Name()+configs[a], b.Name()+configs[b]) }) {
		if !setters[obj] {
			leaks = append(leaks, c.name+": "+configs[obj]+": search config variable "+obj.Name()+" never has its Profile set")
		}
	}
	slices.Sort(leaks)
	return sites, slices.Compact(leaks)
}

func (c checkedFile) isConfigExpr(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.CallExpr:
		return c.isPkgMember(x.Fun, retrievalPath, "DefaultConfig")
	case *ast.CompositeLit:
		return c.isPkgMember(x.Type, retrievalPath, "Config")
	}
	return false
}

// literalSetsProfile is true for a retrieval.Config literal with a `Profile:` key.
func (c checkedFile) literalSetsProfile(e ast.Expr) bool {
	lit, ok := e.(*ast.CompositeLit)
	if !ok || !c.isConfigExpr(lit) {
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

func (c checkedFile) collectAssign(x *ast.AssignStmt, where string, configs map[types.Object]string, setters map[types.Object]bool, stored map[ast.Expr]bool) {
	for i, lhs := range x.Lhs {
		if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "Profile" {
			if id, ok := sel.X.(*ast.Ident); ok {
				if obj := c.objOf(id); obj != nil {
					setters[obj] = true
				}
			}
		}
		id, ok := lhs.(*ast.Ident)
		if !ok || len(x.Lhs) != len(x.Rhs) || !c.isConfigExpr(x.Rhs[i]) {
			continue
		}
		stored[x.Rhs[i]] = true
		if obj := c.objOf(id); obj != nil && !c.literalSetsProfile(x.Rhs[i]) {
			configs[obj] = where
		}
	}
}

// collectValueSpec records `var x = <config>` and `var x retrieval.Config`, and returns how many
// zero-value declarations it saw (they are config sites with no expression to count).
func (c checkedFile) collectValueSpec(x *ast.ValueSpec, where string, configs map[types.Object]string, stored map[ast.Expr]bool) (sites int) {
	zero := x.Type != nil && c.isPkgMember(x.Type, retrievalPath, "Config") && len(x.Values) == 0
	for i, id := range x.Names {
		obj := c.info.Defs[id]
		switch {
		case obj == nil:
		case zero:
			configs[obj] = where
			sites++
		case i < len(x.Values) && c.isConfigExpr(x.Values[i]):
			stored[x.Values[i]] = true
			if !c.literalSetsProfile(x.Values[i]) {
				configs[obj] = where
			}
		}
	}
	return sites
}
