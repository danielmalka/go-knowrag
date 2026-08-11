package clicmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/config"
	"github.com/danielmalka/go-knowrag/internal/retrieval"
)

const searchGolden = "testdata/search_golden.jsonl"

// fakeSearcher records every query it is handed, which is how the tests below prove negatives: not
// "the output looked right", but "the search layer was asked for exactly this, or was not asked at
// all".
type fakeSearcher struct {
	mu      sync.Mutex
	queries []retrieval.Query

	results []retrieval.Result
	err     error
	// resultsFn answers from the query instead of results, which is the only way to model an index
	// where the answer depends on a flag — an inclusion that adds rows rather than replacing them.
	resultsFn func(retrieval.Query) []retrieval.Result
}

func (f *fakeSearcher) Search(_ context.Context, q retrieval.Query) ([]retrieval.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, q)
	if f.err != nil {
		return nil, f.err
	}
	if f.resultsFn != nil {
		return f.resultsFn(q), nil
	}
	return f.results, nil
}

func (f *fakeSearcher) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queries)
}

func (f *fakeSearcher) lastQuery(t *testing.T) retrieval.Query {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queries) == 0 {
		t.Fatal("the searcher was never called")
	}
	return f.queries[len(f.queries)-1]
}

// run drives the real command with real flag parsing, and counts how often the connection was
// opened. The count is the assertion for every test about a refusal: an error alone cannot tell a
// command that refused before dialing from one that dialed, searched and then complained.
func run(t *testing.T, s *fakeSearcher, args ...string) (stdout string, connects int, err error) {
	t.Helper()

	opened := 0
	cmd := NewSearchCmd(&config.Config{DefaultCollection: "interno"},
		func(context.Context) (Searcher, func(), error) {
			opened++
			return s, func() {}, nil
		})

	// Two buffers, not one. The --json contract is about stdout alone, and cobra writes its own
	// error line to stderr — joining them here would make every assertion about "stdout is the whole
	// envelope" a test of the test harness.
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), opened, err
}

func sampleHits() []retrieval.Result {
	return []retrieval.Result{
		{UID: "uid-1", ChunkIndex: 0, Text: "Kubernetes ingress notes.",
			Breadcrumb: "infra > kubernetes > ingress", Path: "infra/kubernetes.md",
			Score: 0.87, Untrusted: true},
		{UID: "uid-2", ChunkIndex: 3, Text: "The certificate rotation runbook.",
			Breadcrumb: "", Path: "infra/certs.md", Score: 0.42, Untrusted: true},
	}
}

// TestSearchCmd_FlagsMapToQuery is the flag-to-field contract. The last row is the one that carries
// the security claim: with no flags beyond the tenant, both privileged inclusions are off.
func TestSearchCmd_FlagsMapToQuery(t *testing.T) {
	tests := map[string]struct {
		args []string
		want retrieval.Query
	}{
		"every flag given": {
			args: []string{"como rotacionar certificados", "--tenant", "malka", "--collection", "outra",
				"--area", "infra", "--top-k", "9", "--include-archived", "--include-private"},
			want: retrieval.Query{
				Collection: "outra", TenantID: "malka", Text: "como rotacionar certificados",
				TopK: 9, Area: "infra", IncludeArchived: true, IncludePrivate: true,
			},
		},
		// The row that carries the security claim: with nothing but a tenant given, neither
		// privileged inclusion is on. It states the top_k default symbolically rather than as a
		// number, and deliberately: what this row proves is that the default reaches the query at
		// all. Whether the number is the right one is a question about the MCP tool's default, which
		// lives in another package — cmd/mcp-server/search_parity_test.go is what compares them.
		"defaults": {
			args: []string{"certificados", "--tenant", "malka"},
			want: retrieval.Query{
				Collection: "interno", TenantID: "malka", Text: "certificados",
				TopK: defaultTopK, Area: "", IncludeArchived: false, IncludePrivate: false,
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			s := &fakeSearcher{results: sampleHits()}
			if _, _, err := run(t, s, tc.args...); err != nil {
				t.Fatalf("search %v: %v", tc.args, err)
			}
			// DeepEqual and not ==: Query carries a []string facet this command never sets, and a
			// field-by-field comparison here would be a list to forget to extend.
			if got := s.lastQuery(t); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("the query built from %v is\n %+v\nwant\n %+v", tc.args, got, tc.want)
			}
		})
	}
}

