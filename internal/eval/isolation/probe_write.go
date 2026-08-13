package isolation

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/danielmalka/go-knowrag/internal/chunk"
	"github.com/danielmalka/go-knowrag/internal/embed"
	"github.com/danielmalka/go-knowrag/internal/ingest"
	"github.com/danielmalka/go-knowrag/internal/schema"
	"github.com/danielmalka/go-knowrag/internal/vault"
)

// This file is probe.go's twin for the write path: the corpus an ingestion is run over, the hostile
// store it writes into, and the seam the suite's own tests swap.
//
// The read probe and this one are deliberately separate types rather than one store with two faces.
// They answer different interfaces — internal/retrieval's queryExecutor versus ingest.Store — and
// the hostility that makes each one useful is different: the read store applies only the conditions
// the request carries, this one keys points only by the scope the *call* carries.

// writeVault is the vault name the ingestion declares. Invented, like every other name in this
// package: the repository is public.
const writeVault = "vault-um"

// writeNotes is the corpus every tenant ingests, built from the same fixture the read cases search.
//
// Every tenant ingests the identical set — same uids, same text — which is the write-path form of
// the overlap probe.go describes, and it is what makes the case's assertions bite. Three tenants
// holding unrelated uids would end up with three disjoint point sets whatever the scoping did, so a
// prune or a read that had lost its tenant would still look correct; here every uid is contested,
// and losing the scope means one tenant reading or deleting a point another just wrote.
//
// The corpus spans all three collections' fixture points because the write path has no notion of a
// collection at all — *store.Client is bound to one at construction (internal/store/points.go) and
// internal/ingest never names one.
func writeNotes() []vault.Note {
	created := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	points := fixture()
	notes := make([]vault.Note, 0, len(points))
	for _, p := range points {
		notes = append(notes, vault.Note{
			Vault:      writeVault,
			Path:       p.uid + ".md",
			UID:        uuid.MustParse(p.uid),
			Type:       schema.NoteTypeConcept(),
			Status:     schema.StatusStable(),
			Visibility: schema.VisibilityInternal(),
			Tags:       []string{"renewal"},
			Created:    created,
			Updated:    created,
			Title:      "A note",
			Lang:       "pt",
			Area:       "research",
			Sub:        "curadoria",
			Body:       "## Section 0\n\n" + p.text + "\n",
		})
	}
	return notes
}

// writeCall is one call that reached the store during an ingestion.
//
// TenantID is the scope the call carried, which is a different fact from the tenant_id inside
// Points: internal/store/points.go builds the point ID from the argument and the payload from
// Fields, so the two are independent at this boundary and the write case asserts both.
type writeCall struct {
	Method   string
	TenantID string
	UID      uuid.UUID
	Points   []ingest.Point
}

// captureStore is a deliberately hostile stand-in for Qdrant on the write side.
//
// It is hostile in the one way that matters here: it keys points **only** by the scope the call
// carried, with no scoping of its own and no memory of who wrote what. It never reads tenant_id out
// of a payload to decide where a point belongs — a store that did would be enforcing the isolation
// this case is supposed to observe, and every assertion would pass because the mock was careful.
type captureStore struct {
	// calls is every call that reached the store, which is how the case proves what was asked rather
	// than inferring it from what came back.
	calls []writeCall
	// points is scope -> chunk_index -> record, where scope is produced by scopeKey below.
	points map[string]map[int]ingest.PointRecord

	// ignoreScope collapses every tenant into one: reads, writes and deletes key on the uid alone.
	// It is the write-path twin of probeStore.ignoreFilter — the store you would have if the tenant
	// half of ADR-006 §2's scope were gone. Nothing in the suite sets it;
	// cases_write_nonvacuity_test.go does, to drive the case into the failure it must detect.
	ignoreScope bool

	// recordAs rewrites the call this store logs, leaving what it does untouched. Same seam and same
	// reason as probeStore.recordAs: a payload naming another tenant, or a call that lost its scope,
	// changes nothing the store does here, so only an inspection of what was asked can notice.
	recordAs func(writeCall) writeCall
}

func newCaptureStore() *captureStore {
	return &captureStore{points: map[string]map[int]ingest.PointRecord{}}
}

func (s *captureStore) scopeKey(tenantID string, uid uuid.UUID) string {
	if s.ignoreScope {
		return uid.String()
	}
	return tenantID + "|" + uid.String()
}

func (s *captureStore) record(c writeCall) {
	if s.recordAs != nil {
		c = s.recordAs(c)
	}
	s.calls = append(s.calls, c)
}

func (s *captureStore) ScrollByUID(_ context.Context, tenantID string, uid uuid.UUID) ([]ingest.PointRecord, error) {
	s.record(writeCall{Method: "scroll", TenantID: tenantID, UID: uid})
	return slices.Collect(maps.Values(s.points[s.scopeKey(tenantID, uid)])), nil
}

// ScrollTenant puts the run on the production read path: *store.Client implements BulkScroller, so
// a run against a store that did not would be exercising the fallback rather than what ships
// (internal/ingest/prefetch.go).
func (s *captureStore) ScrollTenant(_ context.Context, tenantID string) (map[uuid.UUID][]ingest.PointRecord, error) {
	s.record(writeCall{Method: "scroll-tenant", TenantID: tenantID})

	out := map[uuid.UUID][]ingest.PointRecord{}
	for key, byIndex := range s.points {
		scope, rawUID, found := strings.Cut(key, "|")
		if !found {
			// ignoreScope: the key is a bare uid, so every tenant's points answer every snapshot.
			scope, rawUID = tenantID, key
		}
		if scope != tenantID {
			continue
		}
		uid, err := uuid.Parse(rawUID)
		if err != nil {
			return nil, fmt.Errorf("capture store: key %q does not hold a uid: %w", key, err)
		}
		out[uid] = slices.Collect(maps.Values(byIndex))
	}
	return out, nil
}

