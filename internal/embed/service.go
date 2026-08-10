package embed

import (
	"context"
	"errors"
	"fmt"
	"sync"
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
type ServiceEmbedder struct {
	profile   Profile
	transport Transport
	expected  BackendHandshakeInfo
	backoff   time.Duration
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
	return &ServiceEmbedder{profile: p, transport: t, expected: Expected(), backoff: defaultBackoff}, nil
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
			return nil, err
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
			return nil, cErr
		}
		if attempt < e.profile.MaxRetries {
			select {
			case <-time.After(delay):
				delay *= 2
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	return nil, fmt.Errorf("%w: %s: %d attempt(s) failed, last error: %w",
		ErrBackend, e.profile.Endpoint, e.profile.MaxRetries, lastErr)
}
