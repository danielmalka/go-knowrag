// Command mcp-server exposes the knowledge layer to MCP clients over stdio.
//
// It is a thin entrypoint over internal/retrieval and holds no search logic of its own (NFR-8).
// Its whole security posture is one sentence: the collection and tenant it serves are fixed in this
// process's configuration, so no tool argument, no handshake field and no sentence inside a
// retrieved note can move it (ADR-002). Serving a second scope is a second process.
//
// stdout belongs to the MCP protocol. Every diagnostic goes to stderr — a stray write to stdout
// corrupts the JSON-RPC stream and the client hangs with nothing to read in a log.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/danielmalka/go-knowrag/internal/config"
	"github.com/danielmalka/go-knowrag/internal/embed"
	"github.com/danielmalka/go-knowrag/internal/retrieval"
	"github.com/danielmalka/go-knowrag/internal/store"
)

const serverVersion = "0.1.0"

func main() {
	if err := run(); err != nil {
		// stderr, not the logger: the logger may be the thing that failed to come up, and this
		// message has to reach the operator either way.
		fmt.Fprintln(os.Stderr, "mcp-server:", err)
		os.Exit(1)
	}
}

func run() error {
	logger := config.NewLogger(os.Getenv("LOG_LEVEL"))
	slog.SetDefault(logger)

	cfg, err := LoadConfig()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Everything expensive is built here, once, and captured by the tool handler for the process's
	// whole life: the embedding client and the Qdrant connection must not be rebuilt per call
	// (S08 T7 — "no cold start per call").
	qdrantClient, err := store.NewQdrantClient(store.Config{
		Endpoint: cfg.QdrantEndpoint,
		APIKey:   cfg.QdrantAPIKey,
	})
	if err != nil {
		return err
	}
	defer func() { _ = qdrantClient.Close() }()

	// The searcher gets a Querier, not a Client: Client is the collection-bound point-CRUD type the
	// ingestion path uses, and a search names its collection in the request it built (S07).
	querier, err := store.NewQuerier(qdrantClient)
	if err != nil {
		return err
	}

	transport, err := embed.NewHTTPTransport(cfg.EmbedderEndpoint)
	if err != nil {
		return err
	}
	if err := verifyEmbedder(ctx, cfg, transport); err != nil {
		return err
	}

	embedder, err := embed.NewServiceEmbedder(embedProfile(cfg.EmbedderEndpoint), transport)
	if err != nil {
		return err
	}

	searcher := retrieval.NewSearcher(embedder, querier, retrieval.DefaultConfig())

	logger.Info("mcp-server starting", "config", cfg, "version", serverVersion)
	return newServer(cfg, searcher).Run(ctx, &mcp.StdioTransport{})
}

// bootHandshakeTimeout is the whole budget of the startup check, and it is the profile's Timeout
// rather than a deadline on the context because that is the only place it would survive: Handshake
// installs profile.Timeout on top of whatever context it is handed (internal/embed/handshake.go),
// so the effective bound is the smaller of the two and no outer deadline can widen it.
//
// It therefore cannot be embedProfile's 4 s. That number is derived from searchDeadline and from how
// long a query waits behind a batch on the GPU, and a backend that is merely still loading answers
// well past it: BGE-M3 takes ~11 s warm (ADR-001 §6.2, quoted in embed.ServiceEmbedder's own doc).
// Boot and a cold service coming up together is the *likeliest* moment for this process to start, and
// under a 4 s bound the slow answer comes back as ErrBackend — classified as an outage, warned about,
// and skipped. The check would exist and never run.
//
// 30 s is that load plus most of two more. Its cost is stated rather than hidden: a backend that
// black-holes the connection instead of refusing it stalls boot for 30 s. That is the rare case —
// a service not listening yet refuses instantly — and a slow boot is visible, while a skipped
// verification is exactly the silence D-32 is about.
//
// A var rather than a const only so a test can shrink it; nothing at run time writes it.
var bootHandshakeTimeout = 30 * time.Second

