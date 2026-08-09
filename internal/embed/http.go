package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// HTTPTransport is the client for scripts/embedder-service/server.py, the runtime ADR-001 §7.2
// settled on: a thin HTTP shell of our own around FlagEmbedding. TEI serves an ONNX graph that
// never had BGE-M3's sparse head exported into it, and Infinity does not start on current
// dependencies — so the sparse half of every vector, which PRD-contrato §2.3b requires, has exactly
// one source here.
//
// It does nothing but speak the wire: encode the request, decode the response, translate the two
// fields where the server's vocabulary differs from the contract's. It never sorts, filters or
// repairs a vector — ServiceEmbedder validates, and a transport that tidied up first would make
// those checks unreachable against real output.
type HTTPTransport struct {
	endpoint string
	client   *http.Client
}

// Kind is the /embed request's `kind` field. BGE-M3 uses no instruction prefix, so the server
// currently produces the same vector either way; the field is sent because it is part of the
// documented request, and because the day a runtime does distinguish them, a query embedded as a
// passage is a recall bug with no error attached.
type Kind string

const (
	KindQuery   Kind = "query"
	KindPassage Kind = "passage"
)

// NewHTTPTransport validates the endpoint at construction: a typo in a URL should fail where an
// operator is reading a config error, not on the first embedding of an ingestion run.
func NewHTTPTransport(endpoint string) (*HTTPTransport, error) {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("embed: endpoint %q is not a URL: %w", endpoint, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf(
			"embed: endpoint %q must be an http(s) URL with a host, e.g. http://127.0.0.1:7999", endpoint)
	}
	// No Timeout on the client: ServiceEmbedder bounds every attempt with a context deadline, and a
	// second, independent clock here would produce two different errors for one condition.
	return &HTTPTransport{endpoint: endpoint, client: &http.Client{}}, nil
}

// embedRequest / embedResponse mirror POST /embed, verified against the running service on
// 2026-08-09. `tokens` is decoded but unused: Embedding has nowhere to put it, and S03's
// clamp — the one consumer that wants token counts — asks /tokenize instead. See the note in
// the package report about the two clients sharing this transport.
type embedRequest struct {
	Inputs []string `json:"inputs"`
	Kind   Kind     `json:"kind"`
}

type embedResponse struct {
	Dense  [][]float32 `json:"dense"`
	Sparse []struct {
		Indices []uint32  `json:"indices"`
		Values  []float32 `json:"values"`
	} `json:"sparse"`
	Tokens []int `json:"tokens"`
}

func (t *HTTPTransport) Embed(ctx context.Context, texts []string, kind Kind) ([]Embedding, error) {
	body, err := json.Marshal(embedRequest{Inputs: texts, Kind: kind})
	if err != nil {
		return nil, fmt.Errorf("embed: encoding request: %w", err)
	}

	var resp embedResponse
	if err := t.do(ctx, http.MethodPost, "/embed", body, &resp); err != nil {
		return nil, err
	}
	if len(resp.Dense) != len(resp.Sparse) {
		return nil, fmt.Errorf(
			"embed: response has %d dense vectors but %d sparse ones; BGE-M3 produces both in one "+
				"pass, so they cannot legitimately differ", len(resp.Dense), len(resp.Sparse))
	}

	out := make([]Embedding, len(resp.Dense))
	for i := range resp.Dense {
		s := resp.Sparse[i]
		// Checked here rather than left to validateEmbedding because a length mismatch is a
		// response-shape defect: the pair never described a vector, so there is nothing to validate.
		if len(s.Indices) != len(s.Values) {
			return nil, fmt.Errorf(
				"embed: item %d: sparse indices/values length mismatch (%d vs %d)",
				i, len(s.Indices), len(s.Values))
		}
		out[i] = Embedding{Dense: resp.Dense[i], Sparse: Sparse{Indices: s.Indices, Values: s.Values}}
	}
	return out, nil
}

// handshakeResponse mirrors GET /handshake. Normalized is a pointer so an absent key stays
// distinguishable from an explicit false: absent means the backend did not report the field, which
// Handshake treats as a divergence, while false is a backend that reports it is not normalizing.
type handshakeResponse struct {
	ModelRevision     string            `json:"model_revision"`
	TokenizerRevision string            `json:"tokenizer_revision"`
	DenseDim          int               `json:"dense_dim"`
	Normalized        *bool             `json:"normalized"`
	Pooling           string            `json:"pooling"`
	Precision         string            `json:"precision"`
	Sparse            map[string]string `json:"sparse"`
}

// dtypeNames maps torch's dtype spelling to the vocabulary ADR-001 §4 row 7 fixes
// (fp32 | fp16 | int8). The translation lives here, at the wire, for one reason: this value goes
// into point_hash, so it must not change the day the server's spelling does. An unrecognised dtype
// passes through unchanged, so it diverges loudly against the pin instead of being flattened to ""
// and reported as "the backend did not tell us".
var dtypeNames = map[string]string{
	"float32":  "fp32",
	"float16":  "fp16",
	"bfloat16": "bf16",
}

func (t *HTTPTransport) Info(ctx context.Context) (BackendHandshakeInfo, error) {
	var resp handshakeResponse
	if err := t.do(ctx, http.MethodGet, "/handshake", nil, &resp); err != nil {
		return BackendHandshakeInfo{}, err
	}

	precision := resp.Precision
	if canonical, ok := dtypeNames[precision]; ok {
		precision = canonical
	}

	// The server reports normalization as a boolean; the contract names the method. It asserts the
	// dense norm against a real vector at startup and refuses to boot otherwise, so `true` here is
	// L2 measured, not L2 declared.
	normalization := ""
	if resp.Normalized != nil {
		normalization = "none"
		if *resp.Normalized {
			normalization = "l2"
		}
	}

	return BackendHandshakeInfo{
		ModelRevision:     resp.ModelRevision,
		TokenizerRevision: resp.TokenizerRevision,
		Dim:               resp.DenseDim,
		Normalization:     normalization,
		Pooling:           resp.Pooling,
		Precision:         precision,
		SparseParams:      resp.Sparse,
	}, nil
}

// maxErrorBody caps how much of a failure response is read into an error message. The service is
// local and returns small JSON, but an error path that reads an unbounded body is how a wrong URL
// pointed at something else turns into a memory problem.
const maxErrorBody = 4 << 10

func (t *HTTPTransport) do(ctx context.Context, method, path string, body []byte, out any) error {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, t.endpoint+path, rdr)
	if err != nil {
		return fmt.Errorf("embed: building %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("embed: %s %s%s: %w", method, t.endpoint, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return fmt.Errorf("embed: %s %s%s: HTTP %d: %s",
			method, t.endpoint, path, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	// Streamed, not read whole: an ingestion sub-batch of 32 chunks is ~130k floats, and buffering
	// the body only to hand it to the same decoder doubles that for nothing.
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("embed: %s %s%s: decode response: %w", method, t.endpoint, path, err)
	}
	return nil
}