func (s *captureStore) UpsertPoints(
	_ context.Context,
	tenantID string,
	uid uuid.UUID,
	points []ingest.Point,
) (ingest.UpsertOutcome, error) {
	s.record(writeCall{Method: "upsert", TenantID: tenantID, UID: uid, Points: points})

	key := s.scopeKey(tenantID, uid)
	if s.points[key] == nil {
		s.points[key] = map[int]ingest.PointRecord{}
	}
	for _, p := range points {
		s.points[key][p.ChunkIndex] = ingest.PointRecord{
			ChunkIndex: p.ChunkIndex,
			PointHash:  p.PointHash,
			Fields:     maps.Clone(p.Fields),
		}
	}
	return ingest.UpsertConfirmed, nil
}

func (s *captureStore) DeleteByFilter(_ context.Context, tenantID string, uid uuid.UUID, from int) error {
	s.record(writeCall{Method: "delete", TenantID: tenantID, UID: uid})

	for idx := range s.points[s.scopeKey(tenantID, uid)] {
		if idx >= from {
			delete(s.points[s.scopeKey(tenantID, uid)], idx)
		}
	}
	return nil
}

// held is what the store holds for one scope, sorted by chunk_index.
func (s *captureStore) held(tenantID string, uid uuid.UUID) []ingest.PointRecord {
	out := slices.Collect(maps.Values(s.points[s.scopeKey(tenantID, uid)]))
	slices.SortFunc(out, func(a, b ingest.PointRecord) int { return a.ChunkIndex - b.ChunkIndex })
	return out
}

// writeRunner is the one thing the write case needs: something that ingests a note set under a
// tenant.
//
// It is an interface for the same reason caseSearcher is one, and the reason is the same shape:
// WritePathTenantCase asserts that an empty tenant_id is refused before any call reaches the store,
// and that refusal happens in Deps.Validate (internal/ingest/note.go) — no store, however hostile,
// can drive it red. A runner that accepts what production refuses is the only seam that can.
// TestDefaultSuite_UsesTheRealWriteProbe pins that the shipped runner is the production path.
type writeRunner interface {
	Ingest(ctx context.Context, tenantID string, notes []vault.Note) error
}

// productionIngest runs the real ingest.Orchestrate — the entry point cmd/cli/ingest.go calls.
//
// Orchestrate rather than RunBatch or ProcessNote, so the case covers what production covers: the
// cross-vault duplicate check, the vault scope derivation and the batch loop all run.
type productionIngest struct{ store ingest.Store }

func (r productionIngest) Ingest(ctx context.Context, tenantID string, notes []vault.Note) error {
	report, err := ingest.Orchestrate(ctx, writeDeps(tenantID, r.store),
		vault.ScanResult{Vault: writeVault, Notes: notes})
	if err != nil {
		return err
	}
	// A note that fails is a NoteResult carrying its reason rather than an error
	// (internal/ingest/note.go), so a run where every note failed returns nil here. Surfaced as an
	// error because the case would otherwise read that run as "wrote nothing" and blame the wrong
	// thing.
	for _, res := range report.Results {
		if res.Err != nil {
			return fmt.Errorf("note %s: %w", res.Path, res.Err)
		}
	}
	return nil
}

// writeDeps is the production configuration of one ingestion. Spelled once so the suite and its own
// tests cannot end up running different pipelines — the same reason newSearcherOver exists on the
// read side. Only the two ends are stubbed: a deterministic offline embedder (internal/embed's own
// fake) and the store above.
//
// The chunk bounds are wide open so no section is ever merged away: every note in writeNotes has one
// `##` section and therefore produces exactly one chunk, so "this tenant holds no point for that
// uid" is a fact about the scope rather than about a note the chunker happened to drop.
func writeDeps(tenantID string, store ingest.Store) ingest.Deps {
	return ingest.Deps{
		TenantID:  tenantID,
		Store:     store,
		Embedder:  embed.FakeEmbedder{},
		Handshake: writeHandshake(),
		Chunk:     chunk.Config{FloorTokens: 1, CeilingTokens: 1000},
		Tokens:    chunk.FakeTokenCounter{},
	}
}

// writeHandshake is a complete BackendHandshakeInfo. All seven fields are populated because
// ComputePointHash hashes all seven (internal/ingest/pointhash.go), and a fixture that left one at
// its zero value would put a hash nothing produces into the store.
func writeHandshake() embed.BackendHandshakeInfo {
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

// newWriteProbe builds the real ingestion over the hostile store above.
//
// It is a var for one reason: the suite's own tests swap it to prove the case fails when the write
// path stops scoping what it writes (cases_write_nonvacuity_test.go). Nothing in the shipped suite
// reassigns it, and TestDefaultSuite_UsesTheRealWriteProbe pins that.
var newWriteProbe = func() (writeRunner, *captureStore) {
	store := newCaptureStore()
	return productionIngest{store: store}, store
}
