package retrieval

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/embed"
	"github.com/danielmalka/go-knowrag/internal/schema"
)

// fakeEmbedding is a structurally valid embedding with recognisable values, so an assertion that
// the right half reached the right leg fails on content rather than on a length.
func fakeEmbedding() embed.Embedding {
	dense := make([]float32, embed.DenseDim)
	dense[0] = 0.5
	return embed.Embedding{
		Dense:  dense,
		Sparse: embed.Sparse{Indices: []uint32{3, 9, 27}, Values: []float32{0.1, 0.2, 0.3}},
	}
}

func legFor(t *testing.T, req SearchRequest, vector string) PrefetchLeg {
	t.Helper()
	for _, leg := range req.Prefetch {
		if leg.Vector == vector {
			return leg
		}
	}
	t.Fatalf("no prefetch leg using %q; request has %d leg(s)", vector, len(req.Prefetch))
	return PrefetchLeg{}
}

func hasCondition(cs []Condition, field, value string) bool {
	return slices.Contains(cs, Condition{Field: field, Value: value})
}

func countCondition(cs []Condition, field string) int {
	n := 0
	for _, c := range cs {
		if c.Field == field {
			n++
		}
	}
	return n
}

// TestCalibratedPrefetchLimit_MatchesValidatedLimit is T2's whole point: build.go must not carry a
// second implementation of the floor arithmetic. Same inputs, same number, no exceptions.
func TestCalibratedPrefetchLimit_MatchesValidatedLimit(t *testing.T) {
	q := validQuery()
	q.TopK, q.Offset = 5, 3

	for _, multiplier := range []int{1, 2, DefaultPrefetchMultiplier, 10} {
		want, err := prefetchLimit(q.TopK, q.Offset, multiplier)
		if err != nil {
			t.Fatalf("prefetchLimit(%d, %d, %d): %v", q.TopK, q.Offset, multiplier, err)
		}
		if got := calibratedPrefetchLimit(q, multiplier); got != want {
			t.Errorf("calibratedPrefetchLimit(multiplier=%d) = %d, want %d", multiplier, got, want)
		}
	}
}

// TestCalibratedPrefetchLimit_UnvalidatedQueryFallsBackToTheFloor covers the branch that only a
// caller who skipped Validate can reach. It must degrade to the floor, never to zero — zero would
// be a search that silently returns nothing.
func TestCalibratedPrefetchLimit_UnvalidatedQueryFallsBackToTheFloor(t *testing.T) {
	q := validQuery()
	q.TopK, q.Offset = 5, 3

	if got, want := calibratedPrefetchLimit(q, 0), q.TopK+q.Offset; got != want {
		t.Errorf("calibratedPrefetchLimit(multiplier=0) = %d, want the floor %d", got, want)
	}
}

func TestBuildQueryRequest_ShapesHybridQuery(t *testing.T) {
	q := validQuery()
	q.TopK, q.Offset = 5, 3
	emb := fakeEmbedding()
	limit := calibratedPrefetchLimit(q, DefaultPrefetchMultiplier)

	req := buildQueryRequest(q, emb, limit)

	if req.Collection != q.Collection {
		t.Errorf("Collection = %q, want %q", req.Collection, q.Collection)
	}
	if len(req.Prefetch) != 2 {
		t.Fatalf("%d prefetch leg(s), want exactly 2", len(req.Prefetch))
	}
	if !req.FusionRRF {
		t.Error("FusionRRF = false, want RRF fusion (PRD-contrato §2.3b)")
	}
	if req.Limit != q.TopK {
		t.Errorf("Limit = %d, want TopK %d", req.Limit, q.TopK)
	}
	if req.Offset != q.Offset {
		t.Errorf("Offset = %d, want Offset %d", req.Offset, q.Offset)
	}

	dense := legFor(t, req, schema.DenseVectorName)
	if !slices.Equal(dense.Dense, emb.Dense) {
		t.Error("the dense leg does not carry the dense half of the embedding")
	}
	if len(dense.Sparse.Indices) != 0 {
		t.Error("the dense leg also carries a sparse vector")
	}
	if dense.Limit != limit {
		t.Errorf("dense leg limit = %d, want the calibrated %d", dense.Limit, limit)
	}

	sparse := legFor(t, req, schema.SparseVectorName)
	if !slices.Equal(sparse.Sparse.Indices, emb.Sparse.Indices) || !slices.Equal(sparse.Sparse.Values, emb.Sparse.Values) {
		t.Error("the sparse leg does not carry the sparse half of the embedding")
	}
	if len(sparse.Dense) != 0 {
		t.Error("the sparse leg also carries a dense vector")
	}
	if sparse.Limit != limit {
		t.Errorf("sparse leg limit = %d, want the calibrated %d", sparse.Limit, limit)
	}

	// The vector names come from internal/schema, never from a literal here — a literal would be a
	// second definition of what the collections were provisioned with.
	if dense.Vector == sparse.Vector {
		t.Error("both legs query the same named vector")
	}
}

