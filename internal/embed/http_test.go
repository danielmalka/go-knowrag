package embed

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/schema"
)

// liveHandshakeJSON is the exact body scripts/embedder-service/server.py returned on 2026-08-09.
// Pasted verbatim rather than built from a struct: a fixture assembled from the same types the
// decoder uses proves the decoder agrees with itself, not with the server.
const liveHandshakeJSON = `{"model_id": "BAAI/bge-m3", "model_revision": "5617a9f61b028005a4858fdac845db406aefb181", "tokenizer_revision": "5617a9f61b028005a4858fdac845db406aefb181", "dense_dim": 1024, "normalized": true, "pooling": "cls", "precision": "float16", "sparse": {"kind": "lexical_weights", "id_space": "tokenizer_vocab"}, "max_position_embeddings": 8194}`

func newTestTransport(t *testing.T, h http.HandlerFunc) *HTTPTransport {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	tr, err := NewHTTPTransport(srv.URL)
	if err != nil {
		t.Fatalf("NewHTTPTransport: %v", err)
	}
	return tr
}

func TestNewHTTPTransport_RejectsUnusableEndpoint(t *testing.T) {
	for _, endpoint := range []string{"", "   ", "127.0.0.1:7999", "ftp://host/x", "http://"} {
		if _, err := NewHTTPTransport(endpoint); err == nil {
			t.Errorf("endpoint %q was accepted", endpoint)
		}
	}
}

func TestHTTPTransport_Info_DecodesTheLiveHandshakeBody(t *testing.T) {
	var gotPath, gotMethod string
	tr := newTestTransport(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		_, _ = io.WriteString(w, liveHandshakeJSON)
	})

	got, err := tr.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if gotMethod != http.MethodGet || gotPath != "/handshake" {
		t.Errorf("called %s %s, want GET /handshake", gotMethod, gotPath)
	}

	want := BackendHandshakeInfo{
		ModelRevision:     schema.BGEM3Revision,
		TokenizerRevision: schema.BGEM3Revision,
		Dim:               1024,
		Normalization:     "l2",
		Pooling:           "cls",
		Precision:         "fp16",
		SparseParams:      map[string]string{"kind": "lexical_weights", "id_space": "tokenizer_vocab"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Info() = %+v\nwant %+v", got, want)
	}

	// The decoded report must satisfy the pins, or the client cannot talk to the server it was
	// written against — which is the whole point of pinning them.
	if err := validateHandshake(got, Expected()); err != nil {
		t.Errorf("the live server's own handshake body fails this build's pins: %v", err)
	}
}

// TestHTTPTransport_Info_TranslatesBackendVocabulary covers the two fields where the server's
// spelling is not the contract's. The translation is here, at the wire, so the value that reaches
// point_hash does not change if the server's spelling ever does.
func TestHTTPTransport_Info_TranslatesBackendVocabulary(t *testing.T) {
	cases := []struct {
		name           string
		body           string
		wantPrecision  string
		wantNormalized string
	}{
		{"torch float16", `{"precision":"float16","normalized":true}`, "fp16", "l2"},
		{"torch float32", `{"precision":"float32","normalized":true}`, "fp32", "l2"},
		{"torch bfloat16", `{"precision":"bfloat16","normalized":true}`, "bf16", "l2"},
		{"already canonical", `{"precision":"int8","normalized":true}`, "int8", "l2"},
		// An unknown dtype must survive unchanged so it diverges loudly against the pin, rather
		// than being mapped to "" and reported as "the backend did not tell us".
		{"unknown dtype passes through", `{"precision":"float8_e4m3fn","normalized":true}`, "float8_e4m3fn", "l2"},
		{"normalization off", `{"precision":"float16","normalized":false}`, "fp16", "none"},
		{"normalization not reported", `{"precision":"float16"}`, "fp16", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := newTestTransport(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tc.body)
			})
			got, err := tr.Info(context.Background())
			if err != nil {
				t.Fatalf("Info: %v", err)
			}
			if got.Precision != tc.wantPrecision {
				t.Errorf("Precision = %q, want %q", got.Precision, tc.wantPrecision)
			}
			if got.Normalization != tc.wantNormalized {
				t.Errorf("Normalization = %q, want %q", got.Normalization, tc.wantNormalized)
			}
		})
	}
}

