package chunk

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFakeTokenCounter_SameTextSameCount(t *testing.T) {
	tc := FakeTokenCounter{}
	const text = "# Title\n\nsome body with words\n"

	first, err := tc.CountTokens(context.Background(), text)
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	second, err := tc.CountTokens(context.Background(), text)
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	if first != second {
		t.Errorf("counts differ across calls on identical text: %d then %d", first, second)
	}
}

// TestFakeTokenCounter_CountsWhitespaceSeparatedFields pins the fake's rule, because every clamp
// test downstream picks its floor and ceiling by counting words in its fixture. If the rule changed
// without this test, those fixtures would keep compiling and start asserting nothing.
func TestFakeTokenCounter_CountsWhitespaceSeparatedFields(t *testing.T) {
	tests := []struct {
		text string
		want int
	}{
		{"", 0},
		{"   \n\t ", 0},
		{"one", 1},
		{"one two three", 3},
		{"one\ntwo\tthree\n\nfour", 4},
		{"Title > Setup", 3},
	}

	for _, tc := range tests {
		t.Run(tc.text, func(t *testing.T) {
			got, err := FakeTokenCounter{}.CountTokens(context.Background(), tc.text)
			if err != nil {
				t.Fatalf("CountTokens: %v", err)
			}
			if got != tc.want {
				t.Errorf("CountTokens(%q) = %d, want %d", tc.text, got, tc.want)
			}
		})
	}
}

// TestFakeTokenCounter_ImplementsTokenCounter is a compile-time assertion with a name, so the day
// the interface changes the failure points at the fake instead of at whichever caller broke first.
func TestFakeTokenCounter_ImplementsTokenCounter(t *testing.T) {
	var _ TokenCounter = FakeTokenCounter{}
}

// TestCountingTokenCounter_Counts pins what D-25 needs measured: a call that stops incrementing
// calls, bytes or elapsed time would report a number an operator could not trust to decide whether
// chunking needs memoizing.
func TestCountingTokenCounter_Counts(t *testing.T) {
	counted := NewCountingTokenCounter(FakeTokenCounter{})

	if _, err := counted.CountTokens(context.Background(), "one two three"); err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	if _, err := counted.CountTokens(context.Background(), "four five"); err != nil {
		t.Fatalf("CountTokens: %v", err)
	}

	got := counted.Snapshot()
	if got.Calls != 2 {
		t.Errorf("Calls = %d, want 2", got.Calls)
	}
	if want := int64(len("one two three") + len("four five")); got.Bytes != want {
		t.Errorf("Bytes = %d, want %d", got.Bytes, want)
	}
	if got.Spent <= 0 {
		t.Errorf("Spent = %v, want > 0", got.Spent)
	}
}

// failingCounter is the tokenizer being unreachable. Every pass that counts tokens must surface
// this rather than proceeding with a guess.
type failingCounter struct{}

func (failingCounter) CountTokens(context.Context, string) (int, error) {
	return 0, errors.New("tokenizer is down")
}

