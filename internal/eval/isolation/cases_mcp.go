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
// fields themselves, and a rename there makes every literal below stop parsing as a scope
// assignment, which this case reports as nothing left to check rather than as a pass.
var scopeFields = []string{"Collection", "TenantID"}

// MCPScopeBindingCase fails the isolation suite if anything a tool caller sends can reach the scope
// of the search cmd/mcp-server runs.
//
// It is the one case in this suite that cannot drive its subject. cmd/mcp-server is `package main`,
// so no package can import it and no in-process call can enter its handler from here; the dynamic
// proof lives where it is reachable, in that package's own tests — TestToolsList_SearchKnowledge_
// SchemaHasNoTenantOrCollectionFields and TestSearchKnowledge_ExtraTenantAndCollectionInput_
// IgnoredUsesConfigScope (cmd/mcp-server/handler_test.go) drive a real MCP session over a real
// transport with `tenant_id` and `collection` in the JSON and prove the scope that reached the
// search layer was the configured one.
//
// What those cannot do is run at release time. `cli eval --isolation` is the gate the PRD makes
// pass/fail (§2.7 NFR-3), and until this case existed it enforced nothing about the MCP scope at
// all — the report said so in as many words. Same argument as ArchitectureBoundaryCase, which reads
// source for the same reason: an invariant a separate CI step happens to check is not an invariant
// the release gate holds.
//
// What it enforces, and why these three rules and not a grep for the field names:
//
//   - every retrieval.Query built in cmd/mcp-server sets both scope fields. A literal that omits one
//     ships an empty scope, which internal/retrieval refuses (ErrEmptyTenant, retrieval/query.go) —
//     but it refuses at run time, on the call, and this is the gate that runs before the release;
//   - each is set from a like-named field of some other value — `TenantID: cfg.TenantID` and no
//     other shape. Not a call, not a literal, not a differently-named field: a call is where an
//     input value is laundered into the scope without either name appearing next to the other;
//   - and the value it is copied from is not the tool input. The input type is found by its JSON
//     tags rather than by name, so the rule survives a rename, and every identifier bound to one
//     anywhere in the package is treated as tool input everywhere in it.
//
// Plus the ban that removes the shape the first three would otherwise have to chase: no assignment
// anywhere in the package writes a scope field. `q.TenantID = in.Whatever` after a correct literal
// is the escalation the literal rules cannot see, and there is no legitimate reason for one — the
// scope is decided where the query is built or it is not decided at all.
//
// The rules are stated over source this case parses, so they hold for whatever cmd/mcp-server does
// next rather than for what its input struct happens to contain today. That is the difference the
// debt item asked for: the escalation is blocked by an absence — no scope field on the input, no
// read of one in the handler — and an absence is exactly what a refactor removes without noticing.
func MCPScopeBindingCase(root string) Case {
	return Case{Name: "mcp server: no tool input reaches the search scope", Run: func(_ context.Context) string {
		scan, err := scanMCPScope(root)
		if err != nil {
			// Not reported as a pass, same rule as the architecture case: a scan that could not run
			// proved nothing, and reporting an unrun check as a held invariant is the one thing this
			// suite may never do.
			return fmt.Sprintf("the scope scan over %q could not run, so the binding is unproven: %v",
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
	// inputTypes are the struct types this package decodes from the tool call's JSON, and inputVars
	// every identifier bound to one. Both are collected before any rule is applied, because the
	// value being checked and the declaration that makes it tool input are routinely in different
	// files.
	inputTypes map[string]bool
	inputVars  map[string]bool

	// files and queries are the non-vacuity counters. Every rule below holds, silently and
	// completely, over a directory with no source in it and over a package that builds no query —
	// and on a host with no source tree the first is what the scan finds.
	files   int
	queries int

	problems []string
}

// scanMCPScope parses cmd/mcp-server and applies the four rules.
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
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return nil, fmt.Errorf("parsing %s: %w", filepath.Base(path), perr)
		}
		files = append(files, f)
	}

	s := &mcpScopeScan{files: len(files), inputTypes: map[string]bool{}, inputVars: map[string]bool{}}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			if ts, ok := n.(*ast.TypeSpec); ok {
				if st, ok := ts.Type.(*ast.StructType); ok && hasJSONTag(st) {
					s.inputTypes[ts.Name.Name] = true
				}
			}
			return true
		})
	}
	for _, f := range files {
		ast.Inspect(f, s.collectInputVars)
	}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool { return s.check(fset, n) })
	}
	return s, nil
}

// lookedAtSomething reports the ways this scan can hold over nothing at all.
//
// Each of the three is a green run that means "there was nothing here", and none of them is
// distinguishable from a held invariant in the report. They are checked before the problems are
// read, because a scan that found no query and no input has no problems to find.
func (s *mcpScopeScan) lookedAtSomething(root string) string {
	where := filepath.Join(root, mcpServerDir)
	switch {
	case s.files == 0:
		return fmt.Sprintf("no Go source was found in %s, so nothing was read and the binding is "+
			"unproven — this case checks a build-time invariant and has to run where the source is", where)
	case len(s.inputTypes) == 0:
		return fmt.Sprintf("no JSON-decoded input type was found in %s, so the rule that no tool "+
			"input reaches the scope was applied to nothing and the binding is unproven", where)
	case s.queries == 0:
		return fmt.Sprintf("no retrieval.Query is built in %s, so every rule about how its scope is "+
			"set held over nothing and the binding is unproven", where)
	}
	return ""
}