// TestBuildQueryRequest_ProjectsOnlyTheFieldsFormatResultsReads keeps the payload projection and
// the reader in step: a field added to Result without being requested comes back missing, and
// formatResults would report every point as malformed.
func TestBuildQueryRequest_ProjectsOnlyTheFieldsFormatResultsReads(t *testing.T) {
	req := buildQueryRequest(validQuery(), fakeEmbedding(), 20)
	for _, want := range []string{fieldUID, fieldChunkIndex, fieldText, fieldHeadings, fieldPath} {
		if !slices.Contains(req.PayloadFields, want) {
			t.Errorf("PayloadFields %v does not include %q", req.PayloadFields, want)
		}
	}
}

// TestBuildQueryRequest_EnablesAcornOnBothPrefetches is PRD-contrato §2.3c: ACORN is a per-request
// opt-in, and the filter runs on the prefetch legs, so both legs have to ask for it. The outer
// stage only fuses two already-filtered lists, so it has nothing for ACORN to do.
func TestBuildQueryRequest_EnablesAcornOnBothPrefetches(t *testing.T) {
	req := buildQueryRequest(validQuery(), fakeEmbedding(), 20)
	for _, leg := range req.Prefetch {
		if !leg.Acorn {
			t.Errorf("prefetch leg %q has Acorn = false, want true (PRD-contrato §2.3c)", leg.Vector)
		}
	}
}

// TestPrefetchLeg_CarriesNoSelectivityKnob is the other half of §2.3c, expressed structurally: the
// contract says MaxSelectivity stays at the server default, because Qdrant decides per request from
// a cardinality estimate this process does not have. There is deliberately no field to pin it with,
// so a future "tuning" commit has to add one — and argue with this test first.
func TestPrefetchLeg_CarriesNoSelectivityKnob(t *testing.T) {
	tp := reflect.TypeOf(PrefetchLeg{})
	for i := range tp.NumField() {
		if strings.Contains(strings.ToLower(tp.Field(i).Name), "selectivity") {
			t.Errorf("PrefetchLeg has field %q: MaxSelectivity must stay at the server default "+
				"(PRD-contrato §2.3c)", tp.Field(i).Name)
		}
	}
}

// TestBuildFilter_TenantAlwaysMust is the invariant the package exists for. Every row varies
// something else; none of them can vary the tenant condition away.
func TestBuildFilter_TenantAlwaysMust(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Query)
	}{
		{"bare query", func(*Query) {}},
		{"with offset", func(q *Query) { q.Offset = 40 }},
		{"with area", func(q *Query) { q.Area = "infra" }},
		{"with type", func(q *Query) { q.Type = "concept" }},
		{"with vault", func(q *Query) { q.Vault = "trabalho" }},
		{"with tags", func(q *Query) { q.Tags = []string{"golang", "qdrant"} }},
		{"with every facet", func(q *Query) {
			q.Area, q.Type, q.Vault, q.Tags = "infra", "concept", "trabalho", []string{"golang"}
		}},
		{"archived included", func(q *Query) { q.IncludeArchived = true }},
		{"private included", func(q *Query) { q.IncludePrivate = true }},
		{"every restriction lifted", func(q *Query) { q.IncludeArchived, q.IncludePrivate = true, true }},
		{"other collection", func(q *Query) { q.Collection = "clientes" }},
		{"third collection", func(q *Query) { q.Collection = "base_paga" }},
		{"other tenant", func(q *Query) { q.TenantID = "acme" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := validQuery()
			tc.mutate(&q)

			f := buildFilter(q)
			if n := countCondition(f.Must, fieldTenantID); n != 1 {
				t.Fatalf("%d tenant condition(s) in Must %v, want exactly 1", n, f.Must)
			}
			if !hasCondition(f.Must, fieldTenantID, q.TenantID) {
				t.Errorf("Must %v does not carry tenant_id == %q", f.Must, q.TenantID)
			}
			if countCondition(f.MustNot, fieldTenantID) != 0 {
				t.Errorf("MustNot %v mentions tenant_id, which can only weaken the scope", f.MustNot)
			}

			// The whole request, not just the filter helper: every leg and the outer stage.
			req := buildQueryRequest(q, fakeEmbedding(), 20)
			if !hasCondition(req.Filter.Must, fieldTenantID, q.TenantID) {
				t.Errorf("the outer request filter has no tenant condition: %v", req.Filter.Must)
			}
			for _, leg := range req.Prefetch {
				if !hasCondition(leg.Filter.Must, fieldTenantID, q.TenantID) {
					t.Errorf("prefetch leg %q has no tenant condition: %v", leg.Vector, leg.Filter.Must)
				}
			}
		})
	}
}