// tokenizeServer stands in for scripts/embedder-service/server.py. It records the last request body
// so a test can assert the wire shape ADR-001 fixed, and answers with whatever the case needs.
func tokenizeServer(t *testing.T, handler http.HandlerFunc) (*HTTPTokenCounter, *string) {
	t.Helper()
	var lastBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		lastBody = string(raw)
		if r.URL.Path != "/tokenize" {
			t.Errorf("request path = %q, want /tokenize", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("request method = %q, want POST", r.Method)
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	tc, err := NewHTTPTokenCounter(srv.URL)
	if err != nil {
		t.Fatalf("NewHTTPTokenCounter(%q): %v", srv.URL, err)
	}
	return tc, &lastBody
}

// TestHTTPTokenCounter_SendsContractRequestAndReturnsCount pins both halves of the ADR-001 §7.2
// contract in one place: the request shape sent and the field read back. A change to either is a
// change to the agreement with the running service, not a refactor.
func TestHTTPTokenCounter_SendsContractRequestAndReturnsCount(t *testing.T) {
	tc, body := tokenizeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"counts":[10]}`)
	})

	got, err := tc.CountTokens(context.Background(), "como configurar o cron do n8n")
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	if got != 10 {
		t.Errorf("CountTokens = %d, want 10", got)
	}
	if want := `{"inputs":["como configurar o cron do n8n"]}`; *body != want {
		t.Errorf("request body = %s, want %s", *body, want)
	}
}

// TestHTTPTokenCounter_EscapesText proves the body is built by the JSON encoder and not by string
// concatenation: a note containing a quote or a newline is ordinary, and an unescaped one would
// make the service answer 400 for content it can perfectly well tokenize.
func TestHTTPTokenCounter_EscapesText(t *testing.T) {
	tc, body := tokenizeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"counts":[7]}`)
	})

	if _, err := tc.CountTokens(context.Background(), "a \"quoted\"\nline\\"); err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	var decoded struct {
		Inputs []string `json:"inputs"`
	}
	if err := json.Unmarshal([]byte(*body), &decoded); err != nil {
		t.Fatalf("the request body is not valid JSON (%s): %v", *body, err)
	}
	if len(decoded.Inputs) != 1 || decoded.Inputs[0] != "a \"quoted\"\nline\\" {
		t.Errorf("round-tripped inputs = %q, want the text unchanged", decoded.Inputs)
	}
}

