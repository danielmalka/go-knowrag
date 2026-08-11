package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/danielmalka/go-knowrag/internal/chunk"
	"github.com/danielmalka/go-knowrag/internal/config"
	"github.com/danielmalka/go-knowrag/internal/embed"
	"github.com/danielmalka/go-knowrag/internal/ingest"
	"github.com/danielmalka/go-knowrag/internal/ingest/lock"
	"github.com/danielmalka/go-knowrag/internal/store"
	"github.com/danielmalka/go-knowrag/internal/vault"
)

// bothVaults is the --vault value that means "every registered vault". It is spelled here rather
// than read from the roster because it is not a vault: it is this flag's word for all of the
// roster's names, and cfg.VaultNames() is what it resolves to.
const bothVaults = "both"

// defaultTenantID is the tenant this build populates. The CLI is privileged by declaration
// (ADR-002 §2.4) — it may write any tenant — so the value is a flag with a default rather than a
// setting an operator has to repeat on every run.
const defaultTenantID = "interno"

// The clamp bounds default to PRD §2.8's starting range, not to a measured optimum: S03 T10's
// calibration report has not landed, so there is no measured optimum to default to. They are flags
// because they feed point_hash — a run at other bounds reindexes what it touches, and that has to
// be something an operator states, not something they discover.
const (
	defaultFloorTokens   = 256
	defaultCeilingTokens = 1024
)

// upsertAttempts bounds the retry of a non-confirmed write (ingest.Deps.UpsertAttempts). Three is
// enough to ride out a dropped connection and few enough that an unreachable Qdrant fails the run
// instead of hanging it.
const upsertAttempts = 3

// newIngestCmd builds `ingest`.
//
// cfg is read at run time, not at build time, so the command tree assembles and prints its help
// without a valid configuration — same reason as newSchemaCmd.
func newIngestCmd(cfg *config.Config) *cobra.Command {
	opts := ingestOptions{
		vaultFlag: bothVaults,
		tenantID:  defaultTenantID,
		chunkCfg: chunk.Config{
			FloorTokens:   defaultFloorTokens,
			CeilingTokens: defaultCeilingTokens,
		},
	}

	cmd := &cobra.Command{
		Use:   "ingest",
		Short: "Scan the vaults and bring Qdrant in line with them",
		Long: "ingest scans each selected vault, chunks every note, embeds what changed and writes it,\n" +
			"pruning the stale tail of a note that got shorter. Re-running it over an unchanged corpus\n" +
			"writes nothing: a note whose points are all integral is skipped.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// From here on every error is a runtime failure — bad config, unreachable service,
			// a note that breaks the contract — and printing the usage block after it buries the
			// message that matters. Flag errors happen before this line and still print usage.
			cmd.SilenceUsage = true
			return runIngest(cmd.Context(), cmd.OutOrStdout(), cfg, opts)
		},
	}

	// The roster lives in cfg, and cfg is read at run time (see newIngestCmd's own comment), so this
	// text cannot name the configured vaults without breaking `--help` on a host with no config yet.
	cmd.Flags().StringVar(&opts.vaultFlag, "vault", opts.vaultFlag,
		fmt.Sprintf("which vault to ingest: a name from KNOWRAG_VAULTS, or %s for all of them", bothVaults))
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false,
		"scan and chunk, then stop: report the counts without embedding or writing anything")
	cmd.Flags().StringVar(&opts.tenantID, "tenant", opts.tenantID,
		"tenant_id every point is written under and filtered by")
	cmd.Flags().IntVar(&opts.chunkCfg.FloorTokens, "floor-tokens", opts.chunkCfg.FloorTokens,
		"merge consecutive sibling sections below this many tokens")
	cmd.Flags().IntVar(&opts.chunkCfg.CeilingTokens, "ceiling-tokens", opts.chunkCfg.CeilingTokens,
		"split a section above this many tokens")

	return cmd
}

// ingestOptions is the parsed flag set, in one value so runIngest takes what the command collected
// and a test wires it once.
type ingestOptions struct {
	vaultFlag string
	dryRun    bool
	tenantID  string
	chunkCfg  chunk.Config
}

// selectVaults resolves the --vault flag against the configured roster.
//
// The collision check comes first because D-26 is what made it reachable: `both` is a valid slug, so
// the roster — operator input now, not a compile-time enum — may legally contain a vault by that
// name, and then the flag has two meanings and no way to say which. Neither resolution order is
// salvageable: sentinel-first makes that vault unselectable, roster-first makes "every vault"
// unsayable, and both report the wrong scope as a clean run. Refusing every run until the name
// changes is the only answer that does not quietly ingest the wrong thing.
func selectVaults(cfg *config.Config, flag string) ([]string, error) {
	if _, taken := cfg.Vaults[bothVaults]; taken {
		return nil, fmt.Errorf(
			"a vault in the roster is named %q, which is also --vault's word for every vault; "+
				"rename that vault, because as it stands there is no way to select it alone", bothVaults)
	}
	if flag == bothVaults {
		return cfg.VaultNames(), nil
	}
	if _, ok := cfg.Vaults[flag]; !ok {
		return nil, fmt.Errorf("--vault %q is not a vault; use one of %s, or %s",
			flag, strings.Join(cfg.VaultNames(), ", "), bothVaults)
	}
	return []string{flag}, nil
}