func TestBuildFilter(t *testing.T) {
	archived := schema.StatusArchived().String()
	private := schema.VisibilityPrivate().String()
	collections := []string{"interno", "clientes", "base_paga"}

	t.Run("archived is excluded by default", func(t *testing.T) {
		f := buildFilter(validQuery())
		if !hasCondition(f.MustNot, fieldStatus, archived) {
			t.Errorf("MustNot %v does not exclude status == %q", f.MustNot, archived)
		}
	})

	t.Run("archived is included only by the explicit flag", func(t *testing.T) {
		q := validQuery()
		q.IncludeArchived = true

		f := buildFilter(q)
		if countCondition(f.MustNot, fieldStatus) != 0 || countCondition(f.Must, fieldStatus) != 0 {
			t.Errorf("IncludeArchived=true still produced a status condition: %+v", f)
		}
	})

	// Collection is a trust boundary, not a confidentiality classification. The private rule must
	// come out identical in all three collections, in both directions.
	t.Run("private is excluded by default in every collection", func(t *testing.T) {
		for _, c := range collections {
			q := validQuery()
			q.Collection = c

			f := buildFilter(q)
			if !hasCondition(f.MustNot, fieldVisibility, private) {
				t.Errorf("%s: MustNot %v does not exclude visibility == %q", c, f.MustNot, private)
			}
		}
	})

	t.Run("private is included only by the privileged flag, in every collection", func(t *testing.T) {
		for _, c := range collections {
			q := validQuery()
			q.Collection, q.IncludePrivate = c, true

			f := buildFilter(q)
			if countCondition(f.MustNot, fieldVisibility) != 0 || countCondition(f.Must, fieldVisibility) != 0 {
				t.Errorf("%s: IncludePrivate=true still produced a visibility condition: %+v", c, f)
			}
		}
	})

	t.Run("optional facets appear only when set", func(t *testing.T) {
		bare := buildFilter(validQuery())
		for _, field := range []string{fieldArea, fieldType, fieldVault, fieldTags} {
			if countCondition(bare.Must, field) != 0 {
				t.Errorf("an unset %s still produced a condition: %v", field, bare.Must)
			}
		}

		q := validQuery()
		q.Area, q.Type, q.Vault, q.Tags = "infra", "concept", "trabalho", []string{"golang", "qdrant"}
		f := buildFilter(q)
		for _, want := range []Condition{
			{Field: fieldArea, Value: q.Area},
			{Field: fieldType, Value: q.Type},
			{Field: fieldVault, Value: q.Vault},
			{Field: fieldTags, Value: "golang"},
			{Field: fieldTags, Value: "qdrant"},
		} {
			if !hasCondition(f.Must, want.Field, want.Value) {
				t.Errorf("Must %v does not carry %s == %q", f.Must, want.Field, want.Value)
			}
		}
		// Several tags mean "has all of them", one condition each — not one condition carrying a
		// list, which would mean "has any of them".
		if n := countCondition(f.Must, fieldTags); n != 2 {
			t.Errorf("%d tag condition(s), want one per tag (2)", n)
		}
	})

	t.Run("an empty tag is not a condition", func(t *testing.T) {
		q := validQuery()
		q.Tags = []string{"", "golang", ""}

		if n := countCondition(buildFilter(q).Must, fieldTags); n != 1 {
			t.Errorf("%d tag condition(s), want 1 — empty strings must not narrow the answer", n)
		}
	})
}

// TestBuildFilter_PrivateRuleCannotReadCollection parses build.go and asserts that buildFilter's
// body contains no identifier named Collection.
//
// It reads the source rather than the behaviour on purpose. The previous rule was keyed on the
// collection name, which gave a client marking their own note private in `clientes` no protection
// at all. A behavioural test can only show that today's three collections happen to behave alike;
// this one makes the collection-keyed carve-out impossible to reintroduce without deleting an
// assertion that says why. It looks at identifiers, not text, so the comment explaining the rule
// can name the field the code may not touch.
func TestBuildFilter_PrivateRuleCannotReadCollection(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "build.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing build.go: %v", err)
	}

	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "buildFilter" {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatal("no buildFilter in build.go — this test is looking at nothing")
	}

	sawIncludePrivate := false
	ast.Inspect(body, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		switch id.Name {
		case "Collection":
			t.Errorf("buildFilter reads Collection at %s: visibility: private is excluded in every "+
				"collection identically, and the only way past it is Query.IncludePrivate (S07 T5)",
				fset.Position(id.Pos()))
		case "IncludePrivate":
			sawIncludePrivate = true
		}
		return true
	})

	// Non-vacuity: the function this walked has to be the one that carries the rule.
	if !sawIncludePrivate {
		t.Error("buildFilter's body never mentions IncludePrivate — this test is walking the wrong function")
	}
}
