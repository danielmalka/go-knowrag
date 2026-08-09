package ingest

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/danielmalka/go-knowrag/internal/embed"
	"github.com/danielmalka/go-knowrag/internal/schema"
	"github.com/danielmalka/go-knowrag/internal/vault"
)

// harness is a store, an embedder and the journal they share, wired into Deps.
type harness struct {
	journal *journal
	store   *fakeStore
	embed   *spyEmbedder
	deps    Deps
}

func newHarness() *harness {
	j := &journal{}
	store := newFakeStore(j)
	emb := newSpyEmbedder(j)
	return &harness{journal: j, store: store, embed: emb, deps: testDeps(store, emb)}
}

// reset clears the call log without clearing the stored points, which is what "a second run over
// the same index" means.
func (h *harness) reset() {
	h.journal.calls = nil
	h.embed.calls = 0
	h.embed.texts = nil
	h.store.upserts = nil
	h.store.deletes = nil
}

// ingestOnce runs one note through and fails the test unless it completed — the setup step of every
// "and then something changed" scenario.
func (h *harness) ingestOnce(t *testing.T, n vault.Note) NoteResult {
	t.Helper()
	res := ProcessNote(t.Context(), h.deps, n)
	if res.State != StatePruned {
		t.Fatalf("setting up: %s ended %s, want %s (%v)", n.Path, res.State, StatePruned, res.Err)
	}
	return res
}

// assertReembedded is the "this note went the full path" assertion.
//
// It checks StatePruned rather than StateEmbedded, and that is not a weakening: StateEmbedded is
// where a note *stops* when the run dies between embedding and a confirmed upsert (T9). A note that
// reembeds and completes passes through it and ends StatePruned, so asserting StateEmbedded here
// would be asserting a crash.
func assertReembedded(t *testing.T, h *harness, res NoteResult) {
	t.Helper()
	if res.State != StatePruned {
		t.Fatalf("State = %s, want %s (%v)", res.State, StatePruned, res.Err)
	}
	if h.embed.calls != 1 {
		t.Fatalf("the embedder was called %d time(s), want exactly 1 — the note did not reembed",
			h.embed.calls)
	}
}

// TestProcessNote_Skipped_WhenIntegral is the short-circuit: a fully integral note costs one read
// and nothing else.
func TestProcessNote_Skipped_WhenIntegral(t *testing.T) {
	h := newHarness()
	n := testNote(t, "research/curadoria/nota.md", 3)
	h.ingestOnce(t, n)
	h.reset()

	res := ProcessNote(t.Context(), h.deps, n)
	if res.State != StateSkipped {
		t.Fatalf("State = %s, want %s (%v)", res.State, StateSkipped, res.Err)
	}
	if h.embed.calls != 0 {
		t.Errorf("the embedder was called %d time(s) for an unchanged note", h.embed.calls)
	}
	if got := h.journal.calls; !slices.Equal(got, []string{"scroll"}) {
		t.Errorf("calls = %v, want exactly one scroll", got)
	}
}

// TestProcessNote_MtimeOnlyChange_SkipsWithZeroWrites is the end-to-end payoff of the owner's
// 2026-08-08 decision.
//
// The note's bytes are identical and only its git/mtime-derived `updated` moved — a git checkout, a
// rebase, a restored backup, an editor that rewrites on save. Before the decision this re-embedded
// the note. Asserting *zero writes*, not just zero embeds, is the load-bearing half: a payload
// rewrite "just to refresh `updated`" would be the removed payload_only branch under a new name,
// and the accepted cost is precisely that the stored `updated` goes stale (PRD-contrato §2.4).
func TestProcessNote_MtimeOnlyChange_SkipsWithZeroWrites(t *testing.T) {
	h := newHarness()
	n := testNote(t, "research/curadoria/nota.md", 3)
	h.ingestOnce(t, n)
	h.reset()

	touched := n
	touched.Updated = n.Updated.Add(30 * 24 * time.Hour)

	res := ProcessNote(t.Context(), h.deps, touched)
	if res.State != StateSkipped {
		t.Fatalf("State = %s, want %s (%v)", res.State, StateSkipped, res.Err)
	}
	if h.embed.calls != 0 {
		t.Errorf("the embedder was called %d time(s) for a note whose bytes did not change", h.embed.calls)
	}
	if len(h.store.upserts) != 0 || len(h.store.deletes) != 0 {
		t.Errorf("%d upsert(s) and %d delete(s) for an mtime-only change; the stored `updated` is "+
			"knowingly left stale and no code path may refresh it",
			len(h.store.upserts), len(h.store.deletes))
	}

	// And the drift is real: the index still holds the old value, undetected, as designed.
	rec, ok := h.store.record(testTenant, n.UID, 0)
	if !ok {
		t.Fatal("point 0 disappeared")
	}
	if got := rec.Fields[fieldUpdated]; got != n.Updated.UTC().Format(time.RFC3339) {
		t.Errorf("stored `updated` = %v; it should still carry the pre-touch value", got)
	}
}