// TestSearchCmd_MissingTenant_RefusesBeforeConnecting is the CLI half of S07's "tenant_id vazio →
// erro, não busca ampla". retrieval.Search enforces it too, but that check fires after the
// connection is open; this one must fire before the command reaches for one.
//
// The assertion is the call log, not the message: a test on the wording passes just as happily
// against a command that printed the right sentence and searched anyway.
func TestSearchCmd_MissingTenant_RefusesBeforeConnecting(t *testing.T) {
	s := &fakeSearcher{results: sampleHits()}

	_, connects, err := run(t, s, "certificados")
	if err == nil {
		t.Fatal("a search with no --tenant succeeded, so it searched every tenant it could reach")
	}
	if s.calls() != 0 {
		t.Errorf("the searcher was called %d time(s) by a run with no --tenant", s.calls())
	}
	if connects != 0 {
		t.Errorf("the command opened %d connection(s) for a search it was never going to run", connects)
	}
	if got := CategoryOf(err); got != CategoryUsage {
		t.Errorf("CategoryOf(%v) = %q, want %q — the command line is what has to change", err, got, CategoryUsage)
	}
	if !strings.Contains(err.Error(), "--tenant") {
		t.Errorf("the refusal %q does not name the flag that is missing", err)
	}
}

// TestSearchCmd_BlankTenant_IsAlsoRefused covers the value that is not empty and means nothing.
// `--tenant " "` clears a `== ""` check and then searches a tenant no point carries, which reports
// as an honest empty answer about a scope that does not exist.
func TestSearchCmd_BlankTenant_IsAlsoRefused(t *testing.T) {
	s := &fakeSearcher{results: sampleHits()}

	if _, _, err := run(t, s, "certificados", "--tenant", "   "); err == nil {
		t.Fatal("--tenant '   ' was accepted, and an empty answer under it reads as an empty index")
	}
	if s.calls() != 0 {
		t.Errorf("the searcher was called %d time(s) for a whitespace tenant", s.calls())
	}
}

// TestSearchCmd_HumanOutput_CarriesEveryLocator pins what an operator needs to find the chunk
// again: score, path, breadcrumb, uid and chunk_index, per result.
func TestSearchCmd_HumanOutput_CarriesEveryLocator(t *testing.T) {
	s := &fakeSearcher{results: sampleHits()}

	out, _, err := run(t, s, "certificados", "--tenant", "malka")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, want := range []string{
		"0.8700", "infra/kubernetes.md", "infra > kubernetes > ingress", "uid-1", "chunk_index: 0",
		"0.4200", "infra/certs.md", "uid-2", "chunk_index: 3",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the human output does not carry %q:\n%s", want, out)
		}
	}
	// The second hit has no headings, which is ordinary — a chunk can sit above the first heading of
	// its note. What must not happen is an indented blank line, which reads as a rendering fault.
	//
	// The assertion is that the line carries something, not that it carries a particular phrase. A
	// `Contains(out, noBreadcrumb)` would pass with the stand-in set to the empty string, which is
	// precisely the defect: the test would certify the blank line it exists to forbid.
	line := breadcrumbLineAfter(t, out, "infra/certs.md")
	if strings.TrimSpace(line) == "" {
		t.Errorf("a chunk with no headings printed a blank line where its breadcrumb goes:\n%s", out)
	}
}

// breadcrumbLineAfter returns the line the human output puts directly under the one naming path.
func breadcrumbLineAfter(t *testing.T, out, path string) string {
	t.Helper()

	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if strings.Contains(line, path) && i+1 < len(lines) {
			return lines[i+1]
		}
	}
	t.Fatalf("no line naming %s, followed by another, in:\n%s", path, out)
	return ""
}

