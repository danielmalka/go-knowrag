package embed

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielmalka/go-knowrag/internal/schema"
)

// stubTransport stands in for the wire, so everything ServiceEmbedder does above it — retries,
// batching, backpressure, validation, atomicity — is provable without a GPU or a socket. The real
// transport is HTTPTransport, covered in http_test.go.
type stubTransport struct {
	embed func(ctx context.Context, texts []string) ([]Embedding, error)
	info  func(ctx context.Context) (BackendHandshakeInfo, error)
	// Guarded: EmbedDocuments fans sub-batches out across goroutines, so a stub that records
	// anything has to be as concurrency-safe as the real transport must be.
	mu    sync.Mutex
	kinds []Kind // every kind this transport was asked for, in call order
}

func (s *stubTransport) Embed(ctx context.Context, texts []string, kind Kind) ([]Embedding, error) {
	s.mu.Lock()
	s.kinds = append(s.kinds, kind)
	s.mu.Unlock()
	if s.embed == nil {
		return echoEmbed(ctx, texts)
	}
	return s.embed(ctx, texts)
}

func (s *stubTransport) Info(ctx context.Context) (BackendHandshakeInfo, error) {
	if s.info == nil {
		return BackendHandshakeInfo{}, errors.New("stubTransport: no info configured")
	}
	return s.info(ctx)
}

// echoEmbed returns a valid embedding derived from each text, so a test can prove output i came
// from input i rather than merely that some 3-element slice came back.
func echoEmbed(_ context.Context, texts []string) ([]Embedding, error) {
	out := make([]Embedding, len(texts))
	for i, t := range texts {
		out[i] = fakeEmbedding(t)
	}
	return out, nil
}

func testProfile() Profile {
	return Profile{
		Endpoint:      "http://127.0.0.1:8080",
		Timeout:       time.Second,
		BatchSize:     10,
		MaxConcurrent: 4,
		MaxRetries:    3,
	}
}

func newTestEmbedder(t *testing.T, tr Transport) *ServiceEmbedder {
	t.Helper()
	e, err := NewServiceEmbedder(testProfile(), tr)
	if err != nil {
		t.Fatalf("NewServiceEmbedder: %v", err)
	}
	e.backoff = time.Millisecond // keep retry tests fast without exposing a knob callers could set
	return e
}

func TestServiceEmbedder_ImplementsEmbedder(t *testing.T) {
	var _ Embedder = (*ServiceEmbedder)(nil)
}

func TestNewServiceEmbedder_RejectsBadInput(t *testing.T) {
	if _, err := NewServiceEmbedder(testProfile(), nil); err == nil {
		t.Error("a nil transport was accepted")
	}
	if _, err := NewServiceEmbedder(Profile{}, &stubTransport{}); err == nil {
		t.Error("an unvalidated profile was accepted")
	}
}

// TestServiceEmbedder_ModelID_ReturnsPinnedSchemaConstant is S04 T7's "não existe caminho" half:
// no Profile value can change the answer, because no Profile field feeds it.
func TestServiceEmbedder_ModelID_ReturnsPinnedSchemaConstant(t *testing.T) {
	profiles := []Profile{
		testProfile(),
		{Endpoint: "http://elsewhere:9999", Timeout: time.Hour, BatchSize: 1, MaxConcurrent: 1, MaxRetries: 1},
	}
	for _, p := range profiles {
		e, err := NewServiceEmbedder(p, &stubTransport{})
		if err != nil {
			t.Fatalf("NewServiceEmbedder: %v", err)
		}
		if e.ModelID() != schema.BGEM3Revision {
			t.Errorf("ModelID() = %q, want schema.BGEM3Revision (%q)", e.ModelID(), schema.BGEM3Revision)
		}
	}
}

func TestServiceEmbedder_EmbedDocuments_ReturnsOneEmbeddingPerInputInOrder(t *testing.T) {
	texts := []string{"alpha", "beta", "gamma"}
	e := newTestEmbedder(t, &stubTransport{})

	got, err := e.EmbedDocuments(context.Background(), texts)
	if err != nil {
		t.Fatalf("EmbedDocuments: %v", err)
	}
	if len(got) != len(texts) {
		t.Fatalf("got %d embeddings for %d inputs", len(got), len(texts))
	}
	for i, text := range texts {
		if !reflect.DeepEqual(got[i], fakeEmbedding(text)) {
			t.Errorf("output %d does not correspond to input %q — the batch was reordered", i, text)
		}
	}
}