// bootProfile is how the startup check talks to the embedding service. It is not embedProfile: see
// bootHandshakeTimeout for why the search path's per-attempt budget cannot bound boot.
//
// MaxRetries is 1 because the two failures it could cover are answered differently and only one is
// worth waiting for. A service that is not listening yet refuses instantly, spends none of the
// budget, and is a case where starting anyway is already the right outcome (D-21) — retrying would
// delay boot for a verdict the search path reports per call anyway. A service that is loading is
// slow rather than absent, and one long attempt is what covers that.
//
// BatchSize and MaxConcurrent are 1 only because Profile.Validate rejects zero: the handshake sends
// no texts and makes exactly one request.
func bootProfile(endpoint string) embed.Profile {
	return embed.Profile{
		Endpoint:      endpoint,
		Timeout:       bootHandshakeTimeout,
		BatchSize:     1,
		MaxConcurrent: 1,
		MaxRetries:    1,
	}
}

// verifyEmbedder confirms once, at startup, that the service this process will embed queries with is
// the one the index was built with (embed.Handshake, PRD-contrato §2.4). cmd/cli does the same
// before its first write; this path did not, and it is the path where the damage is silent (D-32):
// an embedder serving a different revision, pooling or precision puts query vectors in a different
// space from the stored ones, so recall collapses while every call still returns a clean result full
// of plausible-looking chunks.
//
// The two ways the check can fail are not the same failure and must not share an outcome:
//
//   - No usable answer came back. That is an outage, and D-21 was paid precisely so this server
//     starts through one and explains itself per search (classifyUnavailable, unavailableMessage)
//     instead of dying at boot with the client left to guess. Refusing here would put that back. So
//     the check is skipped, the operator gets a warning saying it was skipped and why, and the next
//     search says the rest.
//   - The backend answered and its config diverges from this build's pins. Nothing is down; the
//     system is assembled wrong, and starting would mean serving confidently wrong answers for as
//     long as nobody happens to notice recall got worse. Refuse, naming the field.
//
// classifyUnavailable is what separates the two, reused rather than re-derived: a second notion of
// "the knowledge layer did not answer" living here would be free to drift from the one the search
// path reports, and then boot and search would disagree about whether the same failure is an outage.
//
// The first branch is wider than an outage, and the warning says so rather than claiming more than
// it knows. embed wraps every transport-level failure as ErrBackend, so an HTTP 500, a page of HTML
// from the wrong port, or a /handshake schema that changed all land there too — the backend answered,
// just not with anything this build can read. Telling those apart would be an error taxonomy for a
// distinction with no different action behind it: a service that cannot serve /handshake will not
// serve /embed either, so the next search reports it. What the warning must not do is assert nothing
// answered when something did.
//
// One wire failure lands on the other side, and the asymmetry is deliberate rather than an oversight:
// a body that decodes cleanly to `{}` never reaches the transport's error path, so it arrives as a
// report with every field at its zero value and is refused, naming the first one. embed's policy is
// that an unreported field is a divergence and not consent (ADR-001 §4), and a service that answers
// about its own configuration with nothing is not a service to embed a query with.
//
// It builds its own embedder over the caller's transport instead of taking one, because the boot
// budget lives in the profile (bootProfile) and handing this function a ServiceEmbedder would let a
// caller silently bound the check with a number chosen for searching.
//
// The report is discarded, which is the one difference from the ingestion. There, Handshake's return
// value feeds point_hash and is written to every point; here nothing consumes it — this call is made
// for its verdict alone.
func verifyEmbedder(ctx context.Context, cfg Config, transport embed.Transport) error {
	embedder, err := embed.NewServiceEmbedder(bootProfile(cfg.EmbedderEndpoint), transport)
	if err != nil {
		return err
	}

	_, err = embedder.Handshake(ctx)
	switch {
	case err == nil:
		return nil
	case classifyUnavailable(cfg, err) != nil:
		// Scrubbed like every other error that reaches a log here. Nothing on this path is known to
		// carry the Qdrant credential; the check costs one comparison and keeps "an error is scrubbed
		// on its way to a log" true by construction rather than by remembering which paths can.
		slog.Warn("the embedder handshake could not confirm the backend: starting unverified",
			"embedder_endpoint", cfg.EmbedderEndpoint,
			"error", scrubCredential(cfg, err.Error()),
			"consequence", "this process cannot tell whether the backend serves the model the index "+
				"was built with; a search that runs before it recovers reports the outage per call",
			// The part an operator would otherwise assume the other way round. This check runs once,
			// at boot, and nothing retries it: a service that comes back on a different revision is
			// then never caught, and the searches that follow look perfectly healthy.
			"not_retried", "the handshake is checked once at startup and never again; restart "+
				"mcp-server once the embedding service is up to verify it")
		return nil
	default:
		return fmt.Errorf("embedder handshake: %w — refusing to start: a server that embeds queries "+
			"with a configuration the index was not built with searches a different vector space and "+
			"answers from it, with a result nobody downstream can tell from a good one", err)
	}
}