// TestProcessNote_Embedded_DefaultCase pins the ordering the whole design rests on: every chunk is
// embedded before anything is written, and the prune fires strictly after the upsert.
func TestProcessNote_Embedded_DefaultCase(t *testing.T) {
	h := newHarness()
	n := testNote(t, "research/curadoria/nota.md", 3)
	h.ingestOnce(t, n)
	h.reset()

	edited := n
	edited.Body = noteWithBodies(t, n.Path, "rewritten body", "body of section 1", "body of section 2").Body

	res := ProcessNote(t.Context(), h.deps, edited)
	assertReembedded(t, h, res)

	if got := h.journal.calls; !slices.Equal(got, []string{"scroll", "embed", "upsert", "delete"}) {
		t.Fatalf("calls = %v, want scroll → embed → upsert → delete", got)
	}
	if got, want := len(h.embed.texts[0]), 3; got != want {
		t.Errorf("the embedder was asked for %d text(s), want all %d chunks in one call", got, want)
	}
}

// TestProcessNote_MetadataOnlyChange_StillEmbeds is the field-by-field table PRD-contrato §2.4
// requires: each of S02's eleven note-metadata fields, changed alone, reembeds the note.
//
// `status: stable → archived` and `visibility: internal → private` are ordinary rows here. They used
// to have a cheaper path — the removed payload_only branch — and the correctness they protected is
// now preserved by reembedding like everything else.
//
// `updated` is deliberately absent from this table. It gets the opposite assertion in
// TestProcessNote_MtimeOnlyChange_SkipsWithZeroWrites, and the two together pin the decision from
// both sides.
func TestProcessNote_MetadataOnlyChange_StillEmbeds(t *testing.T) {
	cases := []struct {
		field  string
		change func(n *vault.Note)
	}{
		{fieldVault, func(n *vault.Note) { n.Vault = schema.VaultMalkaWay() }},
		{fieldPath, func(n *vault.Note) { n.Path = "research/curadoria/renomeada.md" }},
		{fieldType, func(n *vault.Note) { n.Type = schema.NoteTypeReference() }},
		{fieldStatus, func(n *vault.Note) { n.Status = schema.StatusArchived() }},
		{fieldVisibility, func(n *vault.Note) { n.Visibility = schema.VisibilityPrivate() }},
		{fieldTags, func(n *vault.Note) { n.Tags = []string{"golang", "qdrant"} }},
		{fieldTitle, func(n *vault.Note) { n.Title = "Outro título" }},
		{fieldLang, func(n *vault.Note) { n.Lang = "en" }},
		{fieldCreated, func(n *vault.Note) { n.Created = n.Created.Add(24 * time.Hour) }},
		{fieldArea, func(n *vault.Note) { n.Area = schema.AreaPersonal() }},
		{fieldSub, func(n *vault.Note) { n.Sub = "outra-sub" }},
	}

	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			h := newHarness()
			n := testNote(t, "research/curadoria/nota.md", 3)
			h.ingestOnce(t, n)
			h.reset()

			changed := n
			tc.change(&changed)

			assertReembedded(t, h, ProcessNote(t.Context(), h.deps, changed))

			// The new value really reached the index — the point of reembedding rather than
			// short-circuiting is that the payload follows the note.
			rec, ok := h.store.record(testTenant, n.UID, 0)
			if !ok {
				t.Fatal("point 0 is missing after the reingest")
			}
			want := PayloadFields(changed, chunksOf(t, changed)[0], testTenant, testHandshake().ModelRevision)
			if field, equal := equalFields(rec.Fields, want); !equal {
				t.Errorf("stored payload still disagrees on %q after the reingest", field)
			}
		})
	}
}

