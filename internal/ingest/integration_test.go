//go:build integration

package ingest_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/qdrant/go-client/qdrant"

	"github.com/danielmalka/go-knowrag/internal/chunk"
	"github.com/danielmalka/go-knowrag/internal/config"
	"github.com/danielmalka/go-knowrag/internal/embed"
	"github.com/danielmalka/go-knowrag/internal/ingest"
	"github.com/danielmalka/go-knowrag/internal/store"
	"github.com/danielmalka/go-knowrag/internal/vault"
)

// This file is T18 — the task S06a's criteria table pointed NFR-4 and NFR-5 at and nobody ever
// wrote (D-23). Both numbers were verified by hand on 2026-08-10 and neither had a gate, which is
// how NFR-5 could fail by 8× since the first run and only surface because somebody measured it for
// an unrelated reason.
//
// Unlike internal/store's container suite, neither test here creates its own Qdrant. NFR-4 and
// NFR-5 are statements about *this* corpus against *this* deployment: a fresh container holding
// fixtures would produce a number about a system nobody runs. So both read the deployment through
// internal/config — the same resolution `cli ingest` performs, not a second configuration path —
// and skip when it is absent.
//
// Both gates are wall clock, not CPU time, because that is what the PRD promises: how long an
// operator waits.
//
// The suite outlives `go test`'s default 10-minute timeout, so it needs `-timeout` above the NFR-4
// gate — see the Makefile. A run killed by the harness reports a panic, not a verdict.

const (
	// fullRunGate is NFR-4 (PRD §2.9): a full ingestion of both vaults in ≤ 30 min.
	//
	// It is the PRD's number verbatim. No margin is subtracted for a shared machine on purpose: the
	// gate is the contract, and a threshold quietly tightened below the published one is a red run
	// nobody can interpret.
	fullRunGate = 30 * time.Minute

	// noopGate is NFR-5: reingesting a corpus where nothing changed, in ≤ 60 s.
	noopGate = 60 * time.Second
)

// Measured on 2026-08-10, on the owner's desktop, over 734 notes across the two vaults:
//
//	full run, everything written   7m52s   against the 30 min gate   (3,8× headroom)
//	no-op reingestion             33,7 s   against the 60 s gate     (1,8× headroom)
//
// Both were measured by this file, after D-22 was paid. The same morning, before it, the full run
// took 12m54s and the no-op 403 s — the fix moved both, which is why the full-run figure recorded
// here is not the one the debt document quotes for that morning.
//
// The numbers exist so a future red run can be read instead of investigated from zero. A no-op at
// 70 s is a slower or busier machine — 33,7 s was itself measured with other work running, against
// 27,3 s on an idle one. A no-op at 400 s is D-22 coming back.
const (
	measuredFullRun = "7m52s over 734 notes on 2026-08-10, against 12m54s that morning before D-22 was paid"
	measuredNoop    = "33,7 s over 734 notes on 2026-08-10, against 403 s that morning before D-22 was paid"
)

// minCorpusNotes is the floor that keeps a green run from being a vacuous one. A gate that passed
// because the configured vault path held four notes would report health for a system nobody
// measured, which is worse than having no gate at all. It sits far below the 734 notes of
// 2026-08-10 so the corpus can grow, or lose a folder to the exclusion list, without going red for
// it.
const minCorpusNotes = 100

// throwawayTenantPrefix marks the tenant NFR-4 creates and deletes. deleteTenant refuses anything
// without it, so the only name this file can destroy is the one it invented.
const throwawayTenantPrefix = "nfr4-fullrun"

// throwawayTenant is deliberately a constant and not a unique name per run.
//
// The cleanup that removes it is a t.Cleanup, and t.Cleanup does not run when `go test -timeout`
// fires — the alarm panics from another goroutine and takes the process down — nor when an operator
// interrupts an eight-minute run with Ctrl-C, because the testing package installs no signal
// handler. Both are plausible here. With a unique name per run, every one of those interruptions
// would leave another tenant's worth of points in the collection the owner actually uses, growing
// without bound and with nothing to sweep them.
//
// A fixed name bounds that at one: the test deletes it before it starts as well as after it ends, so
// the debris of an interrupted run is cleaned by the next one instead of accumulating. The cost is
// that two of these tests cannot run against the same deployment at the same time, which is true of
// a single-operator gate anyway.
const throwawayTenant = throwawayTenantPrefix + "-scratch"

