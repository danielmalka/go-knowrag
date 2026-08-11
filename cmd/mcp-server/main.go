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

	embedder, err := newSearchEmbedder(cfg, transport)
	if err != nil {
		return err
	}

	searcher := retrieval.NewSearcher(embedder, querier, retrieval.DefaultConfig())

	logger.Info("mcp-server starting", "config", cfg, "version", serverVersion)
	return newServer(cfg, searcher).Run(ctx, &mcp.StdioTransport{})
}

// newSearchEmbedder builds the embedder every search goes through. It is a function only so that a
// test can assert against the object run() actually serves with, rather than against one it built
// itself from the same two lines — which would pass just as happily after run() stopped building
// this one.
//
// It is a second ServiceEmbedder over the same transport, distinct from the one verifyEmbedder
// makes, and the two are separate because their budgets are: 30 s of boot is the wrong bound for a
// query someone is waiting on, and 1,5 s of query is the wrong bound for a check nobody is waiting
// on that may have to outlast a wedged backend (bootHandshakeTimeout). The
// cost of that split is one extra /info round trip against a warm local service on the first search
// after a verified boot — the embedder confirms for itself rather than inheriting a verdict from an
// object it has no reference to (D-33). Measured in milliseconds, once per process.
func newSearchEmbedder(cfg Config, transport embed.Transport) (*embed.ServiceEmbedder, error) {
	return embed.NewServiceEmbedder(embedProfile(cfg.EmbedderEndpoint), transport)
}

// bootHandshakeTimeout is the whole budget of the startup check, and it lives in the profile
// (Profile.VerifyTimeout) rather than in a deadline on the context because that is the only place it
// would survive: Handshake installs its profile's verification budget on top of whatever context it
// is handed (internal/embed/handshake.go), so the effective bound is the smaller of the two and no
// outer deadline can widen it.
//
// It is not the search path's number, and that separation is the whole of D-32's fix: a budget
// derived from how long an agent waits for an answer has no business bounding startup.
//
// What it buys is a service that accepts the connection and then does not answer. There are three
// such states and none of them is a model load: a burst that fills the accept queue (server.py never
// raises request_queue_size off the stdlib default of 5), the GIL under contention, and MemoryHigh=4G
// in the unit throttling the process through reclaim without killing it. "Caught mid-shutdown" is
// deliberately not in that list, though an earlier draft of this comment said it: server.py installs
// no SIGTERM handler and has no drain, so the process dies with its socket rather than lingering with
// one open. Nothing else, and the correction matters because this
// comment used to claim otherwise. It said a service still loading its weights "answers well past"
// 4 s, and that a short bound would misread the slow answer as an outage. There is no slow answer:
// scripts/embedder-service/server.py loads the model and only then binds the socket (its main() calls
// load_model before ThreadingHTTPServer), so a service that is still coming up refuses the connection
// instantly. No timeout distinguishes it from a service that is not there, and none needs to — both
// are correctly reported as unreachable, at once, and the search path confirms for itself later
// (D-33). Found in review on 2026-08-11, one day after the comment shipped.
//
// 30 s therefore is not "a model load plus margin"; it is a generous bound on a case with no measured
// duration, chosen because a slow boot is visible while a skipped verification is exactly the silence
// D-32 is about. Reviewed on 2026-08-11 and kept rather than tuned, with the reason for keeping it
// stated as the weak thing it is: nothing in this stack argues for 30 over 5 or over 120. Under a
// real wedge a longer budget only delays the same outcome — start unverified, warn, and let the
// search path confirm for itself (D-21, D-33) — so the number decides how long a wedged backend
// stalls process start and nothing else. It would deserve an actual measurement if MCP relaunch
// latency ever became something anyone waits on.
//
// It stops being generous-for-free if a proxy ever sits in front of the service and accepts
// connections on its behalf while it loads — that would restore the slow answer this comment used to
// assume, and it is the event that brings this number back to the table.
//
// A var rather than a const only so a test can shrink it; nothing at run time writes it.
var bootHandshakeTimeout = 30 * time.Second

