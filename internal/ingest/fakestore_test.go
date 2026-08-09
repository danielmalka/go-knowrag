package ingest

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danielmalka/go-knowrag/internal/chunk"
	"github.com/danielmalka/go-knowrag/internal/embed"
	"github.com/danielmalka/go-knowrag/internal/schema"
	"github.com/danielmalka/go-knowrag/internal/vault"
)

// journal is the shared call log of the fake store and the spy embedder. It is one list rather than
// a counter per fake because the properties this story has to prove are about *order* — every chunk
// embedded before any write, the prune strictly after a confirmed upsert — and two independent
// counters cannot express either.
type journal struct{ calls []string }

func (j *journal) record(call string) { j.calls = append(j.calls, call) }

// indexOf returns the position of the first call, or -1.
func (j *journal) indexOf(call string) int { return slices.Index(j.calls, call) }

func (j *journal) count(call string) int {
	n := 0
	for _, c := range j.calls {
		if c == call {
			n++
		}
	}
	return n
}

type upsertCall struct {
	TenantID string
	UID      uuid.UUID
	Points   []Point
}

type deleteCall struct {
	TenantID       string
	UID            uuid.UUID
	FromChunkIndex int
}

// fakeStore is an in-memory Store that persists between runs and can be told to fail in the exact
// shapes the crash scenarios need.
//
// It is keyed by tenant_id *and* uid, mirroring the real scoping rule rather than being generous
// about it: a fake keyed by uid alone would make the cross-tenant test pass for the wrong reason.
type fakeStore struct {
	journal *journal
	points  map[string]map[int]PointRecord

	upserts []upsertCall
	deletes []deleteCall

	scrollErr error
	deleteErr error

	// upsertHook decides how many of the batch's points actually land and what the store reports.
	// The write happens *inside* the call, so a partial upsert is injected where a real one would
	// happen rather than only in the upsert→prune window.
	upsertHook func(call int, points []Point) (written int, outcome UpsertOutcome, err error)
}

func newFakeStore(j *journal) *fakeStore {
	return &fakeStore{journal: j, points: map[string]map[int]PointRecord{}}
}

func scope(tenantID string, uid uuid.UUID) string { return tenantID + "|" + uid.String() }

func (f *fakeStore) ScrollByUID(_ context.Context, tenantID string, uid uuid.UUID) ([]PointRecord, error) {
	f.journal.record("scroll")
	if f.scrollErr != nil {
		return nil, f.scrollErr
	}
	out := slices.Collect(maps.Values(f.points[scope(tenantID, uid)]))
	// Descending, deliberately: Qdrant's scroll makes no ordering promise, and returning the points
	// sorted the way the predicate happens to want them would hide a predicate that depends on it.
	slices.SortFunc(out, func(a, b PointRecord) int { return b.ChunkIndex - a.ChunkIndex })
	return out, nil
}

func (f *fakeStore) UpsertPoints(_ context.Context, tenantID string, uid uuid.UUID, points []Point) (UpsertOutcome, error) {
	f.journal.record("upsert")
	f.upserts = append(f.upserts, upsertCall{TenantID: tenantID, UID: uid, Points: points})

	written, outcome, err := len(points), UpsertConfirmed, error(nil)
	if f.upsertHook != nil {
		written, outcome, err = f.upsertHook(len(f.upserts), points)
	}

	key := scope(tenantID, uid)
	if f.points[key] == nil {
		f.points[key] = map[int]PointRecord{}
	}
	for _, p := range points[:written] {
		f.points[key][p.ChunkIndex] = PointRecord{
			ChunkIndex: p.ChunkIndex,
			PointHash:  p.PointHash,
			Fields:     maps.Clone(p.Fields),
		}
	}
	return outcome, err
}

func (f *fakeStore) DeleteByFilter(_ context.Context, tenantID string, uid uuid.UUID, from int) error {
	f.journal.record("delete")
	f.deletes = append(f.deletes, deleteCall{TenantID: tenantID, UID: uid, FromChunkIndex: from})
	if f.deleteErr != nil {
		return f.deleteErr
	}
	for idx := range f.points[scope(tenantID, uid)] {
		if idx >= from {
			delete(f.points[scope(tenantID, uid)], idx)
		}
	}
	return nil
}

// indices returns the chunk_index values held for one scope, sorted — the shape most assertions
// about convergence are written against.
func (f *fakeStore) indices(tenantID string, uid uuid.UUID) []int {
	return slices.Sorted(maps.Keys(f.points[scope(tenantID, uid)]))
}

func (f *fakeStore) record(tenantID string, uid uuid.UUID, index int) (PointRecord, bool) {
	r, ok := f.points[scope(tenantID, uid)][index]
	return r, ok
}

// seed writes a point set directly, bypassing UpsertPoints, so a test can put the store into a
// state a crash would have left behind without that setup counting as a call.
func (f *fakeStore) seed(tenantID string, uid uuid.UUID, records ...PointRecord) {
	key := scope(tenantID, uid)
	if f.points[key] == nil {
		f.points[key] = map[int]PointRecord{}
	}
	for _, r := range records {
		f.points[key][r.ChunkIndex] = r
	}
}

// spyEmbedder counts calls, records the texts it was asked for, and can be made to fail for one
// specific note.
type spyEmbedder struct {
	journal *journal
	inner   embed.FakeEmbedder

	calls int
	texts [][]string

	// failOn makes EmbedDocuments fail when any text contains this substring — the shape a
	// "the embedder fails for note B only" test needs.
	failOn string
	err    error
}

