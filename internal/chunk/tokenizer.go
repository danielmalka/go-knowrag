package chunk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// TokenCounter counts tokens the way the embedding model sees them.
//
// "Token" here means the real BGE-M3 tokenizer (PRD-stories-fundacao §3, S03): counting words or
// characters/4 makes the clamp diverge from what the model reads and invalidates the calibration.
// This interface is the seam that keeps that promise honest — the chunking logic never approximates,
// it asks.
//
// HTTPTokenCounter below is the real implementation, against ADR-001 §7.2's runtime. It waited for
// that decision on purpose: a client written against a guessed request shape would have had to be
// rewritten, along with its call sites, the day the ADR contradicted the guess.
type TokenCounter interface {
	CountTokens(ctx context.Context, text string) (int, error)
}

// FakeTokenCounter counts whitespace-separated fields. It is a test fixture, not an approximation
// of BGE-M3 — it exists so the chunking logic can be tested deterministically and offline, with
// fixtures whose token counts are countable by eye.
//
// It must never be used for the calibration report (T10): a report written against this counter
// would measure this function, not the model.
type FakeTokenCounter struct{}

func (FakeTokenCounter) CountTokens(_ context.Context, text string) (int, error) {
	return len(strings.Fields(text)), nil
}

// HTTPTokenCounter counts tokens by asking scripts/embedder-service/server.py, the runtime
// ADR-001 §7.2 settled on:
//
//	POST /tokenize  {"inputs": [str]}  ->  {"counts": [int]}
//
// It asks the same process that will embed the text, which is the whole point of the endpoint
// existing: a separate tokenizer, however faithful today, is a second artifact free to drift from
// the one the model actually reads.
//
// It sends one text per request rather than batching, because CountTokens is a one-text question
// and the clamp asks it while it is still deciding where the next boundary goes — there is no
// batch to form yet. The service is local and the model is resident, so the cost is a loopback
// round trip.
//
// ponytail: no caching. The clamp asks about overlapping ranges of the same note, so a memo keyed
// by text would hit; add it if a full-corpus run shows /tokenize dominating the wall clock.
type HTTPTokenCounter struct {
	endpoint string
	client   *http.Client
}

var _ TokenCounter = (*HTTPTokenCounter)(nil)

// NewHTTPTokenCounter validates the endpoint at construction, so a typo fails where an operator is
// reading a config error rather than midway through chunking a 730-note corpus.
func NewHTTPTokenCounter(endpoint string) (*HTTPTokenCounter, error) {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("chunk: tokenizer endpoint %q is not a URL: %w", endpoint, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf(
			"chunk: tokenizer endpoint %q must be an http(s) URL with a host, e.g. http://127.0.0.1:7999",
			endpoint)
	}
	// No Timeout on the client: every call carries the caller's context, and a second, independent
	// clock here would produce two different errors for one condition.
	return &HTTPTokenCounter{endpoint: endpoint, client: &http.Client{}}, nil
}

// maxErrorBody caps how much of a failure response is read into an error message. The service is
// local and returns small JSON, but an error path that reads an unbounded body is how a wrong URL
// pointed at something else turns into a memory problem.
const maxErrorBody = 4 << 10

// tokenizeAttempts bounds the retry of a transient failure. It exists because this client and
// internal/embed's talk to the SAME process, and only that one was hardened: restarting the
// embedding service to deploy a fix is a legitimate operational act, and without a retry here it
// marks dozens of perfectly good notes StateFailed while the embedding client rides through the
// same blip. Observed for real during review — a restart mid-scan failed ~75 consecutive notes with
// connection refused, none retried.
//
// ponytail: fixed small delay rather than exponential backoff. The peer is a local process coming
// back up, not a rate-limited remote; the useful wait is "about as long as a restart", and a
// backoff curve tuned for congestion would only make the first retry later.
const (
	tokenizeAttempts = 3
	tokenizeDelay    = 500 * time.Millisecond
)

// CountTokens asks the service, retrying a transport failure but never a rejection.
//
// The distinction is the point: a refused connection is the service being restarted, and trying
// again is right. An HTTP 400 is the service saying the input is wrong, and trying again just asks
// the same wrong question — so a non-2xx is returned on the first response, unretried.
func (c *HTTPTokenCounter) CountTokens(ctx context.Context, text string) (int, error) {
	var lastErr error
	for attempt := range tokenizeAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(tokenizeDelay):
			}
		}
		n, err, retriable := c.countOnce(ctx, text)
		if err == nil {
			return n, nil
		}
		lastErr = err
		// A caller who cancelled is not waiting for a better answer.
		if !retriable || ctx.Err() != nil {
			return 0, err
		}
	}
	return 0, fmt.Errorf("after %d attempt(s): %w", tokenizeAttempts, lastErr)
}

