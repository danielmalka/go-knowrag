package eval

import (
	"context"
	"errors"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/retrieval"
	"github.com/danielmalka/go-knowrag/internal/schema"
)

// profiledSearcher is a Searcher that also reports its model profile and resolves modes the way
// *retrieval.Searcher does, which is how the harness learns what actually ran.
type profiledSearcher struct {
	inner   Searcher
	profile schema.ModelProfile
}

func (p profiledSearcher) Search(ctx context.Context, q retrieval.Query) ([]retrieval.Result, error) {
	return p.inner.Search(ctx, q)
}
func (p profiledSearcher) Profile() schema.ModelProfile { return p.profile }
func (p profiledSearcher) ResolvedMode(m retrieval.SearchMode) (retrieval.SearchMode, error) {
	return retrieval.ResolveMode(m, p.profile)
}

func eg2ForEval(t *testing.T) schema.ModelProfile {
	t.Helper()
	p, ok := schema.ProfileByID("embeddinggemma-2")
	if !ok {
		t.Fatal("embeddinggemma-2 not registered")
	}
	return p
}

// The golden report names the mode that ran: "dense-only" on a dense-only profile, "hybrid" on the
// default one (and for a searcher that says nothing, as before).
func TestGoldenGate_ReportNamesTheModeThatRan(t *testing.T) {
	path, _ := goldenGateFixture(t)
	for name, tc := range map[string]struct {
		searcher Searcher
		want     string
	}{
		"dense-only profile": {profiledSearcher{inner: &modeSearcher{byMode: nil}, profile: eg2ForEval(t)}, "dense-only"},
		"default profile":    {profiledSearcher{inner: &modeSearcher{byMode: nil}, profile: schema.DefaultProfile()}, "hybrid"},
		"unprofiled fake":    {&modeSearcher{byMode: nil}, "hybrid"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := GoldenGate(t.Context(), Options{
				Collection: "interno", TenantID: "tenant-a", Searcher: tc.searcher, GoldenSetPath: path,
			})
			if err != nil {
				t.Fatalf("GoldenGate: %v", err)
			}
			if want := "Mode: " + tc.want; !containsLine(got.Summary, want) {
				t.Errorf("the report does not say %q:\n%s", want, got.Summary)
			}
		})
	}
}

func TestGoldenGate_CompareModes_RefusedOnADenseOnlyProfile(t *testing.T) {
	path, _ := goldenGateFixture(t)
	_, err := GoldenGate(t.Context(), Options{
		Collection: "interno", TenantID: "tenant-a", GoldenSetPath: path, CompareModes: true,
		Searcher: profiledSearcher{inner: &modeSearcher{byMode: nil}, profile: eg2ForEval(t)},
	})
	if !errors.Is(err, ErrNoHybridToCompare) {
		t.Fatalf("GoldenGate = %v, want ErrNoHybridToCompare", err)
	}
}

func containsLine(text, want string) bool {
	for _, line := range splitLines(text) {
		if line == want || line == "- "+want {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}
