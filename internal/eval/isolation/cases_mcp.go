package isolation

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"slices"
	"strings"
)

// mcpServerDir is the package whose source this case reads, relative to the module root. It is the
// long-lived process an MCP client talks to, and the only entry point in this build whose scope is
// fixed by the operator rather than named by the caller (cmd/mcp-server/config.go reads it from the
// environment at startup).
const mcpServerDir = "cmd/mcp-server"

// scopeFields are the two fields of retrieval.Query that decide what a search may reach. Spelled
// here as strings because this case reads source rather than values; internal/retrieval owns the
// fields themselves.
var scopeFields = []string{"Collection", "TenantID"}

// MCPScopeBindingCase is a tripwire on the shape of cmd/mcp-server, and it is not a proof. The
// difference is the whole reason to read this comment before trusting a green run.
//
// # What it is for
//
// cmd/mcp-server binds its collection and tenant at startup from the environment, and no tool input
// reaches either. That is true today and it is checked dynamically where it is checkable:
// TestToolsList_SearchKnowledge_SchemaHasNoTenantOrCollectionFields and
// TestSearchKnowledge_ExtraTenantAndCollectionInput_IgnoredUsesConfigScope
// (cmd/mcp-server/handler_test.go) drive a real MCP session over a real transport with `tenant_id`
// and `collection` in the JSON and read the retrieval.Query that reached the searcher. Those are the
// proof. They are `go test` tests, and `cli eval --isolation` — the gate the PRD makes pass/fail
// (§2.7 NFR-3) — does not run them.
//
// This case is what the release gate can hold instead: the three shapes an escalation takes inside
// that directory, refused before anyone can write one.
//
//   - a retrieval.Query built without one of the two scope fields, which ships a scope this package
//     never decided;
//   - a scope value that mentions a tool input, at any depth and through any alias. Resolved by
//     go/types rather than by reading the source's shape, so `proxy := in` and a struct embedding
//     the input type are the same question as `in` itself;
//   - a scope value assembled by a call, which is where an input is laundered in without its name
//     appearing beside the scope's;
//   - and any assignment to a scope field anywhere in the package. `q.TenantID = …` after a correct
//     literal is the escalation no rule about the literal can see.
//
// # What it does not catch, and this is not a detail
//
// It reads one directory. A query built in another package and returned from here —
// `return escalate.Build(cfg.Collection, in.TenantID)` — has no retrieval.Query literal in
// cmd/mcp-server to check, and the real handler already contains a second, correctly scoped literal
// (explainEmptyArea's probe) that satisfies the "did this scan see any query at all" guard on its
// own. Such a tree passes this case clean. TestMCPScopeCase_DoesNotFollowIndirectionOutOfThePackage
// pins that, over a fixture written to escalate, so nobody can later read a green run as coverage
// it does not have.
//
// Following it would mean answering a data-flow question across the import graph, which is a
// different tool from this one. The report says the binding is not proven here, and it says why.
func MCPScopeBindingCase(root string) Case {
	return Case{Name: "mcp server: the search scope is not copied from a tool input in cmd/mcp-server",
		Run: func(_ context.Context) string {
			scan, err := scanMCPScope(root)
			if err != nil {
				// Not reported as a pass, same rule as the architecture case: a scan that could not run
				// proved nothing, and reporting an unrun check as a held invariant is the one thing this
				// suite may never do.
				return fmt.Sprintf("the scope scan over %q could not run, so nothing was checked: %v",
					filepath.Join(root, mcpServerDir), err)
			}
			if problem := scan.lookedAtSomething(root); problem != "" {
				return problem
			}
			if len(scan.problems) > 0 {
				return fmt.Sprintf("%d escalation route(s) into the search scope in %s: %s",
					len(scan.problems), mcpServerDir, strings.Join(scan.problems, "; "))
			}
			return ""
		}}
}

