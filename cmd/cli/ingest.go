package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
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

// modeIncremental is the only mode that reaches a Report today: --dry-run returns before
// ingest.Orchestrate is ever called (see runIngest), so nothing writes "dry-run" into one. The pair
// becomes real when part A-ii unifies the two paths.
const modeIncremental = "incremental"

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
			// Silenced for everything RunE can return, which now includes the two refusals below.
			// Cobra writes the usage block to stdout, and with --json stdout is the report — a run
			// refused for a bad flag combination would hand a parser a wall of flag descriptions where
			// it expected a document. The refusals name the flags they are about, so nothing an
			// operator needs is lost. Cobra's own parse errors (an unknown flag) never reach this
			// function, so they keep their usage block.
			cmd.SilenceUsage = true

			// Before anything opens a socket or reads a directory: these are refusals about the
			// command line itself, so the operator hears about them without waiting out a vault scan.
			//
			// os.Stdin rather than cmd.InOrStdin(): the question is whether a human is on the other
			// end, which is a property of the file descriptor, not of the io.Reader cobra hands over.
			// The prompt goes to stderr for the same reason the usage block is silenced.
			if err := validateModes(opts); err != nil {
				return err
			}
			if err := confirmPrune(os.Stdin, cmd.ErrOrStderr(), opts); err != nil {
				return err
			}
			return runIngest(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), cfg, opts)
		},
	}

	// The roster lives in cfg, and cfg is read at run time (see newIngestCmd's own comment), so this
	// text cannot name the configured vaults without breaking `--help` on a host with no config yet.
	cmd.Flags().StringVar(&opts.vaultFlag, "vault", opts.vaultFlag,
		fmt.Sprintf("which vault to ingest: a name from KNOWRAG_VAULTS, or %s for all of them", bothVaults))
	// The second sentence is not decoration. A dry run reads nothing from Qdrant, so it cannot see
	// the index and cannot know which notes were deleted; an operator who reads a clean dry run as
	// "no orphans" has been misled by silence. --prune and --json are refused with it for the same
	// reason (validateModes), until part A-ii gives the dry run the snapshot the real run takes.
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false,
		"scan and chunk, then stop: report the counts without embedding or writing anything. "+
			"It never reads the index, so it lists no prune candidates — a clean dry run says nothing "+
			"about deleted notes")
	cmd.Flags().BoolVar(&opts.prune, "prune", false,
		"delete the points of notes that no longer exist in the scanned vaults. Destructive and "+
			"opt-in: without it the orphans are reported and left alone")
	cmd.Flags().BoolVar(&opts.yes, "yes", false,
		"authorize --prune without being asked. Required when stdin is not a terminal, where there "+
			"is nobody to answer the prompt")
	cmd.Flags().BoolVar(&opts.json, "json", false,
		"print the run report as JSON on stdout and nothing else; the tokenizer summary moves to stderr")
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
	prune     bool
	yes       bool
	json      bool
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

// runIngest takes two writers because --json splits them: out carries whatever a machine is meant
// to parse, errOut whatever a human reads alongside it. Without --json both are the same stream in
// practice and every line goes to out, as it always did.
func runIngest(ctx context.Context, out, errOut io.Writer, cfg *config.Config, opts ingestOptions) error {
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
	return ingestScans(ctx, out, errOut, cfg, opts, scans, counted)
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
	out, errOut io.Writer,
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
	report.Mode = modeIncremental

	// The prune runs against the orphan list the batch already computed, from the snapshot it took
	// before writing anything (batch.go). Confirmed is unconditionally true here because the run
	// would not have reached this line otherwise — confirmPrune refused, in RunE, before the vaults
	// were scanned. ingest.Prune re-checks it anyway, for callers that are not this one.
	//
	// A run whose snapshot failed is refused rather than pruned, and the empty orphan list is exactly
	// why the refusal has to be explicit. ingest.Prune over nothing succeeds, removes nothing and
	// returns 0 (prune.go: the loop body never runs) — so without the guard `--prune --yes` would
	// exit clean, and an operator reads a clean exit from a command they asked to delete things as
	// "there was nothing to delete". The human report does say `orphans: not scanned` above it, but a
	// scheduled run reads the exit code and not the prose. This is the same rule the report itself
	// follows: not having looked is never allowed to render as having found nothing.
	// Unconditional, not gated on opts.prune: a candidate whose file is on disk is misreported as a
	// deleted note whether or not this run deletes it, and the report is what an operator reads
	// before deciding to pass --prune next time.
	roots := make(map[string]string, len(scans))
	for _, s := range scans {
		roots[s.Vault] = cfg.Vaults[s.Vault].Path
	}
	splitStillOnDisk(roots, &report)

	var pruneErr error
	if opts.prune {
		pruneErr = pruneOrphans(ctx, points, opts.tenantID, roots, &report)
	}

	// Printed before the failure checks because a run with failures still did whatever it did to the
	// notes it got through, and the operator needs to see that half. A prune that died partway
	// through is the sharpest case: PointsPruned already holds what it removed before it stopped,
	// and that number is the one thing the next run's operator needs.
	if err := printReport(out, errOut, opts, report, tokens); err != nil {
		return err
	}
	if pruneErr != nil {
		return pruneErr
	}
	if report.Failed() {
		return errors.New("the run did not complete: see the failed note(s) above")
	}
	return nil
}

// printReport writes the run report, and with --json it writes nothing else to out: a caller piping
// stdout into a parser must not have to strip a human line first. The tokenizer summary (D-25) moves
// to errOut rather than being dropped — an instrument nobody reads is the same as no instrument.
func printReport(
	out, errOut io.Writer,
	opts ingestOptions,
	report ingest.Report,
	tokens *chunk.CountingTokenCounter,
) error {
	// The write errors are returned rather than discarded, unlike everywhere else in this file that
	// prints. A closed pipe or a full disk truncates the report mid-object, and with --json a
	// consumer that got half a document and a zero exit status has been told the run was clean by a
	// write that did not finish. Fprintln reports a short write as an error, which is the case a
	// bare `_, _ =` throws away.
	human := out
	if opts.json {
		human = errOut
		encoded, err := report.JSON()
		if err != nil {
			return fmt.Errorf("rendering the run report as JSON: %w", err)
		}
		if _, err := fmt.Fprintln(out, string(encoded)); err != nil {
			return fmt.Errorf("writing the run report: %w", err)
		}
	} else if _, err := fmt.Fprintln(out, report); err != nil {
		return fmt.Errorf("writing the run report: %w", err)
	}
	if _, err := fmt.Fprintln(human, tokens.Snapshot()); err != nil {
		return fmt.Errorf("writing the tokenizer summary: %w", err)
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
		Endpoint: endpoint,
		Timeout:  2 * time.Minute,
		// The handshake happens once, at startup, with nothing waiting on it. Nothing here is urgent
		// enough to justify a tighter bound: an ingestion is a 30-minute contract (NFR-4), so 30 s
		// spent confirming beats starting a run against a backend nobody checked. A service that is
		// merely still coming up does not consume this — it refuses the connection instantly, since
		// it binds its socket only after the model is loaded — so what the budget covers is a wedged
		// backend. The MCP search path is where this number has to be small, for its own reason.
		VerifyTimeout: 30 * time.Second,
		BatchSize:     32,
		MaxConcurrent: 2,
		MaxRetries:    3,
	}
}