func TestServiceEmbedder_EmbedQuery_HappyPath(t *testing.T) {
	e := newTestEmbedder(t, &stubTransport{})
	got, err := e.EmbedQuery(context.Background(), "como configurar o cron do n8n")
	if err != nil {
		t.Fatalf("EmbedQuery: %v", err)
	}
	if !reflect.DeepEqual(got, fakeEmbedding("como configurar o cron do n8n")) {
		t.Error("EmbedQuery returned an embedding that does not match its input")
	}
}

func TestServiceEmbedder_EmbedDocuments_EmptyInputDoesNotCallBackend(t *testing.T) {
	var calls atomic.Int32
	e := newTestEmbedder(t, &stubTransport{
		embed: func(ctx context.Context, texts []string) ([]Embedding, error) {
			calls.Add(1)
			return echoEmbed(ctx, texts)
		},
	})

	got, err := e.EmbedDocuments(context.Background(), nil)
	if err != nil {
		t.Fatalf("EmbedDocuments(nil): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d embeddings for no input", len(got))
	}
	if calls.Load() != 0 {
		t.Errorf("the backend was called %d time(s) for an empty batch", calls.Load())
	}
}

func TestServiceEmbedder_EmbedDocuments_CountMismatchIsAnError(t *testing.T) {
	e := newTestEmbedder(t, &stubTransport{
		embed: func(_ context.Context, texts []string) ([]Embedding, error) {
			return []Embedding{fakeEmbedding(texts[0])}, nil
		},
	})

	_, err := e.EmbedDocuments(context.Background(), []string{"a", "b", "c"})
	if err == nil {
		t.Fatal("a response with the wrong number of embeddings was accepted")
	}
	if !strings.Contains(err.Error(), "3") || !strings.Contains(err.Error(), "1") {
		t.Errorf("error %q does not name both counts", err)
	}
}

// TestServiceEmbedder_EmbedDocuments_OutOfOrderSparseIsReportedNotFixed is the client half of the
// ordering contract. The service sorts by token id before answering; if it ever stops, that is a
// bug on its side and it has to surface here. Re-sorting would hide it, and the sparse vector feeds
// point_hash — an order that drifts silently makes an unchanged note look changed on every run.
func TestServiceEmbedder_EmbedDocuments_OutOfOrderSparseIsReportedNotFixed(t *testing.T) {
	e := newTestEmbedder(t, &stubTransport{
		embed: func(_ context.Context, texts []string) ([]Embedding, error) {
			out := make([]Embedding, len(texts))
			for i := range texts {
				out[i] = Embedding{
					Dense:  validDense(),
					Sparse: Sparse{Indices: []uint32{900, 3, 17}, Values: []float32{0.14, 0.28, 0.22}},
				}
			}
			return out, nil
		},
	})

	got, err := e.EmbedDocuments(context.Background(), []string{"a"})
	if err == nil {
		t.Fatal("an out-of-order sparse vector was accepted; the server's ordering guarantee is now unverifiable")
	}
	if got != nil {
		t.Error("a result was returned alongside the error")
	}
	if !strings.Contains(err.Error(), "ascending") {
		t.Errorf("error %q does not name the broken ordering invariant", err)
	}
}