// mcpScopeScan is one reading of cmd/mcp-server's non-test source.
type mcpScopeScan struct {
	fset *token.FileSet
	info *types.Info

	// inputTypes are the named struct types this package decodes from a tool call's JSON, found by
	// their tags rather than by their names so a rename changes nothing.
	inputTypes []types.Type

	// files and queries are the non-vacuity counters. Every rule below holds, silently and
	// completely, over a directory with no source in it and over a package that builds no query —
	// and on a host with no source tree the first is what the scan finds.
	files   int
	queries int

	problems []string
}

// scanMCPScope parses and type-checks cmd/mcp-server, then applies the rules.
//
// The type check runs with no importer and an error handler that discards everything, on purpose.
// Resolving internal/retrieval, the MCP SDK and gRPC would mean type-checking the module and its
// dependencies from source at release time; every question this case asks is about a type declared
// in cmd/mcp-server itself, and those resolve completely from these files alone. What an unresolved
// import costs is precision about types this case never asks about — a retrieval.Query is
// recognised by its written name, below, exactly as the architecture case reads imports as written.
func scanMCPScope(root string) (*mcpScopeScan, error) {
	paths, err := filepath.Glob(filepath.Join(root, mcpServerDir, "*.go"))
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, path := range paths {
		// Tests are skipped by construction. cmd/mcp-server's own tests name a scope on purpose —
		// that is what an escalation test does — and a rule that read them would report the proof as
		// the defect.
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil, fmt.Errorf("parsing %s: %w", filepath.Base(path), perr)
		}
		files = append(files, f)
	}

	s := &mcpScopeScan{
		fset:  fset,
		files: len(files),
		info:  &types.Info{Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}},
	}
	conf := types.Config{Error: func(error) {}, Importer: nil}
	pkg, _ := conf.Check("main", fset, files, s.info)
	s.collectInputTypes(pkg)

	for _, f := range files {
		ast.Inspect(f, s.check)
	}
	return s, nil
}

// collectInputTypes records every named struct in this package that carries a JSON tag.
//
// Config has none and is therefore never mistaken for input, which is the distinction that has to
// hold: it is the one struct in cmd/mcp-server whose scope fields are allowed to reach a query.
func (s *mcpScopeScan) collectInputTypes(pkg *types.Package) {
	if pkg == nil {
		return
	}
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		tn, ok := scope.Lookup(name).(*types.TypeName)
		if !ok {
			continue
		}
		st, ok := tn.Type().Underlying().(*types.Struct)
		if !ok {
			continue
		}
		for i := range st.NumFields() {
			if strings.Contains(st.Tag(i), "json:") {
				s.inputTypes = append(s.inputTypes, tn.Type())
				break
			}
		}
	}
}

// carriesInput reports whether a type is a tool input or reaches one through embedding.
//
// The embedding walk is why this asks the resolved type rather than the written one: a
// `struct{ SearchKnowledgeInput }` promotes every field of the input and declares no tag of its
// own, so a reading of the declaration's shape sees an ordinary struct.
//
// The reach is wider than embedding, and the reason is in checkQueryLiteral rather than here: it
// asks this about *every* identifier in the scope expression, including the selectors partway along
// a chain. So an input parked behind a named field of a wrapper — `b.V.TenantID`, where V is the
// input — is caught on V, whose own resolved type is the input, without this walk ever reaching it.
// A review found that by attacking a generic wrapper this function alone would have let through.
// Worth knowing before trusting this comment's first paragraph as the limit of what is covered.
func (s *mcpScopeScan) carriesInput(t types.Type) bool {
	return s.carriesInputDepth(t, 0)
}

func (s *mcpScopeScan) carriesInputDepth(t types.Type, depth int) bool {
	// Bounded because a type can embed itself through a pointer, and this walk is over a graph
	// rather than a tree. Nothing legitimate nests this far.
	if t == nil || depth > 8 {
		return false
	}
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}
	for _, in := range s.inputTypes {
		if types.Identical(t, in) {
			return true
		}
	}
	st, ok := t.Underlying().(*types.Struct)
	if !ok {
		return false
	}
	for i := range st.NumFields() {
		if f := st.Field(i); f.Anonymous() && s.carriesInputDepth(f.Type(), depth+1) {
			return true
		}
	}
	return false
}