// These mirror cmd/cli/ingest.go verbatim, and have to keep mirroring it. The clamp bounds feed
// point_hash, so a run at other bounds reindexes everything and measures a different thing entirely;
// the embed profile decides how many chunks travel per request and how many requests are in flight,
// which is most of what NFR-4 measures. They are duplicated rather than imported because the command
// lives in package main and nothing can import it — that is the drift risk, and it is the reason
// this comment exists.
const (
	floorTokens    = 256
	ceilingTokens  = 1024
	upsertAttempts = 3
)

// TestIngestRealCorpus_FullRun is NFR-4: the whole corpus read, chunked, embedded and written,
// inside 30 minutes.
func TestIngestRealCorpus_FullRun(t *testing.T) {
	cfg := realDeployment(t)

	// A full run means every note gets written, and against the tenant the operator populates every
	// note is already integral — timing that would be a no-op wearing NFR-4's name. So the run targets
	// a throwaway tenant instead. tenant_id is part of the point ID (schema.PointID), so nothing
	// written here can collide with the real points.
	//
	// It is emptied before as well as after. Starting empty is what makes every note a write, and the
	// debris of a previous run that a timeout or a Ctrl-C prevented from cleaning up is exactly what
	// would leave some of them integral — see throwawayTenant for why that debris is expected rather
	// than hypothetical.
	tenant := throwawayTenant
	deleteTenant(t, cfg, tenant)
	t.Cleanup(func() { deleteTenant(t, cfg, tenant) })

	report, elapsed := ingestOnce(t, cfg, tenant)
	assertRealCorpus(t, report)

	// On a tenant that started empty, a note with chunks cannot be integral, so it is written and then
	// pruned. A note that chunks to *nothing* — valid frontmatter over an empty or whitespace-only
	// body — expects no points at all, and the integrity check is then satisfied by vacuity (no
	// record is missing from an empty set, and the counts match at zero), so it skips even here.
	//
	// That is correct behaviour and not a failed write, and the assertion has to tell the two apart.
	// The corpus has no such note today, which is exactly why this matters: written as a flat
	// `pruned == total`, the first stub note somebody starts and does not finish turns NFR-4 red with
	// a message about the write path.
	written, emptyBody := 0, 0
	for _, r := range report.Results {
		switch {
		case r.Chunks == 0 && r.State == ingest.StateSkipped:
			emptyBody++
		case r.Chunks > 0 && r.State == ingest.StatePruned:
			written++
		default:
			t.Fatalf("%s produced %d chunk(s) and ended in state %s, which a first run over an empty "+
				"tenant cannot produce:\n%s", r.Path, r.Chunks, r.State, report)
		}
	}
	if written < minCorpusNotes {
		t.Fatalf("only %d note(s) were actually written (%d had empty bodies), below the %d this gate "+
			"treats as a real corpus — its duration is not a full ingestion:\n%s",
			written, emptyBody, minCorpusNotes, report)
	}

	// The report is this process's account of what it did; the count is the server's. They disagree
	// exactly when the writes went somewhere else — a wrong collection, a wrong tenant — which is
	// the failure mode that otherwise passes every assertion above.
	points := countTenantPoints(t, cfg, tenant)
	if points == 0 {
		t.Fatalf("the run reported %d note(s) written but %s holds no point at all for tenant %s",
			len(report.Results), cfg.DefaultCollection, tenant)
	}

	t.Logf("NFR-4: %d note(s), %d point(s), %s — gate %s, measured %s",
		len(report.Results), points, elapsed.Round(time.Second), fullRunGate, measuredFullRun)
	if elapsed > fullRunGate {
		t.Errorf("NFR-4: full ingestion of %d note(s) took %s, gate is %s (%s)",
			len(report.Results), elapsed.Round(time.Second), fullRunGate, measuredFullRun)
	}
}