func newSpyEmbedder(j *journal) *spyEmbedder { return &spyEmbedder{journal: j} }

func (s *spyEmbedder) ModelID() string { return s.inner.ModelID() }

func (s *spyEmbedder) EmbedQuery(ctx context.Context, text string) (embed.Embedding, error) {
	return s.inner.EmbedQuery(ctx, text)
}

func (s *spyEmbedder) EmbedDocuments(ctx context.Context, texts []string) ([]embed.Embedding, error) {
	s.journal.record("embed")
	s.calls++
	s.texts = append(s.texts, slices.Clone(texts))

	if s.err != nil {
		return nil, s.err
	}
	if s.failOn != "" {
		for _, t := range texts {
			if strings.Contains(t, s.failOn) {
				return nil, fmt.Errorf("spy embedder: refusing text containing %q", s.failOn)
			}
		}
	}
	return s.inner.EmbedDocuments(ctx, texts)
}

// testTenant is the tenant every fixture writes under. A second one appears only in the
// cross-tenant test, which is the only place two are meaningful.
const testTenant = "tenant-a"

// testHandshake is a complete, plausible BackendHandshakeInfo — all seven fields populated, because
// ComputePointHash hashes all seven and a fixture that left one at its zero value would make the
// per-field tests prove less than they claim.
func testHandshake() embed.BackendHandshakeInfo {
	return embed.BackendHandshakeInfo{
		ModelRevision:     schema.BGEM3Revision,
		TokenizerRevision: schema.BGEM3Revision,
		Dim:               embed.DenseDim,
		Normalization:     embed.ExpectedNormalization,
		Pooling:           embed.ExpectedPooling,
		Precision:         embed.ExpectedPrecision,
		SparseParams:      map[string]string{"indices": "token_id", "values": "lexical_weight"},
	}
}

// testChunkConfig is wide open on purpose: with a floor of 1 no section is ever merged away, so a
// fixture with n `##` sections produces exactly n chunks and the tests can say "chunk 2" and mean it.
func testChunkConfig() chunk.Config {
	return chunk.Config{FloorTokens: 1, CeilingTokens: 1000}
}

func testDeps(store Store, embedder embed.Embedder) Deps {
	return Deps{
		TenantID:  testTenant,
		Store:     store,
		Embedder:  embedder,
		Handshake: testHandshake(),
		Chunk:     testChunkConfig(),
		Tokens:    chunk.FakeTokenCounter{},
	}
}

// testNote builds a note with n `##` sections, therefore n chunks under testChunkConfig.
//
// The body opens directly with the first `##` so there is no preamble section — the chunk count is
// exactly n, with no off-by-one to remember at every call site.
func testNote(t *testing.T, path string, n int) vault.Note {
	t.Helper()
	return noteWithBodies(t, path, sectionBodies(n)...)
}

// noteWithBodies builds a note whose sections carry the given texts, which is what lets a test
// change chunk 0 and chunk 2 and leave chunk 1 alone.
func noteWithBodies(t *testing.T, path string, bodies ...string) vault.Note {
	t.Helper()
	var b strings.Builder
	for i, body := range bodies {
		fmt.Fprintf(&b, "## Section %d\n\n%s\n\n", i, body)
	}

	created := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	return vault.Note{
		Vault:      schema.VaultMalkaLife(),
		Path:       path,
		UID:        uuidFromPath(t, path),
		Type:       schema.NoteTypeConcept(),
		Status:     schema.StatusStable(),
		Visibility: schema.VisibilityInternal(),
		Tags:       []string{"golang", "architecture"},
		Created:    created,
		Title:      "A note",
		Lang:       "pt",
		Area:       schema.AreaResearch(),
		Sub:        "curadoria",
		Updated:    created.Add(48 * time.Hour),
		Body:       b.String(),
	}
}

func sectionBodies(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("body of section %d", i)
	}
	return out
}

// uuidFromPath derives a stable uid from the path so two fixtures with different paths never
// collide and the same fixture rebuilt mid-test keeps its identity.
func uuidFromPath(t *testing.T, path string) uuid.UUID {
	t.Helper()
	return uuid.NewSHA1(uuid.MustParse(schema.NamespaceKnowrag), []byte("test:"+path))
}

// chunksOf runs the real chunker over a note, because every expectation in these tests has to be
// built from the same chunks ProcessNote will build.
func chunksOf(t *testing.T, n vault.Note) []chunk.Chunk {
	t.Helper()
	chunks, err := chunk.ChunkNote(t.Context(), n, testChunkConfig(), chunk.FakeTokenCounter{})
	if err != nil {
		t.Fatalf("chunking %s: %v", n.Path, err)
	}
	return chunks
}

// expectedFor is the point set a note should have, as PointRecords — the shape the store hands back.
func expectedFor(t *testing.T, n vault.Note) []PointRecord {
	t.Helper()
	expected := ExpectedPoints(n, chunksOf(t, n), testTenant, NewPipelineConfig(testChunkConfig()), testHandshake())
	out := make([]PointRecord, len(expected))
	for i, e := range expected {
		out[i] = PointRecord{ChunkIndex: e.ChunkIndex, PointHash: e.PointHash, Fields: maps.Clone(e.Fields)}
	}
	return out
}