// lookedAtSomething reports the ways this scan can hold over nothing at all.
//
// Each is a green run that means "there was nothing here", and none of them is distinguishable from
// a held rule in the report. They are checked before the problems are read, because a scan that
// found no query and no input has no problems to find.
func (s *mcpScopeScan) lookedAtSomething(root string) string {
	where := filepath.Join(root, mcpServerDir)
	switch {
	case s.files == 0:
		return fmt.Sprintf("no Go source was found in %s, so nothing was read — this case checks a "+
			"build-time shape and has to run where the source is", where)
	case len(s.inputTypes) == 0:
		return fmt.Sprintf("no JSON-decoded input type was found in %s, so the rule about tool input "+
			"was applied to nothing", where)
	case s.queries == 0:
		return fmt.Sprintf("no retrieval.Query is built in %s, so every rule about how its scope is "+
			"set held over nothing", where)
	}
	return ""
}

// check applies the rules to one node.
func (s *mcpScopeScan) check(n ast.Node) bool {
	switch node := n.(type) {
	case *ast.AssignStmt:
		for _, lhs := range node.Lhs {
			sel, ok := lhs.(*ast.SelectorExpr)
			if !ok || !slices.Contains(scopeFields, sel.Sel.Name) {
				continue
			}
			s.report(lhs.Pos(), "assigns %s after the fact; the scope is decided where the query is "+
				"built or it is not decided at all, and an assignment is the one shape the rules on "+
				"the literal cannot see", types.ExprString(lhs))
		}
	case *ast.CompositeLit:
		if !isRetrievalQuery(node.Type) {
			return true
		}
		s.queries++
		s.checkQueryLiteral(node)
	}
	return true
}

// checkQueryLiteral applies the three rules about how a retrieval.Query's scope is set.
func (s *mcpScopeScan) checkQueryLiteral(lit *ast.CompositeLit) {
	set := map[string]ast.Expr{}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := kv.Key.(*ast.Ident); ok {
			set[key.Name] = kv.Value
		}
	}

	for _, field := range scopeFields {
		value, ok := set[field]
		if !ok {
			s.report(lit.Pos(), "builds a retrieval.Query without setting %s, so it ships a scope "+
				"this package never decided", field)
			continue
		}
		ast.Inspect(value, func(n ast.Node) bool {
			switch e := n.(type) {
			case *ast.CallExpr:
				// Shape, not flow: a call is refused whatever it does, because what it does is the
				// question this case cannot answer. The scope is copied from a value or it is not
				// set here.
				s.report(e.Pos(), "sets %s from %s; a scope assembled by a call is where an input "+
					"is laundered in without its name appearing beside the scope's",
					field, types.ExprString(value))
			case *ast.Ident:
				obj := s.info.Uses[e]
				if obj == nil {
					obj = s.info.Defs[e]
				}
				if obj != nil && s.carriesInput(obj.Type()) {
					s.report(e.Pos(), "sets %s from %s, whose %s is a tool input (%s) — the scope of "+
						"this server is its instance configuration and a caller may not name it "+
						"(ADR-002 §2.1)", field, types.ExprString(value), e.Name, obj.Type())
				}
			}
			return true
		})
	}
}

// report records one problem, located precisely enough to fix without searching.
func (s *mcpScopeScan) report(pos token.Pos, format string, args ...any) {
	p := s.fset.Position(pos)
	s.problems = append(s.problems,
		fmt.Sprintf("%s:%d %s", filepath.Base(p.Filename), p.Line, fmt.Sprintf(format, args...)))
}

// isRetrievalQuery reports whether a composite literal's type is retrieval.Query.
//
// Read as written rather than resolved, because the type check above runs with no importer — see
// scanMCPScope on why. The architecture case already fails the suite if some other package starts
// speaking to the store directly, so `retrieval` here names the one package it can.
func isRetrievalQuery(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "retrieval" && sel.Sel.Name == "Query"
}