// TestProcessNote_PipelineOrEmbedderConfigChange_StillEmbeds proves ProcessNote reacts to a config
// change with the same full reembed it gives a content change — the note's bytes never moved.
//
// Representative rather than exhaustive: the per-component coverage lives in
// TestComputePointHash_ChangesPerComponent, and what is being proven here is only that ProcessNote
// has no branch that treats one category of divergence as cheaper.
func TestProcessNote_PipelineOrEmbedderConfigChange_StillEmbeds(t *testing.T) {
	t.Run("pipeline config", func(t *testing.T) {
		h := newHarness()
		n := testNote(t, "research/curadoria/nota.md", 3)
		h.ingestOnce(t, n)
		h.reset()

		// A ceiling nothing is near: the chunk boundaries are identical, only the hashed config moved.
		h.deps.Chunk.CeilingTokens = 999
		assertReembedded(t, h, ProcessNote(t.Context(), h.deps, n))
	})

	t.Run("embedder config", func(t *testing.T) {
		h := newHarness()
		n := testNote(t, "research/curadoria/nota.md", 3)
		h.ingestOnce(t, n)
		h.reset()

		h.deps.Handshake.Precision = "fp32"
		assertReembedded(t, h, ProcessNote(t.Context(), h.deps, n))
	})
}

// TestProcessNote_InsertThenPrune is ADR-006's ordering rule, both halves.
func TestProcessNote_InsertThenPrune(t *testing.T) {
	t.Run("confirmed upsert is followed by a scoped prune", func(t *testing.T) {
		h := newHarness()
		n := testNote(t, "research/curadoria/nota.md", 3)

		res := ProcessNote(t.Context(), h.deps, n)
		if res.State != StatePruned {
			t.Fatalf("State = %s, want %s (%v)", res.State, StatePruned, res.Err)
		}
		if up, del := h.journal.indexOf("upsert"), h.journal.indexOf("delete"); del < up {
			t.Fatalf("delete at %d came before upsert at %d (%v)", del, up, h.journal.calls)
		}
		if len(h.store.deletes) != 1 {
			t.Fatalf("%d delete(s), want 1", len(h.store.deletes))
		}
		got := h.store.deletes[0]
		want := deleteCall{TenantID: testTenant, UID: n.UID, FromChunkIndex: 3}
		if got != want {
			t.Errorf("delete scope = %+v, want %+v — pruning by anything less than "+
				"tenant_id + uid + chunk_index >= N destroys the neighbour tenant's points (ADR-006 §2)",
				got, want)
		}
	})

	for _, tc := range []struct {
		name    string
		outcome UpsertOutcome
	}{
		{"an ambiguous upsert never advances to the prune", UpsertAmbiguous},
		{"a failed upsert never advances to the prune", UpsertFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness()
			h.store.upsertHook = func(_ int, points []Point) (int, UpsertOutcome, error) {
				return len(points), tc.outcome, errors.New("qdrant said no")
			}
			n := testNote(t, "research/curadoria/nota.md", 3)

			res := ProcessNote(t.Context(), h.deps, n)
			if res.State != StateFailed {
				t.Fatalf("State = %s, want %s", res.State, StateFailed)
			}
			if len(h.store.deletes) != 0 {
				t.Fatalf("%d delete(s) after an unconfirmed upsert; pruning on top of a write nobody "+
					"confirmed is how a note loses points it still needs (ADR-006 §2)", len(h.store.deletes))
			}
		})
	}
}

// TestProcessNote_EmbedderFailure_WritesNothing: the embedder is upstream of every write, so its
// failure leaves the index byte-identical.
func TestProcessNote_EmbedderFailure_WritesNothing(t *testing.T) {
	h := newHarness()
	h.embed.err = errors.New("backend is down")
	n := testNote(t, "research/curadoria/nota.md", 3)

	res := ProcessNote(t.Context(), h.deps, n)
	if res.State != StateFailed {
		t.Fatalf("State = %s, want %s", res.State, StateFailed)
	}
	if len(h.store.upserts) != 0 || len(h.store.deletes) != 0 {
		t.Errorf("%d upsert(s), %d delete(s) after an embedder failure",
			len(h.store.upserts), len(h.store.deletes))
	}
}