// TestIngestRealCorpus_IncrementalNoop is NFR-5: reingesting a corpus where nothing changed, inside
// 60 seconds.
func TestIngestRealCorpus_IncrementalNoop(t *testing.T) {
	cfg := realDeployment(t)
	tenant := realTenant()

	// The first run is not the measurement, and it must not be. NFR-5 is "nothing changed", and a
	// run timed straight after notes were edited measures the work of ingesting those edits — the
	// number would be honest about something the NFR does not ask about. This run exists only to
	// converge the tenant with what is on disk. It writes exactly what `cli ingest` would have
	// written anyway, and the run below, by construction, has nothing left to do.
	converge, convergeElapsed := ingestOnce(t, cfg, tenant)
	if converge.Failed() {
		t.Fatalf("the converging run did not complete, so the run after it is not a no-op:\n%s", converge)
	}
	t.Logf("converged in %s (not the measurement): %v", convergeElapsed.Round(time.Second), converge.Counts())

	report, elapsed := ingestOnce(t, cfg, tenant)
	assertRealCorpus(t, report)

	// StateSkipped is the only state a no-op may produce: it is the short-circuit taken when the
	// integrity predicate finds the uid intact, which means nothing was embedded and nothing was
	// written. One note in any other state and this is a run that did work, timed against a gate for
	// a run that does none.
	if skipped, total := report.Count(ingest.StateSkipped), len(report.Results); skipped != total {
		t.Fatalf("only %d of %d note(s) were skipped, so this run wrote and is not the no-op NFR-5 "+
			"measures:\n%s", skipped, total, report)
	}

	t.Logf("NFR-5: %d note(s), all skipped, %s — gate %s, measured %s",
		len(report.Results), elapsed.Round(100*time.Millisecond), noopGate, measuredNoop)
	if elapsed > noopGate {
		t.Errorf("NFR-5: reingestion with nothing changed over %d note(s) took %s, gate is %s (%s)",
			len(report.Results), elapsed.Round(100*time.Millisecond), noopGate, measuredNoop)
	}
}

// ingestOnce runs what `cli ingest` runs, and returns the report with the wall clock it took.
//
// The timed region opens at the vault scan and closes when Orchestrate returns. That boundary is
// NFR-5's "comando completo do operador" minus flag parsing, and the scan is inside it because it
// has to be: scanning both vaults alone was ~13 s of the 27,3 s measured, so a timer started after
// it would report roughly half of what the operator waits. Connecting to Qdrant and the embedder
// handshake are in there for the same reason — they happen on every real run.
//
// The wiring is duplicated from cmd/cli/ingest.go rather than called, because that code lives in
// package main. What it must never do is diverge from it: see the mirrored constants above.
func ingestOnce(t *testing.T, cfg *config.Config, tenant string, tweaks ...func(*ingest.Deps)) (ingest.Report, time.Duration) {
	t.Helper()
	start := time.Now()

	names := cfg.VaultNames()
	scans := make([]vault.ScanResult, 0, len(names))
	for _, name := range names {
		settings := cfg.Vaults[name]
		scan, err := vault.ScanVault(settings.Path, name, settings.AreaNames(), vault.Exclusions{
			Folders:   settings.Folders(),
			RootFiles: settings.RootFiles(),
		})
		if err != nil {
			t.Fatalf("scanning vault %s at %s: %v", name, settings.Path, err)
		}
		scans = append(scans, scan)
	}

	// runIngest refuses to write when a uid repeats across the two vaults, because the point ID does
	// not include the vault and the second upsert would overwrite the first in silence. Mirroring the
	// timing without mirroring that check would let this gate report a clean pass over a corpus the
	// real command declines to ingest at all.
	for i := range scans {
		for j := i + 1; j < len(scans); j++ {
			if err := vault.CheckCrossVaultDuplicateUIDs(scans[i], scans[j]); err != nil {
				t.Fatalf("vaults %s and %s: %v", scans[i].Vault, scans[j].Vault, err)
			}
		}
	}

	tokenizer, err := chunk.NewHTTPTokenCounter(cfg.EmbedderEndpoint)
	if err != nil {
		t.Fatalf("building the token counter: %v", err)
	}
	tokens := chunk.NewCountingTokenCounter(tokenizer)

	points, err := store.NewClient(qdrantClient(t, cfg), cfg.DefaultCollection)
	if err != nil {
		t.Fatalf("store.NewClient: %v", err)
	}

	transport, err := embed.NewHTTPTransport(cfg.EmbedderEndpoint)
	if err != nil {
		t.Fatalf("building the embedding transport: %v", err)
	}
	embedder, err := embed.NewServiceEmbedder(embed.Profile{
		Endpoint:      cfg.EmbedderEndpoint,
		Timeout:       2 * time.Minute,
		VerifyTimeout: 30 * time.Second,
		BatchSize:     32,
		MaxConcurrent: 2,
		MaxRetries:    3,
	}, transport)
	if err != nil {
		t.Fatalf("building the embedder: %v", err)
	}

	// The handshake feeds point_hash and must carry what the backend confirmed. It is inside the
	// timed region because the operator pays for it too.
	handshake, err := embedder.Handshake(t.Context())
	if err != nil {
		t.Fatalf("embedder handshake: %v", err)
	}

	deps := ingest.Deps{
		TenantID:       tenant,
		Store:          points,
		Embedder:       embedder,
		Handshake:      handshake,
		Chunk:          chunk.Config{FloorTokens: floorTokens, CeilingTokens: ceilingTokens},
		Tokens:         tokens,
		UpsertAttempts: upsertAttempts,
	}
	for _, tweak := range tweaks {
		tweak(&deps)
	}

	report, err := ingest.Orchestrate(t.Context(), deps, scans...)
	if err != nil {
		t.Fatalf("ingest.Orchestrate: %v", err)
	}
	elapsed := time.Since(start)

	t.Logf("%s", report)
	t.Logf("%s", tokens.Snapshot())
	return report, elapsed
}