// embedProfile is how this process talks to the embedding service. A query is a single short text,
// so the batching knobs the ingestion path needs are irrelevant here; the timeout is short because
// a search that takes a minute has already failed as far as the agent asking is concerned.
//
// Timeout is per attempt, so the whole embedding leg is bounded by 2×4 s plus 0,25 s of backoff =
// 8,25 s. That total is the number that matters, and it has to stay below searchDeadline: a search
// deadline that could fire while this leg was still retrying would silently shorten MaxRetries,
// turning a blip the retry absorbs into a reported outage and dropping the last transport failure
// from the error.
//
// The per-attempt number is not derived from a healthy idle service, and that is the point. Idle,
// a query embed measures ~50 ms (ADR-001 §6.2 measured a p99 of 71 ms on its own run; the number
// here is the median of the 2026-08-10 run tabulated below, and the two are different occasions,
// not one range). Almost any timeout would clear that. What this one has to survive is a query
// issued while an ingestion is running, because the embedding service holds one resident model and
// serialises inference: the query waits out whatever is executing on the GPU when it arrives.
//
// That wait used to grow with the ingestion profile's MaxConcurrent — the service admitted requests
// first-come-first-served, so a query queued behind every waiting batch as well as the running one.
// It no longer does: scripts/embedder-service/server.py reads the `kind` this client already sends
// and admits a query ahead of queued batches (D-27, paid 2026-08-10), which bounds the wait at the
// batch in flight — plus, if the query loses a microseconds-wide race to register, one more, once.
// Never one per queued batch, which is the difference that matters here: the bound is a constant,
// not a multiple of MaxConcurrent. The argument for why it cannot be worse lives in that file's
// gpu(), where the lock that makes it true is.
//
// Measured on the live service, 2026-08-10, one batch = 32 chunks of real Portuguese prose from
// this repo's markdown, 485–901 tokens each (median 651, 21330 tokens per request), which is the
// band cmd/cli/ingest.go's chunker produces; query = 16 tokens. Medians, n=5 (n=10 for the query
// alone), before and after on the same payload with a service restart between:
//
//	query alone, idle          0,05 s     0,04 s
//	one 32-chunk batch alone   1,28 s     1,19 s
//	query behind 1 batch       1,23 s     1,18 s
//	query behind 2 batches     2,39 s     1,22 s
//	query behind 4 batches     4,42 s     1,24 s
//
// The payload line is not decoration: the same scenario measured with 32 short strings puts a batch
// at ~0,3 s and every row shrinks with it, so a number quoted without it cannot be compared to
// anything. What does not depend on the payload is the shape — before, the wait is depth × batch;
// after, it is one batch.
//
// Four seconds is ~3,2× that bound and stays. Re-measuring it against the new floor would trade
// real headroom for nothing: the failure it prevents is reporting KNOWLEDGE BASE UNAVAILABLE for a
// service that is up, healthy and busy, and that error is expensive in a way 3 s of a timeout that
// almost never fires is not.
//
// MaxRetries is 2 rather than 3 for the same reason, and it is the trade that lets the budget fit:
// a longer single attempt beats more short ones, because a retry rejoins the same serialised
// service. Two attempts of 4 s ride out a longer stall than three of 2 s and cost 1,5 s less wall
// clock.
func embedProfile(endpoint string) embed.Profile {
	return embed.Profile{
		Endpoint:      endpoint,
		Timeout:       4 * time.Second,
		BatchSize:     1,
		MaxConcurrent: 1,
		MaxRetries:    2,
	}
}