// TestProcessNote_UpsertRetryIsBounded: an unreachable store must fail the note, not hang the run.
func TestProcessNote_UpsertRetryIsBounded(t *testing.T) {
	h := newHarness()
	h.deps.UpsertAttempts = 3
	attempts := 0
	h.store.upsertHook = func(_ int, _ []Point) (int, UpsertOutcome, error) {
		attempts++
		return 0, UpsertAmbiguous, errors.New("connection refused")
	}

	res := ProcessNote(t.Context(), h.deps, testNote(t, "research/curadoria/nota.md", 2))
	if res.State != StateFailed {
		t.Fatalf("State = %s, want %s", res.State, StateFailed)
	}
	if attempts != 3 {
		t.Errorf("the store was called %d time(s), want the configured budget of 3", attempts)
	}
	if len(h.store.deletes) != 0 {
		t.Errorf("%d prune(s) after every attempt failed", len(h.store.deletes))
	}
}

// TestProcessNote_EmbedderCountMismatch_WritesNothing: a backend that returns fewer embeddings than
// texts would otherwise pair chunk i's payload with chunk j's vector — a corruption point_hash
// cannot catch, since it covers the text and not the vector.
func TestProcessNote_EmbedderCountMismatch_WritesNothing(t *testing.T) {
	h := newHarness()
	h.deps.Embedder = shortEmbedder{h.embed}

	res := ProcessNote(t.Context(), h.deps, testNote(t, "research/curadoria/nota.md", 3))
	if res.State != StateFailed {
		t.Fatalf("State = %s, want %s", res.State, StateFailed)
	}
	if len(h.store.upserts) != 0 {
		t.Errorf("%d upsert(s) after a truncated embedder response", len(h.store.upserts))
	}
}

// TestPayloadFields_CarriesEveryContractField pins the §2.4 payload key set. A field silently
// dropped here is a field nothing writes and, since it is also in the hash, a note that reprocesses
// forever without converging.
func TestPayloadFields_CarriesEveryContractField(t *testing.T) {
	n := testNote(t, "research/curadoria/nota.md", 2)
	fields := PayloadFields(n, chunksOf(t, n)[0], testTenant, testHandshake().ModelRevision)

	want := []string{
		fieldTenantID, fieldUID, fieldChunkIndex, fieldText, fieldVault, fieldArea, fieldSub,
		fieldPath, fieldType, fieldStatus, fieldVisibility, fieldTags, fieldTitle, fieldHeadings,
		fieldLang, fieldCreated, fieldUpdated, fieldOversize, fieldEmbeddingModel,
	}
	for _, k := range want {
		if _, ok := fields[k]; !ok {
			t.Errorf("payload is missing %q (PRD-contrato §2.4)", k)
		}
	}
	if got := slices.Sorted(maps.Keys(fields)); len(got) != len(want) {
		t.Errorf("payload has %d field(s) (%v), want exactly the %d of §2.4 minus point_hash",
			len(got), got, len(want))
	}
	if _, ok := fields[fieldPointHash]; ok {
		t.Error("point_hash is inside the field map; it must travel separately so condition 4 " +
			"cannot accidentally compare it")
	}
}

// shortEmbedder returns one embedding fewer than it was asked for.
type shortEmbedder struct{ inner *spyEmbedder }

func (s shortEmbedder) ModelID() string { return s.inner.ModelID() }

func (s shortEmbedder) EmbedQuery(ctx context.Context, text string) (embed.Embedding, error) {
	return s.inner.EmbedQuery(ctx, text)
}

func (s shortEmbedder) EmbedDocuments(ctx context.Context, texts []string) ([]embed.Embedding, error) {
	out, err := s.inner.EmbedDocuments(ctx, texts)
	if err != nil || len(out) == 0 {
		return out, err
	}
	return out[:len(out)-1], nil
}