func TestServiceEmbedder_EmbedQuery_AndDocuments_UseDifferentKinds(t *testing.T) {
	tr := &stubTransport{}
	e := newTestEmbedder(t, tr)

	if _, err := e.EmbedQuery(context.Background(), "q"); err != nil {
		t.Fatalf("EmbedQuery: %v", err)
	}
	if _, err := e.EmbedDocuments(context.Background(), []string{"d"}); err != nil {
		t.Fatalf("EmbedDocuments: %v", err)
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if !reflect.DeepEqual(tr.kinds, []Kind{KindQuery, KindPassage}) {
		t.Errorf("kinds = %v, want [query passage]", tr.kinds)
	}
}

func TestServiceEmbedder_EmbedDocuments_OneBadItemFailsWholeBatch(t *testing.T) {
	e := newTestEmbedder(t, &stubTransport{
		embed: func(_ context.Context, texts []string) ([]Embedding, error) {
			out := make([]Embedding, len(texts))
			for i, txt := range texts {
				out[i] = fakeEmbedding(txt)
			}
			out[2].Sparse.Values[0] = 0 // degenerate: zero weight
			return out, nil
		},
	})

	got, err := e.EmbedDocuments(context.Background(), []string{"a", "b", "c"})
	if err == nil {
		t.Fatal("a batch containing an invalid embedding was returned to the caller")
	}
	if got != nil {
		t.Errorf("a partial result of %d embeddings escaped alongside the error", len(got))
	}
	if !errors.Is(err, ErrSparseZeroWeight) {
		t.Errorf("error %v does not match ErrSparseZeroWeight; S06a cannot attribute it to a note", err)
	}

	var item *ItemError
	if !errors.As(err, &item) {
		t.Fatalf("error %v does not carry the failing item's position in the batch", err)
	}
	if item.Index != 2 {
		t.Errorf("ItemError.Index = %d, want 2", item.Index)
	}
}

// TestServiceEmbedder_EmbedDocuments_ItemErrorIndexIsInOriginalBatch is the trap in sub-batching:
// an index relative to the sub-batch would send S06a to the wrong note.
func TestServiceEmbedder_EmbedDocuments_ItemErrorIndexIsInOriginalBatch(t *testing.T) {
	e := newTestEmbedder(t, &stubTransport{
		embed: func(_ context.Context, texts []string) ([]Embedding, error) {
			out := make([]Embedding, len(texts))
			for i, txt := range texts {
				out[i] = fakeEmbedding(txt)
				if txt == "bad" {
					out[i].Dense = out[i].Dense[:3]
				}
			}
			return out, nil
		},
	})

	texts := make([]string, 25) // BatchSize 10 → sub-batches [0,10) [10,20) [20,25)
	for i := range texts {
		texts[i] = "ok"
	}
	texts[22] = "bad"

	_, err := e.EmbedDocuments(context.Background(), texts)
	var item *ItemError
	if !errors.As(err, &item) {
		t.Fatalf("error %v carries no item position", err)
	}
	if item.Index != 22 {
		t.Errorf("ItemError.Index = %d, want 22 (the position in the original input, not in the sub-batch)", item.Index)
	}
}

func TestServiceEmbedder_EmbedQuery_BackendTimeout_ReturnsError(t *testing.T) {
	e := newTestEmbedder(t, &stubTransport{
		embed: func(ctx context.Context, texts []string) ([]Embedding, error) {
			// Slow, not infinite. A stub that blocked forever would make a missing per-attempt
			// timeout hang the whole suite until `go test` panics, instead of failing here — a
			// test that can only fail by wedging the run is a test nobody will keep.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(2 * time.Second):
				return echoEmbed(ctx, texts)
			}
		},
	})
	e.profile.Timeout = 20 * time.Millisecond
	e.profile.MaxRetries = 1

	got, err := e.EmbedQuery(context.Background(), "hello")
	if err == nil {
		t.Fatal("a backend that never answered produced no error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %v is not a deadline-exceeded-class error", err)
	}
	if !errors.Is(err, ErrBackend) {
		t.Errorf("error %v does not match ErrBackend", err)
	}
	if len(got.Dense) != 0 {
		t.Error("a zero vector was returned alongside the timeout error")
	}
}

