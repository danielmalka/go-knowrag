package embed

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/danielmalka/go-knowrag/internal/schema"
)

// defaultBackoff is the first retry delay; it doubles on each further attempt. A constant rather
// than a Profile field: the story asks for "backoff", not for a tunable one, and a knob nobody
// has a reason to turn is a knob to get wrong.
const defaultBackoff = 250 * time.Millisecond

// Transport is one round-trip to the embedding service, and it is the only part of this package
// that knows the wire.
//
// The one implementation is HTTPTransport (http.go), against the service ADR-001 §7.2 chose. The
// interface survives the decision because it is what lets every wire-independent behaviour —
// per-attempt timeout, retry with backoff, sub-batching, backpressure, one-output-per-input,
// validation, all-or-nothing — be tested without a GPU, a model or a socket.
//
// Implementations must return exactly one Embedding per text, in order, exactly as the backend
// produced it: no sorting, no filtering, no repair — ServiceEmbedder validates, and a transport
// that cleaned up first would make those checks unreachable. Info must leave any field the backend
// does not report at its zero value; Handshake reads that as a divergence.
type Transport interface {
	Embed(ctx context.Context, texts []string, kind Kind) ([]Embedding, error)
	Info(ctx context.Context) (BackendHandshakeInfo, error)
}

// ServiceEmbedder is the Embedder that talks to the resident embedding service.
//
// Resident is not an implementation preference: loading BGE-M3 takes ~11 s warm (ADR-001 §6.2; the
// earlier 314,5 s figure included downloading the weights). Eleven seconds in front of a measured
// 71 ms p99 is still the whole latency budget spent on startup, so a design that loads per call is
// out. This type assumes the model is already in memory on the other side and never starts anything.
//
// It is safe for concurrent use, as both its callers need: S06a embeds batches while S07 serves
// queries, and neither owns the other's client.
//
// It also refuses to embed anything through a backend it has not confirmed. See ensureVerified.
type ServiceEmbedder struct {
	profile   Profile
	transport Transport
	expected  BackendHandshakeInfo
	backoff   time.Duration

	// verified latches at the first successful Handshake and never clears. verifyGate serialises the
	// attempts: whoever holds the single slot is handshaking, and everyone else waits.
	//
	// The pair rather than a sync.Once because Once latches failure too, and a failed handshake is
	// exactly the state that must be retried: the case this exists for is a backend that was down
	// when the process started and came back afterwards (D-33). Once would answer "already done" for
	// the rest of the process's life, which is the bug, not the fix.
	//
	// A one-slot channel rather than a sync.Mutex because the wait has to be abandonable. A mutex
	// cannot be acquired with a deadline, so a caller whose own context died went on queuing for a
	// lock it no longer had any use for, and then walked out of the wait holding a dead context —
	// which is how a cancelled call used to reach the fan-out and come back with zero vectors and no
	// error. Selecting on the caller's context makes giving up the caller's own decision.
	//
	// What this does not do is share one attempt's verdict. On success it is moot: everyone queued
	// behind the winner re-checks the latch and makes no call. On failure each caller still waiting
	// runs its own handshake in turn, so a wedged backend is asked once per waiting caller rather
	// than once — bounded now by each caller's patience rather than unbounded. Deduplicating the
	// failed attempt needs an in-flight generation to distinguish "queued behind this attempt" from
	// "arrived after it", and it has to refuse to share a verdict that came from the initiator's own
	// cancellation rather than from the backend. That is more machinery than the latency it saves in
	// a state where every one of those callers is failing anyway.
	verified   atomic.Bool
	verifyGate chan struct{}
}

var _ Embedder = (*ServiceEmbedder)(nil)

// NewServiceEmbedder validates the profile up front so a misconfiguration fails at startup rather
// than in the middle of a run.
func NewServiceEmbedder(p Profile, t Transport) (*ServiceEmbedder, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if t == nil {
		return nil, errors.New("embed: transport is nil; use NewHTTPTransport(profile.Endpoint)")
	}
	return &ServiceEmbedder{
		profile:    p,
		transport:  t,
		expected:   Expected(),
		backoff:    defaultBackoff,
		verifyGate: make(chan struct{}, 1),
	}, nil
}

// ModelID reports the revision this build is pinned to, and reads nothing from the profile — there
// is no configuration path by which two deployments could report different revisions (S04 T7).
// It is self-attested: what the backend actually loaded is Handshake's answer, not this one.
func (e *ServiceEmbedder) ModelID() string { return schema.BGEM3Revision }

// EmbedQuery embeds one text through exactly the same path as a batch, so a query vector and a
// document vector can never diverge by taking different validation branches.
func (e *ServiceEmbedder) EmbedQuery(ctx context.Context, text string) (Embedding, error) {
	out, err := e.embed(ctx, []string{text}, KindQuery)
	if err != nil {
		return Embedding{}, err
	}
	return out[0], nil
}