// countOnce is one attempt. The bool says whether the failure is worth repeating.
func (c *HTTPTokenCounter) countOnce(ctx context.Context, text string) (int, error, bool) {
	// json.Marshal of a string cannot fail — it has no unsupported types, no cycles and no custom
	// marshaler — so the body is assembled around the escaped text rather than through a struct
	// whose error return no test could ever reach.
	quoted, _ := json.Marshal(text)
	body := append(append([]byte(`{"inputs":[`), quoted...), []byte(`]}`)...)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/tokenize", bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("chunk: building POST %s/tokenize: %w", c.endpoint, err), false
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("chunk: POST %s/tokenize: %w", c.endpoint, err), true
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		// 5xx is the service failing at something it might do right next time; 4xx is a verdict on
		// this input and repeating it changes nothing.
		return 0, fmt.Errorf("chunk: POST %s/tokenize: HTTP %d: %s",
			c.endpoint, resp.StatusCode, strings.TrimSpace(string(msg))), resp.StatusCode >= 500
	}

	var decoded struct {
		Counts []int `json:"counts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return 0, fmt.Errorf("chunk: POST %s/tokenize: decode response: %w", c.endpoint, err), false
	}
	// One text in, one count out. Anything else is a response that does not answer the question
	// asked, and returning counts[0] from it would silently clamp against another text's length.
	if len(decoded.Counts) != 1 {
		return 0, fmt.Errorf("chunk: POST %s/tokenize: %d count(s) for 1 text",
			c.endpoint, len(decoded.Counts)), false
	}
	return decoded.Counts[0], nil, false
}

// CountingTokenCounter wraps a TokenCounter and records how many times it was asked, how much text
// it measured, and how long it spent answering. D-25 (docs/debitos-tecnicos.md) found that a
// `--dry-run` spends 388 of 403 seconds in chunking without knowing why — this is the instrument
// that answers "why" before anyone reaches for a fix like memoizing the clamp.
//
// The counters are atomic rather than plain fields even though ingest.RunBatch processes notes one
// at a time today: proving that stays true at every call site is more work than a type that does not
// need it to.
type CountingTokenCounter struct {
	next  TokenCounter
	calls atomic.Int64
	bytes atomic.Int64
	nanos atomic.Int64
}

// NewCountingTokenCounter wraps tc. It does not replace tc's decisions — a call still goes to tc and
// returns exactly what tc returned — it only counts.
func NewCountingTokenCounter(tc TokenCounter) *CountingTokenCounter {
	return &CountingTokenCounter{next: tc}
}

func (c *CountingTokenCounter) CountTokens(ctx context.Context, text string) (int, error) {
	start := time.Now()
	n, err := c.next.CountTokens(ctx, text)
	c.calls.Add(1)
	c.bytes.Add(int64(len(text)))
	c.nanos.Add(int64(time.Since(start)))
	return n, err
}

// TokenCounterStats is a read of CountingTokenCounter's counters at one point in time, taken once
// the run that accumulated them is done.
type TokenCounterStats struct {
	Calls int64
	Bytes int64

	// Spent is wall time inside CountTokens, which for HTTPTokenCounter includes the retry backoff
	// above (500 ms per retriable failure, up to three attempts) and not only the service's own
	// answer. A run that survives an embedder restart mid-scan — observed for real, ~75 consecutive
	// notes — reports a Spent inflated by seconds of sleeping, and nothing in the number says so.
	// Read it as "time the clamp waited on the tokenizer", not "time the tokenizer worked": the
	// difference matters because D-25 exists to stop this project treating one measured part as if
	// it explained the whole, which is the error D-22 recorded twice.
	Spent time.Duration
}

// Snapshot reads the counters. Safe to call while CountTokens is still being called elsewhere — the
// three loads are independent and a run summary printed after the last call sees the final values.
func (c *CountingTokenCounter) Snapshot() TokenCounterStats {
	return TokenCounterStats{
		Calls: c.calls.Load(),
		Bytes: c.bytes.Load(),
		Spent: time.Duration(c.nanos.Load()),
	}
}

func (s TokenCounterStats) String() string {
	return fmt.Sprintf("tokenizer: %d call(s), %d byte(s) measured, %s waiting on the tokenizer (retries included)",
		s.Calls, s.Bytes, s.Spent)
}