// bootProfile is how the startup check talks to the embedding service. It is not embedProfile: see
// bootHandshakeTimeout for why the search path's per-attempt budget cannot bound boot.
//
// MaxRetries is 1 because the failures it could cover are answered elsewhere. A service that is not
// listening — which includes one still loading, since it binds only after the load — refuses
// instantly, spends none of the budget, and is a case where starting anyway is already the right
// outcome (D-21); retrying would delay boot for a verdict the search path reports per call anyway.
// What is left is the backend that accepts and does not answer, and one long attempt covers that
// better than several short ones against a service that is not talking.
//
// This sentence used to end "a service that is loading is slow rather than absent", which was the
// same false premise bootHandshakeTimeout carried and is corrected there.
//
// BatchSize and MaxConcurrent are 1 only because Profile.Validate rejects zero: the handshake sends
// no texts and makes exactly one request.
func bootProfile(endpoint string) embed.Profile {
	return embed.Profile{
		Endpoint: endpoint,
		// Timeout is unused on this path — the boot check embeds nothing — but Profile.Validate
		// requires it, and a second copy of the boot budget is the value least likely to surprise
		// whoever adds an embed call here one day.
		Timeout:       bootHandshakeTimeout,
		VerifyTimeout: bootHandshakeTimeout,
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
//     search says the rest. Skipped here is not skipped forever: the search embedder verifies for
//     itself before its first embedding (embed.ServiceEmbedder, D-33), so a backend that comes back
//     on a different revision is caught by the first search that reaches it rather than never.
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
			// Said explicitly because the useful thing here is what the operator does *not* have to
			// do. This warning used to end with "restart mcp-server once the embedding service is
			// up", which was the honest instruction while a skipped check stayed skipped for the
			// life of the process (D-33). It no longer does.
			"retried", "the search path confirms the backend itself before it embeds anything, so a "+
				"service that comes back is checked on the first search that reaches it — and refused "+
				"there, by name, if it comes back on a different configuration. No restart is needed")
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
// Since D-33 the first search of every process also pays a handshake, because an embedder that has
// not confirmed its backend confirms it before embedding anything — and this one has not, being a
// different object from the one the boot check verified (newSearchEmbedder). That leg has to fit
// inside the same 10 s, which is what VerifyTimeout is for and why it is 1,5 s here: 1,5 + 8,25 =
// 9,75 s, still under the deadline, with the ordering pinned by TestSearchDeadline_ExceedsTheEmbedderBudget.
//
// An earlier version of this comment defended a 4 s verification that made the total 12,25 s, on
// the grounds that the deadline would cut it and the resulting KNOWLEDGE BASE UNAVAILABLE would be
// true anyway. Review showed the defence had a hole with the shape this file keeps producing: it
// only covers the case where the handshake FAILS. A handshake that succeeds near its cap leaves the
// embedding leg short of its own budget, and then a transient blip that the retry would have
// absorbed surfaces as an outage — the exact failure the ordering exists to prevent, arriving by a
// door the argument never looked at.
//
// What 1,5 s costs is smaller than the first draft of this comment claimed, and the correction is
// worth keeping because the claim was the same false one bootHandshakeTimeout used to make. It said
// a service still loading its model could not answer inside 1,5 s and so a recovery would report an
// outage instead of waiting. A loading service does not answer slowly — it is not listening yet
// (scripts/embedder-service/server.py binds after the load), so that call fails instantly at any
// budget. What 1,5 s actually gives up is the wedged backend: a service holding the connection open
// without answering is written off here in 1,5 s, where boot would wait 30 s for it. For a call
// someone is waiting on, written off and retried on the next search is the right answer.
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
		VerifyTimeout: 1500 * time.Millisecond,
		BatchSize:     1,
		MaxConcurrent: 1,
		MaxRetries:    2,
	}
}