// EmbedDocuments embeds every text, splitting into BatchSize-sized requests with at most
// MaxConcurrent in flight, and returns results in the caller's input order.
//
// All-or-nothing, and that is a contract rather than an implementation detail (S04 Escopo: "Erro
// invalida o batch inteiro — nunca retorna resultado parcial"): every sub-batch is collected before
// anything is returned, so a failure in the last request still discards the first request's
// successes. A caller never sees len(result) < len(texts), and never sees a zero vector standing in
// for one that failed.
func (e *ServiceEmbedder) EmbedDocuments(ctx context.Context, texts []string) ([]Embedding, error) {
	return e.embed(ctx, texts, KindPassage)
}

func (e *ServiceEmbedder) embed(ctx context.Context, texts []string, kind Kind) ([]Embedding, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, outerCtxErr(e.profile.Endpoint, err)
	}
	if err := e.ensureVerified(ctx); err != nil {
		return nil, err
	}

	// Cancelled as soon as one sub-batch fails: the call is already doomed, and letting the other
	// requests run would keep the GPU busy producing results nobody will ever see.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	out := make([]Embedding, len(texts))
	sem := make(chan struct{}, e.profile.MaxConcurrent)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for start := 0; start < len(texts); start += e.profile.BatchSize {
		end := min(start+e.profile.BatchSize, len(texts))

		wg.Add(1)
		go func(start, end int) {
			defer wg.Done()

			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-runCtx.Done():
				// Recording the reason is the whole point of this branch existing rather than a
				// bare return. A sub-batch that gives up here writes nothing into its slice of
				// out, and out was allocated full of zero Embeddings — so a silent return left
				// firstErr nil and handed the caller a vector of zeros as a *successful* result.
				// A zero query vector searches the collection and comes back with plausible
				// nonsense, which is the one failure nothing downstream can detect.
				//
				// It is guarded on the caller's context rather than runCtx's because the other way
				// this branch fires is a sibling sub-batch that already failed and cancelled: that
				// path sets firstErr before it cancels, so there is nothing to record and its more
				// specific error must not be overwritten.
				if cErr := ctx.Err(); cErr != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = outerCtxErr(e.profile.Endpoint, cErr)
					}
					mu.Unlock()
				}
				return
			}

			if err := e.embedSubBatch(runCtx, texts[start:end], out[start:end], start, kind); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				mu.Unlock()
			}
		}(start, end)
	}
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// ensureVerified makes "the backend was confirmed" a precondition of embedding rather than a
// courtesy the caller is trusted to have performed.
//
// Handshake's contract already said it must be called once before the first embedding, and both
// callers do call it — but a contract enforced by documentation only holds while every path keeps
// it. cmd/mcp-server's did not, in the specific way that made D-32 possible, and the fix for D-32
// left one window open behind it (D-33): the startup check is allowed to be *skipped* when the
// backend is unreachable, because refusing to boot through an outage is the thing D-21 exists to
// prevent. A skipped check used to mean unverified for the life of the process, and the sequence
// that produces it is ordinary — restarting the embedding service is stop, swap, start, and an MCP
// client relaunches its server whenever it likes, including during the stop.
//
// So the guarantee moves here, where every caller gets it: nothing is embedded through a backend
// this embedder has not confirmed, and if the confirmation has not happened yet it happens now.
//
// Three consequences worth stating, because each is a choice:
//
//   - A caller that already handshook pays nothing. Handshake sets the same latch, so cmd/cli's
//     explicit call at startup — whose return value it needs anyway, for point_hash — leaves this
//     as one atomic load per batch.
//   - A failed verification does not latch, so the next call tries again. That is the whole point:
//     the backend that was down at boot is expected to come back.
//   - The handshake is bounded by profile.VerifyTimeout, not by profile.Timeout, and it happens
//     inside the caller's call. A caller whose own deadline is tight sets a tight one; a caller
//     verifying at startup sets a generous one. Either way a failure here is reported as an outage
//     and the next call tries again, which is the right answer while the answer is unknown.
//
// It is deliberately not a fourth thing: it does not re-verify periodically. A backend that swapped
// revisions *after* a successful handshake is not covered here and never was — that is the same
// window every long-lived client of a mutable service has, and closing it needs a mechanism (a
// generation token on the wire) that neither side has today.
func (e *ServiceEmbedder) ensureVerified(ctx context.Context) error {
	if e.verified.Load() {
		return nil
	}

	// The wait is abandonable: a caller whose deadline has already passed has no use for a slot it
	// would only walk out of with a dead context, and nothing after this point would have caught it.
	select {
	case e.verifyGate <- struct{}{}:
		defer func() { <-e.verifyGate }()
	case <-ctx.Done():
		return outerCtxErr(e.profile.Endpoint, ctx.Err())
	}

	// Re-checked once inside: everything queued behind the caller that just succeeded would
	// otherwise handshake again, one after another, for no answer that is not already known.
	if e.verified.Load() {
		return nil
	}

	if _, err := e.Handshake(ctx); err != nil {
		return fmt.Errorf("%w: nothing was embedded — this backend has not been confirmed to serve "+
			"the configuration the index was built with, and embedding through an unconfirmed one is "+
			"how a wrong vector space stays invisible", err)
	}
	return nil
}