// collectInputVars records every identifier bound to a tool-input value: function and function
// literal parameters, and package or local declarations.
//
// Package-wide and by name, deliberately. A per-function reading would be more precise and would
// buy nothing: a name reused for something that is not tool input makes this case stricter, which
// costs a maintainer one rename, while the precise version's failure mode is an escalation this
// case does not see.
func (s *mcpScopeScan) collectInputVars(n ast.Node) bool {
	record := func(fields *ast.FieldList) {
		if fields == nil {
			return
		}
		for _, f := range fields.List {
			if !s.inputTypes[bareTypeName(f.Type)] {
				continue
			}
			for _, name := range f.Names {
				s.inputVars[name.Name] = true
			}
		}
	}
	switch node := n.(type) {
	case *ast.FuncDecl:
		record(node.Type.Params)
	case *ast.FuncLit:
		record(node.Type.Params)
	case *ast.ValueSpec:
		if s.inputTypes[bareTypeName(node.Type)] {
			for _, name := range node.Names {
				s.inputVars[name.Name] = true
			}
		}
	}
	return true
}

// check applies the rules to one node.
func (s *mcpScopeScan) check(fset *token.FileSet, n ast.Node) bool {
	switch node := n.(type) {
	case *ast.AssignStmt:
		for _, lhs := range node.Lhs {
			sel, ok := lhs.(*ast.SelectorExpr)
			if !ok || !slices.Contains(scopeFields, sel.Sel.Name) {
				continue
			}
			s.report(fset, lhs.Pos(), "assigns %s after the fact; the scope is decided where the "+
				"query is built or it is not decided at all, and an assignment is the one shape the "+
				"rules on the literal cannot see", types.ExprString(lhs))
		}
	case *ast.CompositeLit:
		if !isRetrievalQuery(node.Type) {
			return true
		}
		s.queries++
		s.checkQueryLiteral(fset, node)
	}
	return true
}

// checkQueryLiteral applies the three rules about how a retrieval.Query's scope is set.
func (s *mcpScopeScan) checkQueryLiteral(fset *token.FileSet, lit *ast.CompositeLit) {
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
			s.report(fset, lit.Pos(), "builds a retrieval.Query without setting %s, so it ships a "+
				"scope this package never decided", field)
			continue
		}
		sel, ok := value.(*ast.SelectorExpr)
		if !ok {
			s.report(fset, value.Pos(), "sets %s from %s; the scope may only be copied from a "+
				"like-named field of another value, because anything else is a place an input value "+
				"can be laundered into the scope", field, types.ExprString(value))
			continue
		}
		root, ok := sel.X.(*ast.Ident)
		if !ok || sel.Sel.Name != field {
			s.report(fset, value.Pos(), "sets %s from %s, not from a plain value's own %s",
				field, types.ExprString(value), field)
			continue
		}
		if s.inputVars[root.Name] {
			s.report(fset, value.Pos(), "sets %s from the tool input %s — the scope of this server "+
				"is its instance configuration and a caller may not name it (ADR-002 §2.1)",
				field, types.ExprString(value))
		}
	}
}

// report records one problem, located precisely enough to fix without searching.
func (s *mcpScopeScan) report(fset *token.FileSet, pos token.Pos, format string, args ...any) {
	p := fset.Position(pos)
	s.problems = append(s.problems,
		fmt.Sprintf("%s:%d %s", filepath.Base(p.Filename), p.Line, fmt.Sprintf(format, args...)))
}

// isRetrievalQuery reports whether a composite literal's type is retrieval.Query.
//
// Read as written, with no import resolution: cmd/mcp-server imports internal/retrieval under its
// own name, and the architecture case already fails the suite if some other package starts speaking
// to the store directly.
func isRetrievalQuery(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "retrieval" && sel.Sel.Name == "Query"
}

// hasJSONTag reports whether a struct declares at least one `json:` tag, which is what makes it a
// type this package decodes from a tool call rather than one it builds for itself.
//
// Config has no tags and is therefore never mistaken for input, which is the distinction that has
// to hold: it is the one struct in cmd/mcp-server whose scope fields are allowed to reach a query.
func hasJSONTag(st *ast.StructType) bool {
	if st.Fields == nil {
		return false
	}
	for _, f := range st.Fields.List {
		if f.Tag != nil && strings.Contains(f.Tag.Value, "json:") {
			return true
		}
	}
	return false
}

// bareTypeName strips pointers and returns the type's own name, or "" for anything else.
func bareTypeName(expr ast.Expr) string {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}
