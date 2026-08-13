// Command measure-ingest is the two-minute NFR-5 command: it runs one real, complete incremental
// `cli ingest` (lock acquisition, vault scan, embed/write/prune-detection) against the deployed
// Qdrant and embedding service, times the whole thing end to end, and reports the decomposition
// (lock/scan/orchestrate) an operator needs to know where the time went if the 60s gate is missed
// (PRD.md's NFR-5 row: "reingestão incremental ... medida no comando completo do operador ...
// <= 60 s").
//
// It calls the same production packages cmd/cli/ingest.go's runIngest and ingestScans call, in the
// same order, with the same defaults (no --dry-run, no --prune, no --only, no --full) — a real
// no-op reingestion is what NFR-5 measures. The sequencing here is a small, deliberate duplicate of
// that file's glue rather than an import from cmd/cli, so this tool and cmd/cli/ingest.go can each
// change without touching the other; if cmd/cli/ingest.go's phase order changes, update this file's
// comment and internal/measure/ingest.go's IngestPhases comment too.
//
// It never runs against real infrastructure on its own — an operator runs it, pointed at their own
// deployment and vault(s) via the usual KNOWRAG_* / QDRANT_* / EMBEDDER_ENDPOINT environment
// variables this repository already documents (deploy/.env.example).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/danielmalka/go-knowrag/internal/chunk"
	"github.com/danielmalka/go-knowrag/internal/config"
	"github.com/danielmalka/go-knowrag/internal/embed"
	"github.com/danielmalka/go-knowrag/internal/ingest"
	"github.com/danielmalka/go-knowrag/internal/ingest/lock"
	"github.com/danielmalka/go-knowrag/internal/measure"
	"github.com/danielmalka/go-knowrag/internal/store"
	"github.com/danielmalka/go-knowrag/internal/vault"
)

// nfr5Gate is NFR-5's budget. PRD.md's NFR-5 row is where the number lives; repeated here as a
// literal because this binary has no dependency on docs/ (gitignored, CLAUDE.md) — if PRD.md's gate
// changes, this line is where to update it too.
const nfr5Gate = 60 * time.Second

// defaultTenantID matches cmd/cli/ingest.go's defaultTenantID: the tenant the CLI populates by
// default, since the CLI is privileged and may write any tenant (ADR-002 §2.4).
const defaultTenantID = "interno"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("measure-ingest", flag.ContinueOnError)
	fs.SetOutput(stderr)
	vaultFlag := fs.String("vault", "", "which rostered vault to ingest; empty means every "+
		"vault in KNOWRAG_VAULTS")
	tenant := fs.String("tenant", defaultTenantID, "tenant_id to write under and lock the scope by")
	out := fs.String("out", "", "path to write the durable JSON report to; defaults to "+
		"./out/measure-ingest-<unix-timestamp>.json")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "usage: measure-ingest [--vault name] [--tenant id] [--out path]\n\n"+
			"Measures NFR-5: a real, complete incremental `cli ingest` run's total wall time and its\n"+
			"lock/scan/orchestrate decomposition, against the real deployment named by QDRANT_ENDPOINT\n"+
			"/ KNOWRAG_ADMIN_QDRANT_API_KEY / EMBEDDER_ENDPOINT / DEFAULT_COLLECTION / KNOWRAG_VAULTS.\n"+
			"For a meaningful NFR-5 number, run it once first so the corpus is fully indexed, then run\n"+
			"it again — that second run is the no-op reingestion the gate is about.\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "measure-ingest: loading config: %v\n", err)
		return 1
	}
	names := []string{*vaultFlag}
	if *vaultFlag == "" {
		names = cfg.VaultNames()
	}
	if err := cfg.Require(config.NeedEmbedder | config.NeedQdrant | config.NeedCollection); err != nil {
		_, _ = fmt.Fprintf(stderr, "measure-ingest: %v\n", err)
		return 1
	}
	if err := cfg.RequireVaults(names...); err != nil {
		_, _ = fmt.Fprintf(stderr, "measure-ingest: %v\n", err)
		return 1
	}

	ctx := context.Background()
	start := time.Now()

	t0 := time.Now()
	ingestion, err := lock.New(ctx, cfg.QdrantEndpoint, cfg.DefaultCollection, *tenant)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "measure-ingest: %v\n", err)
		return 1
	}
	if err := ingestion.TryAcquire(); err != nil {
		_, _ = fmt.Fprintf(stderr, "measure-ingest: acquiring the ingestion lock: %v\n", err)
		return 1
	}
	defer func() { _ = ingestion.Release() }()
	lockDur := time.Since(t0)

	t1 := time.Now()
	scans, err := scanVaults(cfg, names)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "measure-ingest: %v\n", err)
		return 1
	}
	scanDur := time.Since(t1)

	t2 := time.Now()
	report, err := runOrchestrate(ctx, cfg, *tenant, scans)
	orchestrateDur := time.Since(t2)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "measure-ingest: %v\n", err)
		return 1
	}

	total := time.Since(start)
	phases := measure.IngestPhases{LockAcquire: lockDur, VaultScan: scanDur, Orchestrate: orchestrateDur}
	// Reprocessed is every note the run did work for. Skipped is the integrity short-circuit
	// (internal/ingest/note.go) — the note was read, hashed, and found already correct — which is what
	// a no-op run is made of. Anything else embedded or wrote, and NFR-5 does not describe that run.
	reprocessed := 0
	for state, n := range report.Counts() {
		if state != ingest.StateSkipped {
			reprocessed += n
		}
	}
	verdict := measure.EvaluateIngest(total, phases, nfr5Gate, len(report.Results), reprocessed)

	_, _ = fmt.Fprintln(stdout, report.String())
	_, _ = fmt.Fprintln(stdout)
	_, _ = fmt.Fprintln(stdout, verdict.String())
	if report.Failed() {
		_, _ = fmt.Fprintln(stderr, "measure-ingest: the run reported failed note(s) — this is not the clean "+
			"no-op run NFR-5 measures; fix the failures and measure again")
	}

	path := *out
	if path == "" {
		path = fmt.Sprintf("out/measure-ingest-%d.json", time.Now().Unix())
	}
	if err := writeJSONReport(path, verdict, report); err != nil {
		_, _ = fmt.Fprintf(stderr, "measure-ingest: writing report to %s: %v\n", path, err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "report written to %s\n", path)

	if !verdict.Pass || report.Failed() {
		return 1
	}
	return 0
}