// embedSubBatch performs one request-with-retries and writes its validated results into dst.
// offset is the sub-batch's start in the original input, so an ItemError points at the note the
// caller actually passed rather than at a position in a slice it never saw.
func (e *ServiceEmbedder) embedSubBatch(ctx context.Context, texts []string, dst []Embedding, offset int, kind Kind) error {
	raw, err := e.callWithRetry(ctx, texts, kind)
	if err != nil {
		return err
	}
	if len(raw) != len(texts) {
		return fmt.Errorf(
			"backend returned %d embeddings for %d texts; one output per input, in order, is the "+
				"only response shape this client accepts", len(raw), len(texts))
	}
	// Validated exactly as it arrived. The service already emits sparse pairs sorted by index with
	// no zero weights and no repeated ids, and that is a contract between the two sides rather than
	// a convenience.
	//
	// This comment used to justify that by saying the sparse vector feeds point_hash, and it does
	// not: ComputePointHash takes note metadata, chunk text, the pipeline config and the seven
	// handshake fields, and `embedder_sparse_params` among them is the static {kind, id_space} the
	// handshake declares — no per-chunk index or weight enters the hash. What an unstable order
	// actually corrupts is the vector written to Qdrant, so the damage lands on hybrid retrieval,
	// which returns worse results and says nothing. The reason to validate rather than sort here is
	// unchanged and is the reason that matters: sorting would silently absorb the day the server
	// stops holding up its end, and that is the one thing that must not happen quietly.
	for i, item := range raw {
		if err := validateEmbedding(item); err != nil {
			return &ItemError{Index: offset + i, Err: err}
		}
		dst[i] = item
	}
	return nil
}

// callWithRetry makes at most MaxRetries attempts, each bounded by Timeout, with a doubling delay
// between them.
//
// Only transport failures are retried. A caller's cancellation is not — it asked to stop — and a
// validation failure is not either, since it happens after this returns: re-asking a backend that
// just produced a malformed vector for the same text is unlikely to produce a different one and
// certain to triple the time before anyone hears about it.
//
// ponytail: a 4xx is retried like any other failure. The service only returns one (400, malformed
// request) for a bug on this side, which no retry fixes; the cost of not classifying is three round
// trips against localhost before the same error surfaces. Classify if that ever shows up in a
// latency profile.
func (e *ServiceEmbedder) callWithRetry(ctx context.Context, texts []string, kind Kind) ([]Embedding, error) {
	delay := e.backoff
	var lastErr error

	for attempt := 1; attempt <= e.profile.MaxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, outerCtxErr(e.profile.Endpoint, err)
		}

		attemptCtx, cancel := context.WithTimeout(ctx, e.profile.Timeout)
		out, err := e.transport.Embed(attemptCtx, texts, kind)
		cancel()

		if err == nil {
			return out, nil
		}
		lastErr = err

		// The caller (or an already-failed sibling sub-batch) cancelled: stop, and report that
		// rather than a retry-exhausted error, which would name the wrong cause.
		if cErr := ctx.Err(); cErr != nil {
			return nil, outerCtxErr(e.profile.Endpoint, cErr)
		}
		if attempt < e.profile.MaxRetries {
			select {
			case <-time.After(delay):
				delay *= 2
			case <-ctx.Done():
				return nil, outerCtxErr(e.profile.Endpoint, ctx.Err())
			}
		}
	}
	return nil, fmt.Errorf("%w: %s: %d attempt(s) failed, last error: %w",
		ErrBackend, e.profile.Endpoint, e.profile.MaxRetries, lastErr)
}

// outerCtxErr renders the caller's own context error on its way out of this package. It is applied
// at every point where the outer context is checked, which is the only reason it is a function
// rather than four copies of an if.
//
// A deadline that kills an embedding call is ErrBackend by this package's own definition — "no
// answer came out of the service at all" — and the wrapping happens here rather than in whichever
// consumer wants to report it. A consumer only sees a bare context.DeadlineExceeded and would have
// to *infer* which component died from the shape of an error that names none; this package knows,
// because it is the one that made the call. Getting that inference wrong sends an operator to the
// VPS when the local embedding service is what stopped, which is worse than no message at all.
//
// Cancellation is deliberately not wrapped. It means the caller stopped the call — a client that
// disconnected, or a sibling sub-batch that already failed and cancelled its runCtx — not a service
// that failed to answer, and widening this to cover both would report every disconnect as an outage.
//
// The context error stays in the chain either way, so errors.Is(err, context.DeadlineExceeded)
// keeps working for anything that checks it.
func outerCtxErr(endpoint string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %s: %w", ErrBackend, endpoint, err)
	}
	return err
}