// TestSearchCmd_EmptyResults_SayItAndExitZero covers the answer that is easiest to render as
// nothing at all. An empty search is a successful search, and the terminal must say so.
func TestSearchCmd_EmptyResults_SayItAndExitZero(t *testing.T) {
	s := &fakeSearcher{results: nil}

	out, _, err := run(t, s, "certificados", "--tenant", "malka")
	if err != nil {
		t.Fatalf("an empty answer failed the command: %v", err)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatal("an empty answer printed nothing, which is what a crashed command prints")
	}
	if !strings.Contains(out, noMatches) {
		t.Errorf("an empty answer printed %q, not the message that says the index answered", out)
	}
}

// TestSearchCmd_JSON_MatchesGolden is the schema contract of `search --json`, as a fixture rather
// than as field assertions — a rename keeps every value correct and breaks every script.
func TestSearchCmd_JSON_MatchesGolden(t *testing.T) {
	s := &fakeSearcher{results: sampleHits()}

	out, _, err := run(t, s, "certificados", "--tenant", "malka", "--json")
	if err != nil {
		t.Fatalf("search --json: %v", err)
	}

	want, err := os.ReadFile(searchGolden) // #nosec G304 -- a literal path inside the package
	if err != nil {
		t.Fatalf("reading the golden fixture: %v", err)
	}
	if out != string(want) {
		t.Errorf("the `search --json` shape changed. Any consumer reading these keys breaks with "+
			"it; if the change is intended, update %s deliberately.\n\ngot:\n%s\nwant:\n%s",
			searchGolden, out, want)
	}
}

// TestSearchCmd_JSON_IsTheWholeOfStdout covers the other half of --json: stdout has to parse on its
// own. A human line printed alongside the envelope costs the consumer a strip step it has no way to
// know about.
func TestSearchCmd_JSON_IsTheWholeOfStdout(t *testing.T) {
	s := &fakeSearcher{results: sampleHits()}

	out, _, err := run(t, s, "certificados", "--tenant", "malka", "--json")
	if err != nil {
		t.Fatalf("search --json: %v", err)
	}
	if lines := strings.Count(strings.TrimRight(out, "\n"), "\n"); lines != 0 {
		t.Errorf("--json wrote %d extra line(s) beside the envelope:\n%s", lines, out)
	}
	var envelope Result
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("stdout does not parse as JSON on its own: %v\n%s", err, out)
	}
	if !envelope.OK {
		t.Errorf("a successful search reported ok=false: %s", out)
	}
}

// TestSearchCmd_JSON_FailureWritesAnEnvelope keeps the promise --json makes in the case a consumer
// most needs it kept. A script that gets an empty stdout and a non-zero status knows only that
// something went wrong; the envelope says which kind.
func TestSearchCmd_JSON_FailureWritesAnEnvelope(t *testing.T) {
	s := &fakeSearcher{err: errors.New("qdrant is unreachable")}

	out, _, err := run(t, s, "certificados", "--tenant", "malka", "--json")
	if err == nil {
		t.Fatal("a failing search returned no error")
	}

	var envelope Result
	if uerr := json.Unmarshal([]byte(out), &envelope); uerr != nil {
		t.Fatalf("a failed --json run wrote no parseable envelope: %v\n%s", uerr, out)
	}
	if envelope.OK {
		t.Errorf("a failed search reported ok=true: %s", out)
	}
	if envelope.Error == nil || envelope.Error.Category != string(CategoryBackend) {
		t.Errorf("the envelope does not carry the backend category: %s", out)
	}
	if envelope.Error != nil && !strings.Contains(envelope.Error.Message, "qdrant is unreachable") {
		t.Errorf("the envelope swallowed the underlying message: %s", out)
	}
}