func TestHTTPTokenCounter_ServiceFailures_ReturnError(t *testing.T) {
	tests := map[string]http.HandlerFunc{
		"HTTP 500": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"RuntimeError: CUDA out of memory"}`)
		},
		"body is not JSON": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `not json`)
		},
		// One text was sent, so any other number of counts answers a different question — pairing
		// the clamp with another text's length is exactly the silent divergence this counter exists
		// to prevent.
		"wrong number of counts": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"counts":[10,3]}`)
		},
		"no counts at all": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{}`)
		},
	}

	for name, handler := range tests {
		t.Run(name, func(t *testing.T) {
			tc, _ := tokenizeServer(t, handler)
			if n, err := tc.CountTokens(context.Background(), "text"); err == nil {
				t.Fatalf("CountTokens = %d, want an error", n)
			}
		})
	}
}

// TestHTTPTokenCounter_UnreachableService_ReturnsError is the case that must never be approximated
// away: a tokenizer that is down has to fail the chunking, not fall back to counting words.
func TestHTTPTokenCounter_UnreachableService_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	tc, err := NewHTTPTokenCounter(url)
	if err != nil {
		t.Fatalf("NewHTTPTokenCounter: %v", err)
	}
	if n, err := tc.CountTokens(context.Background(), "text"); err == nil {
		t.Fatalf("CountTokens against a closed server = %d, want an error", n)
	}
}

// TestHTTPTokenCounter_UnbuildableRequest_ReturnsError reaches the request-construction failure by
// bypassing the constructor, which is the only way to hold an endpoint that parses here and not
// there. Constructed literally rather than via NewHTTPTokenCounter on purpose.
func TestHTTPTokenCounter_UnbuildableRequest_ReturnsError(t *testing.T) {
	tc := &HTTPTokenCounter{endpoint: ":", client: &http.Client{}}

	if n, err := tc.CountTokens(context.Background(), "text"); err == nil {
		t.Fatalf("CountTokens with an unbuildable request = %d, want an error", n)
	}
}

func TestNewHTTPTokenCounter_RejectsBadEndpoints(t *testing.T) {
	tests := map[string]string{
		"empty":           "",
		"no scheme":       "127.0.0.1:7999",
		"wrong scheme":    "ftp://127.0.0.1:7999",
		"no host":         "http://",
		"unparseable URL": "http://[::1",
	}

	for name, endpoint := range tests {
		t.Run(name, func(t *testing.T) {
			if tc, err := NewHTTPTokenCounter(endpoint); err == nil {
				t.Fatalf("NewHTTPTokenCounter(%q) = %v, want an error", endpoint, tc)
			}
		})
	}
}

// TestNewHTTPTokenCounter_TrimsTrailingSlash keeps the built URL from becoming //tokenize, which
// the service answers 404 for.
func TestNewHTTPTokenCounter_TrimsTrailingSlash(t *testing.T) {
	tc, err := NewHTTPTokenCounter("  http://127.0.0.1:7999/  ")
	if err != nil {
		t.Fatalf("NewHTTPTokenCounter: %v", err)
	}
	if tc.endpoint != "http://127.0.0.1:7999" {
		t.Errorf("endpoint = %q, want %q", tc.endpoint, "http://127.0.0.1:7999")
	}
}

// TestHTTPTokenCounter_ImplementsTokenCounter names the assertion, so the day the interface changes
// the failure points here instead of at whichever caller broke first.
func TestHTTPTokenCounter_ImplementsTokenCounter(t *testing.T) {
	var _ TokenCounter = (*HTTPTokenCounter)(nil)
}

// TestHTTPTokenCounter_RetriesTransportFailureNotRejection pins the asymmetry that review found:
// this client and internal/embed's talk to the same process, and restarting that process is a
// normal operational act. A transport failure must be ridden through; an HTTP 400 must not, because
// asking the same rejected question again only wastes the operator's time.
func TestHTTPTokenCounter_RetriesTransportFailureNotRejection(t *testing.T) {
	t.Run("recovers from a connection that fails once", func(t *testing.T) {
		var calls int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			if calls == 1 {
				// Hijack and close without a response: the transport-level failure a restarting
				// server produces, not a status code.
				hj, ok := w.(http.Hijacker)
				if !ok {
					t.Fatal("no hijacker")
				}
				c, _, _ := hj.Hijack()
				_ = c.Close()
				return
			}
			_, _ = w.Write([]byte(`{"counts":[7]}`))
		}))
		defer srv.Close()

		tc, err := NewHTTPTokenCounter(srv.URL)
		if err != nil {
			t.Fatalf("new: %v", err)
		}
		n, err := tc.CountTokens(context.Background(), "texto")
		if err != nil {
			t.Fatalf("want recovery, got %v", err)
		}
		if n != 7 || calls != 2 {
			t.Errorf("got n=%d after %d call(s), want 7 after 2", n, calls)
		}
	})

	t.Run("does not retry a 400", func(t *testing.T) {
		var calls int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"input exceeds the model window"}`))
		}))
		defer srv.Close()

		tc, _ := NewHTTPTokenCounter(srv.URL)
		if _, err := tc.CountTokens(context.Background(), "texto"); err == nil {
			t.Fatal("want error")
		}
		if calls != 1 {
			t.Errorf("the service rejected the input %d times; want 1 — a verdict is not a blip", calls)
		}
	})
}

// TestHTTPTokenCounter_CancellationDuringRetryDelay covers the one path the two subtests above
// cannot reach: the caller gives up while the client is waiting to try again. Without this, the
// retry would keep a cancelled ingestion alive for the length of its own delay budget.
func TestHTTPTokenCounter_CancellationDuringRetryDelay(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("no hijacker")
		}
		c, _, _ := hj.Hijack()
		_ = c.Close()
	}))
	defer srv.Close()

	tc, _ := NewHTTPTokenCounter(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()

	start := time.Now()
	_, err := tc.CountTokens(ctx, "texto")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	// It must abandon the wait, not sit through the full delay budget of every remaining attempt.
	if elapsed := time.Since(start); elapsed > tokenizeDelay*tokenizeAttempts {
		t.Errorf("returned after %v; a cancelled caller should not wait out the retry budget", elapsed)
	}
}