// realDeployment resolves the deployment through internal/config and skips unless everything the
// write path needs is present. Skipping rather than failing is the correct default: a laptop with
// docker still runs the container half of `make test-integration`, and these two tests are the ones
// that touch the collection the owner's data lives in.
func realDeployment(t *testing.T) *config.Config {
	t.Helper()

	cfg, err := config.Load()
	if err != nil {
		t.Skipf("the configuration could not be loaded, so there is no deployment to measure: %v", err)
	}

	// Exactly the needs `cli ingest` declares on its write path, computed the same way. A hand-written
	// list here would drift from the command whose timing this file claims to be measuring.
	need := config.NeedQdrant | config.NeedCollection | config.NeedEmbedder
	if err := errors.Join(cfg.Require(need), cfg.RequireVaults(cfg.VaultNames()...)); err != nil {
		t.Skipf("NFR-4 and NFR-5 measure the real deployment against the real vaults: %v", err)
	}
	return cfg
}

// realTenant is the tenant the operator populates — cmd/cli/ingest.go's defaultTenantID, overridable
// through the same variable internal/retrieval's real-corpus test reads, so a deployment writing
// some other tenant can still run the gate.
func realTenant() string {
	if t := os.Getenv("KNOWRAG_TENANT_ID"); t != "" {
		return t
	}
	return "interno"
}

func qdrantClient(t *testing.T, cfg *config.Config) *qdrant.Client {
	t.Helper()

	client, err := store.NewQdrantClient(store.Config{
		Endpoint: cfg.QdrantEndpoint,
		APIKey:   cfg.QdrantAPIKey,
	})
	if err != nil {
		t.Fatalf("connecting to the deployed Qdrant: %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("closing the Qdrant client: %v", err)
		}
	})
	return client
}

// countTenantPoints asks the server how many points the tenant holds, exactly rather than estimated:
// an approximate count is a segment-level guess, and "did the writes land" is not a question to
// answer with a guess.
func countTenantPoints(t *testing.T, cfg *config.Config, tenant string) uint64 {
	t.Helper()

	n, err := qdrantClient(t, cfg).Count(t.Context(), &qdrant.CountPoints{
		CollectionName: cfg.DefaultCollection,
		Filter:         &qdrant.Filter{Must: []*qdrant.Condition{qdrant.NewMatchKeyword("tenant_id", tenant)}},
		Exact:          qdrant.PtrOf(true),
	})
	if err != nil {
		t.Fatalf("counting the points of tenant %s in %s: %v", tenant, cfg.DefaultCollection, err)
	}
	return n
}

