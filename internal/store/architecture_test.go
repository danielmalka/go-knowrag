package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Two boundaries meet in this package, and this test is what keeps them from collapsing into each
// other (S07 Context).
//
//   - S09's invariant: internal/store is the sole holder of the Qdrant connection and the sole
//     importer of the Qdrant client. Nothing outside may dial or speak to it.
//   - S07's invariant: every actual search decision — which filter conditions exist, that the
//     tenant condition is mandatory, that the fusion is RRF, how far each prefetch leg reaches,
//     that ACORN is on — is made in internal/retrieval and nowhere else.
//
// They hold at once because ExecuteQuery has no search semantics of its own. It receives a
// retrieval.SearchRequest that is already finished and transcribes it onto the wire; it adds no
// condition, removes none, and reads none. That is why it is on the allowlist below and why its
// signature is checked rather than just its name: an "improvement" that let it take a tenant, a
// filter or a Query would move a decision into this package, and the signature check is what makes
// that fail here instead of in review.
//
// The allowlist is the mechanism on purpose. A denylist of search-sounding names (Search, Find,
// HybridSearch) is a list of the words somebody already thought of; a new exported function has to
// be added here deliberately, which is the moment to ask whether it belongs in this package at all.
var allowedStoreExports = []string{
	// Construction.
	"NewQdrantClient", "NewClient", "NewQuerier",
	// Schema application (S05).
	"Apply",
	// Point CRUD (S06a).
	"UpsertPoints", "DeleteByFilter", "ScrollByUID",
	// ScrollTenant (D-22): the same scroll as ScrollByUID with one condition fewer, returning the
	// tenant's points grouped by uid. It belongs here because it decides nothing about a search --
	// no vector, no fusion, no ranking, no scoring. It is a filtered read of stored payloads, the
	// same category as ScrollByUID. It exists because asking that question one uid at a time cost
	// one network round trip per note, which is what put NFR-5 8x over budget.
	"ScrollTenant",
	// Stats (S09 T10): the same scroll again, projecting the uid alone, counting rows and distinct
	// uids. It decides nothing about a search — no vector, no fusion, no ranking, no scoring — and
	// it ranks nothing it returns: two integers come back. The reason it is a scroll rather than
	// Qdrant's facet API is written where it is implemented (points.go).
	"Stats",
	// The one sanctioned query passthrough (S07 T8). Its signature is checked below.
	"ExecuteQuery",
	// Value-type plumbing: redaction, formatting and error unwrapping. None of these reach Qdrant.
	"LogValue", "String", "Error", "Unwrap", "UpToDate",
}

// searchShapedNames are names that could only belong to a function that decides something about a
// search. They are listed separately from the allowlist so the failure message can say *why*
// rather than just "not allowed", and so a fixture can prove this test is not vacuous.
var searchShapedNames = []string{"Search", "Find", "Query", "HybridSearch", "SearchPoints", "Retrieve"}

func TestStoreExportsNoSearchFunction(t *testing.T) {
	for _, decl := range storeFuncDecls(t) {
		name := decl.Name.Name
		if !ast.IsExported(name) {
			continue
		}
		if slices.Contains(allowedStoreExports, name) {
			continue
		}
		if slices.Contains(searchShapedNames, name) {
			t.Errorf("internal/store exports %q: search belongs to internal/retrieval, which is the "+
				"only place the mandatory tenant filter is assembled — a query path here would be a "+
				"second place to remember it (S07)", name)
			continue
		}
		t.Errorf("internal/store exports %q, which is not on the allowlist in this test. If it is "+
			"mechanical CRUD, add it here and say so; if it decides anything about a search, it "+
			"belongs in internal/retrieval (S07 T8)", name)
	}
}

// TestExecuteQuerySignature checks the shape, not the name. ExecuteQuery is allowlisted because it
// forwards a finished request; a version taking a tenant, a filter, or anything it would have to
// interpret would be a search function wearing the allowlisted name.
func TestExecuteQuerySignature(t *testing.T) {
	var fn *ast.FuncDecl
	for _, decl := range storeFuncDecls(t) {
		if decl.Name.Name == "ExecuteQuery" {
			fn = decl
		}
	}
	if fn == nil {
		t.Fatal("no ExecuteQuery in internal/store — this test is looking at nothing")
	}

	params := flatTypes(fn.Type.Params)
	if want := []string{"context.Context", "retrieval.SearchRequest"}; !slices.Equal(params, want) {
		t.Errorf("ExecuteQuery takes %v, want %v — anything else is a parameter this package would "+
			"have to interpret, which is a search decision (S07 T8)", params, want)
	}

	results := flatTypes(fn.Type.Results)
	if want := []string{"[]retrieval.ScoredPoint", "error"}; !slices.Equal(results, want) {
		t.Errorf("ExecuteQuery returns %v, want %v — raw scored points, not a formatted result type; "+
			"formatting is internal/retrieval's decision", results, want)
	}
}

// TestStoreDeclaresNoFilterOfItsOwn is the other half of the boundary: transcription may not become
// authorship. Every condition this package puts on the wire has to come from a field of the request
// it was handed, so no literal payload field name may appear in a filter built here.
//
// The names checked are the ones a search filter would be built from. tenant_id and uid are
// legitimately spelled in points.go — S06a scopes its own reads and deletes by them, which is CRUD,
// not search — so this test looks only at query.go.
func TestStoreDeclaresNoFilterOfItsOwn(t *testing.T) {
	src, err := os.ReadFile("query.go")
	if err != nil {
		t.Fatalf("reading query.go: %v", err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "query.go", src, 0)
	if err != nil {
		t.Fatalf("parsing query.go: %v", err)
	}

	forbidden := []string{"tenant_id", "status", "visibility", "area", "vault", "tags"}
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		value := strings.Trim(lit.Value, `"`)
		if slices.Contains(forbidden, value) {
			t.Errorf("query.go contains the payload field name %q at %s: every condition on the wire "+
				"must come from the retrieval.SearchRequest it was handed, never from a literal here "+
				"(S07 T8)", value, fset.Position(lit.Pos()))
		}
		return true
	})
}

// storeFuncDecls parses this package's non-test sources. It refuses to return an empty set: a walk
// that found no files would make every assertion above pass while proving nothing.
func storeFuncDecls(t *testing.T) []*ast.FuncDecl {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}

	fset := token.NewFileSet()
	var out []*ast.FuncDecl
	files := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, name, nil, 0)
		if perr != nil {
			t.Fatalf("parsing %s: %v", name, perr)
		}
		files++
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				out = append(out, fn)
			}
		}
	}

	if files == 0 || len(out) == 0 {
		t.Fatalf("parsed %d file(s) and %d function(s) in internal/store — the walk is wrong, so "+
			"these tests prove nothing", files, len(out))
	}
	return out
}

// flatTypes renders a parameter or result list as one type string per value, expanding grouped
// declarations (`a, b int`) so the position of each type is what is compared.
func flatTypes(fields *ast.FieldList) []string {
	if fields == nil {
		return nil
	}
	var out []string
	for _, f := range fields.List {
		n := max(len(f.Names), 1)
		for range n {
			out = append(out, typeString(f.Type))
		}
	}
	return out
}

func typeString(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return typeString(t.X) + "." + t.Sel.Name
	case *ast.StarExpr:
		return "*" + typeString(t.X)
	case *ast.ArrayType:
		return "[]" + typeString(t.Elt)
	case *ast.Ellipsis:
		return "..." + typeString(t.Elt)
	default:
		return "?"
	}
}