// TestSearchCmd_StructuralRejection_IsAUsageFailure keeps the two failure kinds apart at the exit
// code. internal/retrieval exports these as sentinels precisely so a caller can tell "you asked for
// something impossible" from "Qdrant is down", and a scheduler that cannot tell them apart retries
// a command line that will fail identically forever.
func TestSearchCmd_StructuralRejection_IsAUsageFailure(t *testing.T) {
	tests := map[string]struct {
		err  error
		want Category
	}{
		"top_k out of range": {
			err:  fmt.Errorf("%w: top_k = 0, want 1..1000", retrieval.ErrInvalidTopK),
			want: CategoryUsage,
		},
		"empty collection": {err: retrieval.ErrEmptyCollection, want: CategoryUsage},
		"an unreachable database": {
			err:  errors.New("rpc error: code = Unavailable"),
			want: CategoryBackend,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			s := &fakeSearcher{err: tc.err}
			_, _, err := run(t, s, "certificados", "--tenant", "malka")
			if err == nil {
				t.Fatal("the failing search returned no error")
			}
			if got := CategoryOf(err); got != tc.want {
				t.Errorf("CategoryOf(%v) = %q, want %q", err, got, tc.want)
			}
		})
	}
}

// TestSearchCmd_IncludePrivate_OnlyAddsPrivateResults is the guarantee that makes the parity claim
// safe to state with the flag off (cmd/mcp-server/search_parity_test.go): turning it on must add
// private chunks and do nothing else. A flag that also reordered or dropped a result would make the
// privileged view a different search rather than a wider one, and then the comparison with the MCP
// adapter would only hold for the exact configuration it was run in.
//
// The private hit is returned in the middle of the list, not appended, because that is where a
// fusion would put it — appending it would let a command that concatenates two searches pass.
func TestSearchCmd_IncludePrivate_OnlyAddsPrivateResults(t *testing.T) {
	const privateUID = "uid-private"
	public := sampleHits()
	private := retrieval.Result{UID: privateUID, ChunkIndex: 1, Text: "salary review notes.",
		Breadcrumb: "pessoal > rh", Path: "pessoal/rh.md", Score: 0.61, Untrusted: true}
	withPrivate := []retrieval.Result{public[0], private, public[1]}

	s := &fakeSearcher{resultsFn: func(q retrieval.Query) []retrieval.Result {
		if q.IncludePrivate {
			return withPrivate
		}
		return public
	}}

	off := searchJSONHits(t, s, "certificados", "--tenant", "malka", "--json")
	on := searchJSONHits(t, s, "certificados", "--tenant", "malka", "--include-private", "--json")

	// The diff, computed rather than eyeballed: strike the private uid out of the privileged answer
	// and what is left has to be the unprivileged answer, element for element and in order.
	var kept []searchHitJSON
	added := 0
	for _, h := range on {
		if h.UID == privateUID {
			added++
			continue
		}
		kept = append(kept, h)
	}
	if added != 1 {
		t.Fatalf("--include-private added %d private hit(s), want exactly the one the index has", added)
	}
	if len(kept) != len(off) {
		t.Fatalf("--include-private changed the public answer from %d hit(s) to %d", len(off), len(kept))
	}
	for i := range kept {
		if kept[i] != off[i] {
			t.Errorf("--include-private changed public hit %d beyond adding a private one:\n"+
				"with the flag:    %+v\nwithout the flag: %+v", i, kept[i], off[i])
		}
	}
}

// searchJSONHits runs one search in --json mode and returns the payload as the hits it carries.
func searchJSONHits(t *testing.T, s *fakeSearcher, args ...string) []searchHitJSON {
	t.Helper()

	out, _, err := run(t, s, args...)
	if err != nil {
		t.Fatalf("search %v: %v", args, err)
	}
	var envelope struct {
		OK   bool            `json:"ok"`
		Data []searchHitJSON `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("parsing the envelope from %v: %v\n%s", args, err, out)
	}
	if !envelope.OK {
		t.Fatalf("search %v reported ok=false: %s", args, out)
	}
	return envelope.Data
}