func TestHTTPTransport_Info_FailsOnBadResponse(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		wantSub string
	}{
		{
			name: "server error",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"error":"RuntimeError: CUDA out of memory"}`)
			},
			wantSub: "CUDA out of memory",
		},
		{
			name: "not found",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"error":"no such endpoint","path":"/handshake"}`)
			},
			wantSub: "404",
		},
		{
			name: "malformed JSON is an error, not a panic",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"dense_dim": tru`)
			},
			wantSub: "decode",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := newTestTransport(t, tc.handler)
			_, err := tr.Info(context.Background())
			if err == nil {
				t.Fatal("no error")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not mention %q", err, tc.wantSub)
			}
		})
	}
}

func TestHTTPTransport_Embed_SendsTheDocumentedRequest(t *testing.T) {
	type request struct {
		Inputs []string `json:"inputs"`
		Kind   string   `json:"kind"`
	}
	var got request
	var gotPath, gotMethod, gotContentType string

	tr := newTestTransport(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotContentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		_, _ = io.WriteString(w, embedBody(2))
	})

	if _, err := tr.Embed(context.Background(), []string{"a", "b"}, KindPassage); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/embed" {
		t.Errorf("called %s %s, want POST /embed", gotMethod, gotPath)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if !reflect.DeepEqual(got.Inputs, []string{"a", "b"}) {
		t.Errorf("inputs = %v, want [a b]", got.Inputs)
	}
	if got.Kind != "passage" {
		t.Errorf("kind = %q, want passage", got.Kind)
	}
}

func TestHTTPTransport_Embed_DecodesDenseAndSparseInOrder(t *testing.T) {
	tr := newTestTransport(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"dense":[[1,2],[3,4]],
			"sparse":[{"indices":[7,9],"values":[0.5,0.25]},{"indices":[1],"values":[0.75]}],
			"tokens":[3,4]}`)
	})

	got, err := tr.Embed(context.Background(), []string{"a", "b"}, KindPassage)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	want := []Embedding{
		{Dense: []float32{1, 2}, Sparse: Sparse{Indices: []uint32{7, 9}, Values: []float32{0.5, 0.25}}},
		{Dense: []float32{3, 4}, Sparse: Sparse{Indices: []uint32{1}, Values: []float32{0.75}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Embed() = %+v\nwant %+v", got, want)
	}
}

// TestHTTPTransport_Embed_ReturnsRawOutput is the transport's half of the T2/T3 boundary: it must
// hand ServiceEmbedder exactly what the server said. A transport that tidied up first would make
// the ordering guarantee untestable from Go — and that guarantee is a contract between the two
// sides, not a detail (an unstable sparse order makes an unchanged note look changed to point_hash).
func TestHTTPTransport_Embed_ReturnsRawOutput(t *testing.T) {
	tr := newTestTransport(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"dense":[[1,2]],
			"sparse":[{"indices":[9,7,7],"values":[0.5,0,0.25]}],"tokens":[3]}`)
	})

	got, err := tr.Embed(context.Background(), []string{"a"}, KindPassage)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	want := Sparse{Indices: []uint32{9, 7, 7}, Values: []float32{0.5, 0, 0.25}}
	if !reflect.DeepEqual(got[0].Sparse, want) {
		t.Errorf("the transport altered the server's sparse vector: got %+v, want %+v", got[0].Sparse, want)
	}
}

func TestHTTPTransport_Embed_FailsOnBadResponse(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		wantSub string
	}{
		{
			name: "dense and sparse disagree on length",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"dense":[[1],[2]],"sparse":[{"indices":[1],"values":[1]}],"tokens":[1,1]}`)
			},
			wantSub: "sparse",
		},
		{
			name: "sparse indices and values disagree on length",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"dense":[[1]],"sparse":[{"indices":[1,2],"values":[1]}],"tokens":[1]}`)
			},
			wantSub: "length",
		},
		{
			name: "malformed JSON is an error, not a panic",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"dense":[[1,`)
			},
			wantSub: "decode",
		},
		{
			name: "server rejected the request",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, "{\"error\":\"`inputs` must be a non-empty array of strings\"}")
			},
			wantSub: "non-empty array",
		},
		{
			name: "server blew up",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"error":"RuntimeError: CUDA out of memory"}`)
			},
			wantSub: "CUDA out of memory",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := newTestTransport(t, tc.handler)
			_, err := tr.Embed(context.Background(), []string{"a", "b"}, KindPassage)
			if err == nil {
				t.Fatal("no error")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not mention %q", err, tc.wantSub)
			}
		})
	}
}

func TestHTTPTransport_RespectsContextCancellation(t *testing.T) {
	tr := newTestTransport(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := tr.Embed(ctx, []string{"a"}, KindQuery); !errors.Is(err, context.Canceled) {
		t.Errorf("Embed on a cancelled context gave %v, want context.Canceled", err)
	}
	if _, err := tr.Info(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Info on a cancelled context gave %v, want context.Canceled", err)
	}
}

// embedBody builds a minimal well-formed /embed response for n inputs.
func embedBody(n int) string {
	var b strings.Builder
	b.WriteString(`{"dense":[`)
	for i := range n {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString("[1,2]")
	}
	b.WriteString(`],"sparse":[`)
	for i := range n {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"indices":[1],"values":[0.5]}`)
	}
	b.WriteString(`],"tokens":[`)
	for i := range n {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString("3")
	}
	b.WriteString("]}")
	return b.String()
}