// deleteTenant removes everything NFR-4's run wrote.
//
// It builds its own client and its own context because it runs from t.Cleanup, after the test's
// context has already been cancelled — a cleanup inheriting that context leaves behind the points it
// meant to remove, under a tenant nobody will ever think to look for.
//
// One filtered delete rather than one per uid: tenant_id is the is_tenant keyword index of every
// collection in the manifest, so strict mode accepts the filter and the server does the walk.
func deleteTenant(t *testing.T, cfg *config.Config, tenant string) {
	t.Helper()

	// This function issues a filtered delete against the collection the owner's data lives in, and
	// the filter is a string built by its caller. Today that string cannot be the real tenant; the
	// guard is here for the edit that makes it possible, because the failure it prevents is silent
	// destruction of the corpus and the cost of preventing it is one comparison.
	if !strings.HasPrefix(tenant, throwawayTenantPrefix) {
		t.Fatalf("refusing to delete tenant %q: this removes every point of a tenant and may only be "+
			"pointed at a throwaway one, whose name starts with %q", tenant, throwawayTenantPrefix)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	client, err := store.NewQdrantClient(store.Config{
		Endpoint: cfg.QdrantEndpoint,
		APIKey:   cfg.QdrantAPIKey,
	})
	if err != nil {
		t.Errorf("connecting to Qdrant to remove the throwaway tenant %s: %v", tenant, err)
		return
	}
	defer func() { _ = client.Close() }()

	if _, err := client.Delete(ctx, &qdrant.DeletePoints{
		CollectionName: cfg.DefaultCollection,
		Points: qdrant.NewPointsSelectorFilter(&qdrant.Filter{
			Must: []*qdrant.Condition{qdrant.NewMatchKeyword("tenant_id", tenant)},
		}),
		Wait: qdrant.PtrOf(true),
	}); err != nil {
		t.Errorf("removing the throwaway tenant %s from %s — its points are still there: %v",
			tenant, cfg.DefaultCollection, err)
	}
}

// assertRealCorpus fails a run that measured nothing, before any duration is read. D-17's standard
// is that a test passing identically with and without the thing it protects proves nothing, and a
// timing gate has two ways to reach that state: skipping, and measuring an empty corpus. The skip is
// visible in the output; this is the other one.
func assertRealCorpus(t *testing.T, report ingest.Report) {
	t.Helper()

	if n := len(report.Results); n < minCorpusNotes {
		t.Fatalf("the run covered %d note(s), below the %d this gate treats as a real corpus — check "+
			"the configured vault paths before reading any duration", n, minCorpusNotes)
	}
	if report.Failed() {
		t.Fatalf("the run did not complete, so its duration is not the NFR's:\n%s", report)
	}
}

// TestIngestRealCorpus_DryRunWritesNothing is T4's acceptance criterion, asserted the way it is
// written: the number of points in Qdrant is identical before and after.
//
// It runs against the tenant the operator actually populates, and that is the point — a dry run over
// a throwaway empty tenant would report every note as work to do and write nothing for the trivial
// reason that there was nothing there. Over the real tenant the run reaches the integrity check for
// all ~730 notes, decides what each one would become, and still must not touch a single point.
//
// Counting is a Count call rather than a scroll because the assertion is a number, and because a
// scroll that paginated wrong would make this test fail for its own reasons.
func TestIngestRealCorpus_DryRunWritesNothing(t *testing.T) {
	cfg := realDeployment(t)
	tenant := realTenant()

	before := countTenantPoints(t, cfg, tenant)
	if before == 0 {
		t.Skipf("tenant %s holds no points; a before/after count over an empty tenant proves nothing",
			tenant)
	}

	report, _ := ingestOnce(t, cfg, tenant, func(d *ingest.Deps) { d.DryRun = true })

	if after := countTenantPoints(t, cfg, tenant); after != before {
		t.Errorf("point count went from %d to %d across a dry run; --dry-run must write nothing",
			before, after)
	}
	// Not a vacuous pass: the run has to have reached the per-note decision for the whole corpus.
	if n := len(report.Results); n < minCorpusNotes {
		t.Errorf("the dry run evaluated %d note(s), want at least %d", n, minCorpusNotes)
	}
	if !report.OrphansScanned {
		t.Error("the dry run did not scan for orphans; reading the index is what it gained in A-ii")
	}
}