func TestServiceEmbedder_EmbedDocuments_CallerCancellationIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	e := newTestEmbedder(t, &stubTransport{
		embed: func(ctx context.Context, _ []string) ([]Embedding, error) {
			calls.Add(1)
			return nil, ctx.Err()
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := e.EmbedDocuments(ctx, []string{"a"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error %v is not a cancellation error", err)
	}
	if calls.Load() > 1 {
		t.Errorf("a cancelled call was retried %d times; the caller asked it to stop", calls.Load())
	}
}

func TestServiceEmbedder_EmbedDocuments_RetriesOnTransportError(t *testing.T) {
	var calls atomic.Int32
	e := newTestEmbedder(t, &stubTransport{
		embed: func(ctx context.Context, texts []string) ([]Embedding, error) {
			if calls.Add(1) < 3 {
				return nil, errors.New("500 internal server error")
			}
			return echoEmbed(ctx, texts)
		},
	})

	got, err := e.EmbedDocuments(context.Background(), []string{"a"})
	if err != nil {
		t.Fatalf("a transient failure was not retried to success: %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("attempts = %d, want 3", calls.Load())
	}
	if !reflect.DeepEqual(got[0], fakeEmbedding("a")) {
		t.Error("the successful retry did not return the correct result")
	}
}

func TestServiceEmbedder_EmbedDocuments_RetriesExhausted_ReturnsError(t *testing.T) {
	var calls atomic.Int32
	e := newTestEmbedder(t, &stubTransport{
		embed: func(context.Context, []string) ([]Embedding, error) {
			calls.Add(1)
			return nil, errors.New("connection refused")
		},
	})

	got, err := e.EmbedDocuments(context.Background(), []string{"a"})
	if err == nil {
		t.Fatal("a permanently failing backend produced no error")
	}
	if !errors.Is(err, ErrBackend) {
		t.Errorf("error %v does not match ErrBackend", err)
	}
	if got != nil {
		t.Error("a result was returned alongside the error")
	}
	if int(calls.Load()) != testProfile().MaxRetries {
		t.Errorf("attempts = %d, want exactly MaxRetries = %d", calls.Load(), testProfile().MaxRetries)
	}
}

func TestServiceEmbedder_EmbedDocuments_SplitsIntoConfiguredBatchSize(t *testing.T) {
	var mu sync.Mutex
	var sizes []int

	e := newTestEmbedder(t, &stubTransport{
		embed: func(ctx context.Context, texts []string) ([]Embedding, error) {
			mu.Lock()
			sizes = append(sizes, len(texts))
			mu.Unlock()
			return echoEmbed(ctx, texts)
		},
	})

	texts := make([]string, 25)
	for i := range texts {
		texts[i] = string(rune('a' + i))
	}

	got, err := e.EmbedDocuments(context.Background(), texts)
	if err != nil {
		t.Fatalf("EmbedDocuments: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	total := 0
	for _, s := range sizes {
		if s > testProfile().BatchSize {
			t.Errorf("a sub-batch of %d exceeds BatchSize %d", s, testProfile().BatchSize)
		}
		total += s
	}
	if len(sizes) != 3 {
		t.Errorf("got %d requests %v, want 3 ([10 10 5] in some order)", len(sizes), sizes)
	}
	if total != len(texts) {
		t.Errorf("sub-batches cover %d texts, want %d", total, len(texts))
	}
	for i, text := range texts {
		if !reflect.DeepEqual(got[i], fakeEmbedding(text)) {
			t.Fatalf("output %d does not correspond to input %q — reassembly crossed a sub-batch boundary", i, text)
		}
	}
}

func TestServiceEmbedder_EmbedDocuments_RespectsMaxConcurrency(t *testing.T) {
	var inFlight, peak atomic.Int32

	e := newTestEmbedder(t, &stubTransport{
		embed: func(ctx context.Context, texts []string) ([]Embedding, error) {
			n := inFlight.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			inFlight.Add(-1)
			return echoEmbed(ctx, texts)
		},
	})
	e.profile.BatchSize = 1
	e.profile.MaxConcurrent = 2

	texts := make([]string, 20)
	for i := range texts {
		texts[i] = string(rune('a' + i))
	}
	if _, err := e.EmbedDocuments(context.Background(), texts); err != nil {
		t.Fatalf("EmbedDocuments: %v", err)
	}
	if peak.Load() > 2 {
		t.Errorf("peak concurrent requests = %d, want at most MaxConcurrent = 2", peak.Load())
	}
}

func TestServiceEmbedder_EmbedDocuments_LaterSubBatchFailure_DiscardsEarlierResults(t *testing.T) {
	e := newTestEmbedder(t, &stubTransport{
		embed: func(ctx context.Context, texts []string) ([]Embedding, error) {
			if texts[0] == "fail" {
				return nil, errors.New("backend exploded")
			}
			return echoEmbed(ctx, texts)
		},
	})
	e.profile.BatchSize = 1
	e.profile.MaxConcurrent = 1
	e.profile.MaxRetries = 1

	got, err := e.EmbedDocuments(context.Background(), []string{"ok", "fail", "ok"})
	if err == nil {
		t.Fatal("a failing sub-batch did not fail the call")
	}
	if got != nil {
		t.Errorf("the successful sub-batch's result (%d embeddings) leaked to the caller", len(got))
	}
}