// scanVaults mirrors cmd/cli/ingest.go's scanVaults+checkCrossVault.
func scanVaults(cfg *config.Config, names []string) ([]vault.ScanResult, error) {
	scans := make([]vault.ScanResult, 0, len(names))
	for _, name := range names {
		settings := cfg.Vaults[name]
		scan, err := vault.ScanVault(settings.Path, name, settings.AreaNames(), vault.Exclusions{
			Folders:   settings.Folders(),
			RootFiles: settings.RootFiles(),
		})
		if err != nil {
			return nil, fmt.Errorf("scanning vault %s at %s: %w", name, settings.Path, err)
		}
		scans = append(scans, scan)
	}
	for i := range scans {
		for j := i + 1; j < len(scans); j++ {
			if err := vault.CheckCrossVaultDuplicateUIDs(scans[i], scans[j]); err != nil {
				return nil, fmt.Errorf("vaults %s and %s: %w", scans[i].Vault, scans[j].Vault, err)
			}
		}
	}
	return scans, nil
}

// runOrchestrate mirrors cmd/cli/ingest.go's ingestScans: dial, handshake, ingest.Orchestrate. No
// --dry-run, no --prune, no --only, no --full — the plain incremental run NFR-5 measures.
func runOrchestrate(ctx context.Context, cfg *config.Config, tenant string, scans []vault.ScanResult) (ingest.Report, error) {
	client, err := store.NewQdrantClient(store.Config{Endpoint: cfg.QdrantEndpoint, APIKey: cfg.QdrantAPIKey})
	if err != nil {
		return ingest.Report{}, err
	}
	defer func() { _ = client.Close() }()

	points, err := store.NewClient(client, cfg.DefaultCollection)
	if err != nil {
		return ingest.Report{}, err
	}

	tokens, err := chunk.NewHTTPTokenCounter(cfg.EmbedderEndpoint)
	if err != nil {
		return ingest.Report{}, err
	}

	transport, err := embed.NewHTTPTransport(cfg.EmbedderEndpoint)
	if err != nil {
		return ingest.Report{}, err
	}
	embedder, err := embed.NewServiceEmbedder(embed.OperatorProfile(cfg.EmbedderEndpoint), transport)
	if err != nil {
		return ingest.Report{}, err
	}

	handshake, err := embedder.Handshake(ctx)
	if err != nil {
		return ingest.Report{}, fmt.Errorf("embedder handshake: %w", err)
	}

	report, err := ingest.Orchestrate(ctx, ingest.Deps{
		TenantID:       tenant,
		Store:          points,
		Embedder:       embedder,
		Handshake:      handshake,
		Chunk:          chunk.Config{FloorTokens: 256, CeilingTokens: 1024},
		Tokens:         tokens,
		UpsertAttempts: 3,
	}, scans...)
	if err != nil {
		return ingest.Report{}, err
	}
	report.Mode = "incremental"
	return report, nil
}

func writeJSONReport(path string, verdict measure.IngestReport, report ingest.Report) error {
	if dir := dirOf(path); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(reportJSON{
		RequirementGate: "NFR-5 (PRD.md): incremental no-op reingestion, complete operator command, <= 60s",
		TotalSeconds:    verdict.Total.Seconds(),
		LockAcquireSec:  verdict.Phases.LockAcquire.Seconds(),
		VaultScanSec:    verdict.Phases.VaultScan.Seconds(),
		OrchestrateSec:  verdict.Phases.Orchestrate.Seconds(),
		UnaccountedSec:  verdict.Unaccounted.Seconds(),
		GateSeconds:     verdict.Gate.Seconds(),
		Pass:            verdict.Pass,
		NoteCounts:      countsAsStrings(report),
		NotesFailed:     report.Failed(),
		MeasuredAt:      time.Now().UTC().Format(time.RFC3339),
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func countsAsStrings(r ingest.Report) map[string]int {
	out := map[string]int{}
	for state, n := range r.Counts() {
		out[string(state)] = n
	}
	return out
}

type reportJSON struct {
	RequirementGate string         `json:"requirement_gate"`
	TotalSeconds    float64        `json:"total_seconds"`
	LockAcquireSec  float64        `json:"lock_acquire_seconds"`
	VaultScanSec    float64        `json:"vault_scan_seconds"`
	OrchestrateSec  float64        `json:"orchestrate_seconds"`
	UnaccountedSec  float64        `json:"unaccounted_seconds"`
	GateSeconds     float64        `json:"gate_seconds"`
	Pass            bool           `json:"pass"`
	NoteCounts      map[string]int `json:"note_counts"`
	NotesFailed     bool           `json:"notes_failed"`
	MeasuredAt      string         `json:"measured_at"`
}

func dirOf(path string) string {
	i := strings.LastIndexByte(path, '/')
	if i < 0 {
		return ""
	}
	return path[:i]
}