func runIngest(ctx context.Context, out io.Writer, cfg *config.Config, opts ingestOptions) error {
	names, err := selectVaults(cfg, opts.vaultFlag)
	if err != nil {
		return err
	}

	// What the run requires depends on what the run does. A dry run never opens a connection to
	// Qdrant, so demanding its endpoint and key would refuse a command that has no use for them —
	// the same defect that used to refuse `schema apply` for a missing EMBEDDER_ENDPOINT. The
	// embedder is required either way: the clamp counts real BGE-M3 tokens over /tokenize and
	// refuses to approximate, so even a dry run talks to the service.
	need := config.NeedEmbedder
	if !opts.dryRun {
		need |= config.NeedQdrant | config.NeedCollection
	}
	// Two independent checks, joined rather than short-circuited: a host missing both the Qdrant
	// settings and the vault's path should hear about both in one run, not fix one and discover the
	// other on the next.
	if err := errors.Join(cfg.Require(need), cfg.RequireVaults(names...)); err != nil {
		return err
	}

	// The lock is taken before the scan rather than before the first write. Scanning the vaults is
	// ~15 s of reading, and a run that is going to be refused should be refused before it spends
	// them; everything after this line is covered by the deferred release, whatever the outcome.
	//
	// A dry run takes no lock because it writes nothing: it never opens a connection to Qdrant, so
	// there is nothing for a concurrent run to tread on and nothing of its own to protect. Not
	// because it lacks the settings to build a key — `need` only governs what Require demands, and an
	// environment that sets QDRANT_ENDPOINT and DEFAULT_COLLECTION populates cfg either way.
	//
	// The scope is the values this run will actually write with, not defaults re-derived here: a lock
	// keyed on anything else excludes a run nobody is making.
	if !opts.dryRun {
		ingestion, err := lock.New(ctx, cfg.QdrantEndpoint, cfg.DefaultCollection, opts.tenantID)
		if err != nil {
			return err
		}
		if err := ingestion.TryAcquire(); err != nil {
			if errors.Is(err, lock.ErrHeld) {
				// Wrapped, not replaced: main maps ErrHeld to its own exit code, and what the operator
				// reads has to be the situation — someone else is ingesting this scope — rather than the
				// sentence a lock package wrote about a file descriptor.
				return fmt.Errorf(
					"another ingestion is already running against %s, collection %s, tenant %s: "+
						"wait for it to finish, because two runs over one scope delete each other's "+
						"points (%w)",
					cfg.QdrantEndpoint, cfg.DefaultCollection, opts.tenantID, err)
			}
			return err
		}
		// The release error is not returned: the run is over by the time this fires, and the kernel
		// drops the flock when the process exits whether Release reported anything or not, so it
		// cannot change the outcome the operator is being told about. It is logged rather than
		// discarded because an unlock or a close that fails is something odd about this machine, and
		// the operator who eventually has to explain it needs a trace that it happened.
		defer func() {
			if err := ingestion.Release(); err != nil {
				slog.Warn("releasing the ingestion lock", "error", err)
			}
		}()
	}

	tokens, err := chunk.NewHTTPTokenCounter(cfg.EmbedderEndpoint)
	if err != nil {
		return err
	}
	// D-25 (docs/debitos-tecnicos.md): before anyone optimizes chunking, count what it costs.
	// Wrapping here means both dry-run and the real write path report through the same instrument.
	counted := chunk.NewCountingTokenCounter(tokens)

	scans, err := scanVaults(cfg, names)
	if err != nil {
		return err
	}
	// Before anything is embedded or written, in both modes. The point ID is
	// uuid5(tenant_id + uid + chunk_index) and does not include `vault`, so a uid repeated across
	// the two vaults collides and the second upsert overwrites the first in silence.
	//
	// ponytail: ingest.Orchestrate runs this check again on the write path. The repetition is a map
	// over ~730 uids and it is what makes the dry run — which never reaches Orchestrate — report
	// the collision instead of a clean count.
	if err := checkCrossVault(scans); err != nil {
		return err
	}

	if opts.dryRun {
		return dryRun(ctx, out, scans, opts.chunkCfg, counted)
	}
	return ingestScans(ctx, out, cfg, opts, scans, counted)
}

// scanVaults turns each selected vault into a ScanResult, with the areas and exclusions the
// operator configured. A vault that fails the contract fails the run before any other vault is
// read: the scan reports every offending note at once, and ingesting half a corpus while the
// other half is unreadable is not a partial success.
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
	return scans, nil
}

func checkCrossVault(scans []vault.ScanResult) error {
	for i := range scans {
		for j := i + 1; j < len(scans); j++ {
			if err := vault.CheckCrossVaultDuplicateUIDs(scans[i], scans[j]); err != nil {
				return fmt.Errorf("vaults %s and %s: %w", scans[i].Vault, scans[j].Vault, err)
			}
		}
	}
	return nil
}

// dryRun chunks everything and writes nothing, so an operator can see the size of a run — how many
// notes, how many chunks, how many chunks the model will be asked to embed — before spending the
// GPU and the network on it.
//
// It still counts real tokens, which is what makes the number trustworthy: a dry run against an
// approximate counter would report a chunk count the real run does not produce.
func dryRun(
	ctx context.Context,
	out io.Writer,
	scans []vault.ScanResult,
	cfg chunk.Config,
	tokens *chunk.CountingTokenCounter,
) error {
	totalNotes, totalChunks, totalOversize := 0, 0, 0
	var failures []error

	for _, scan := range scans {
		notes, chunks, oversize := 0, 0, 0
		for _, n := range scan.Notes {
			cs, err := chunk.ChunkNote(ctx, n, cfg, tokens)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			notes++
			chunks += len(cs)
			for _, c := range cs {
				if c.Oversize {
					oversize++
				}
			}
		}
		_, _ = fmt.Fprintf(out, "%s: %d note(s), %d skipped, %d chunk(s), %d oversize\n",
			scan.Vault, notes, len(scan.Skipped), chunks, oversize)
		totalNotes += notes
		totalChunks += chunks
		totalOversize += oversize
	}

	_, _ = fmt.Fprintf(out,
		"dry run: %d note(s), %d chunk(s) to embed, %d oversize — nothing was embedded or written\n",
		totalNotes, totalChunks, totalOversize)
	_, _ = fmt.Fprintln(out, tokens.Snapshot())

	for _, err := range failures {
		_, _ = fmt.Fprintf(out, "  - %v\n", err)
	}
	if len(failures) > 0 {
		return fmt.Errorf("%d note(s) could not be chunked", len(failures))
	}
	return nil
}

// ingestScans runs the real thing: handshake, then embed and write.
//
// The handshake is not a health check placed here for tidiness. Deps.Handshake feeds point_hash and
// must carry what the backend *confirmed*, never what this build expects — a service quietly
// serving an unpinned revision would otherwise poison every point_hash in the run, and the poisoned
// points would look integral forever. So it runs before the first write, and a divergence aborts
// with nothing written.
func ingestScans(
	ctx context.Context,
	out io.Writer,
	cfg *config.Config,
	opts ingestOptions,
	scans []vault.ScanResult,
	tokens *chunk.CountingTokenCounter,
) error {
	client, err := store.NewQdrantClient(store.Config{
		Endpoint: cfg.QdrantEndpoint,
		APIKey:   cfg.QdrantAPIKey,
	})
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	points, err := store.NewClient(client, cfg.DefaultCollection)
	if err != nil {
		return err
	}

	transport, err := embed.NewHTTPTransport(cfg.EmbedderEndpoint)
	if err != nil {
		return err
	}
	embedder, err := embed.NewServiceEmbedder(embedProfile(cfg.EmbedderEndpoint), transport)
	if err != nil {
		return err
	}

	handshake, err := embedder.Handshake(ctx)
	if err != nil {
		return fmt.Errorf("embedder handshake: %w", err)
	}

	report, err := ingest.Orchestrate(ctx, ingest.Deps{
		TenantID:       opts.tenantID,
		Store:          points,
		Embedder:       embedder,
		Handshake:      handshake,
		Chunk:          opts.chunkCfg,
		Tokens:         tokens,
		UpsertAttempts: upsertAttempts,
	}, scans...)
	if err != nil {
		return err
	}

	// Printed before the failure check because a run with failures still did whatever it did to the
	// notes it got through, and the operator needs to see that half.
	_, _ = fmt.Fprintln(out, report)
	_, _ = fmt.Fprintln(out, tokens.Snapshot())
	if report.Failed() {
		return errors.New("the run did not complete: see the failed note(s) above")
	}
	return nil
}

// embedProfile is how this command talks to the embedding service. The values are the client's,
// not the model's: BatchSize is how many chunks go in one request, MaxConcurrent how many requests
// are in flight against a single resident GPU, and Timeout bounds one attempt rather than the run.
//
// ponytail: not configurable. Nothing has yet needed a second set of numbers; promote them to
// settings when a deployment does.
func embedProfile(endpoint string) embed.Profile {
	return embed.Profile{
		Endpoint:      endpoint,
		Timeout:       2 * time.Minute,
		BatchSize:     32,
		MaxConcurrent: 2,
		MaxRetries:    3,
	}
}
