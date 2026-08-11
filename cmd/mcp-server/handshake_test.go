package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danielmalka/go-knowrag/internal/embed"
)

// stubTransport is the wire for the startup handshake, so these tests need no embedding service, no
// GPU and no socket. Embed is never called — verifyEmbedder only ever asks for Info — and says so
// rather than returning something plausible, because a startup check that embedded anything would be
// a different (and wrong) design.
type stubTransport struct {
	info embed.BackendHandshakeInfo
	err  error
	// hangs is a backend that accepts the connection and then never speaks: the black-holed case,
	// where nothing but a deadline on this side ends the call.
	hangs bool

	// budget is how much time the call was actually given, recorded where it can be observed: at the
	// wire, after every layer that could have narrowed it. It is the only way to see the bound this
	// package *thinks* it set, since Handshake installs its profile's Timeout over the caller's
	// context and the smaller of the two silently wins.
	budget time.Duration

	// infoCalls counts how many times the backend was asked about its config, which is the only
	// observable difference between "checked once" and any form of re-verification.
	infoCalls int
}

func (s *stubTransport) Embed(context.Context, []string, embed.Kind) ([]embed.Embedding, error) {
	return nil, errors.New("stubTransport: the startup handshake must not embed anything")
}

func (s *stubTransport) Info(ctx context.Context) (embed.BackendHandshakeInfo, error) {
	s.infoCalls++
	if deadline, ok := ctx.Deadline(); ok {
		s.budget = time.Until(deadline)
	}
	if s.hangs {
		select {
		case <-ctx.Done():
			return embed.BackendHandshakeInfo{}, ctx.Err()
		case <-time.After(hangCap):
			// Same reasoning as fakeSearcher's: returning rather than blocking forever turns a boot
			// nothing bounds into a failed assertion instead of a test binary that hangs until the
			// package timeout kills it.
			return embed.BackendHandshakeInfo{}, errors.New("the handshake was never cancelled: nothing bounds how long boot may wait")
		}
	}
	return s.info, s.err
}

// TestVerifyEmbedder_DivergentBackend_RefusesToStart is the half of D-32 that has to fail closed. A
// backend that answers with a different pooling is not an outage: it is a system assembled wrong,
// and a server that started here would embed every query in a space the index was never built in
// and report nothing at all.
func TestVerifyEmbedder_DivergentBackend_RefusesToStart(t *testing.T) {
	info := embed.Expected()
	info.Pooling = "mean"

	err := verifyEmbedder(context.Background(), testConfig(), &stubTransport{info: info})
	if err == nil {
		t.Fatal("the server started against a backend whose config diverges from this build's pins")
	}
	if !errors.Is(err, embed.ErrHandshake) {
		t.Errorf("the refusal is not a handshake failure: %v", err)
	}
	// Naming the field is the whole operational value: "the handshake failed" sends an operator
	// diffing two configurations by hand.
	for _, want := range []string{"Pooling", `"mean"`, `"cls"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s:\n%v", want, err)
		}
	}
}

// TestVerifyEmbedder_BootBudget_OutlastsAModelLoad is the bound this check actually runs under, and
// it is asserted at the wire because that is the only place it is visible. Handshake installs its
// profile's Timeout on top of the context it is handed, so a caller that "gave boot 30 s" by wrapping
// the context in a deadline gets min(30 s, profile) and no error saying which won.
//
// The floor is the failure it prevents: BGE-M3 takes ~11 s to load warm (ADR-001 §6.2), and a check
// that gave up before that would classify the slow answer as an outage, warn, and skip itself —
// silently, on precisely the cold start where this process and the service come up together, which
// is the likeliest boot there is.
func TestVerifyEmbedder_BootBudget_OutlastsAModelLoad(t *testing.T) {
	tr := &stubTransport{info: embed.Expected()}
	if err := verifyEmbedder(context.Background(), testConfig(), tr); err != nil {
		t.Fatalf("a backend matching every pin was refused: %v", err)
	}

	if search := embedProfile(testConfig().EmbedderEndpoint).Timeout; tr.budget <= search {
		t.Errorf("the boot check ran on %v, the search path's per-attempt budget of %v or less: "+
			"a number tuned for how long an agent waits for an answer is bounding startup", tr.budget, search)
	}
	// 11 s is the measured warm load, not a round number: a budget under it makes a booting service
	// indistinguishable from a missing one.
	if tr.budget < 11*time.Second {
		t.Errorf("the boot check ran on %v, less than the ~11 s BGE-M3 takes to load: a backend that "+
			"is merely still loading would come back as an outage and the verification would be skipped",
			tr.budget)
	}
}

// TestVerifyEmbedder_UnreachableBackend_StartsAnyway is D-21 held in place. The embedding service
// runs beside this process and is restarted, upgraded and occasionally simply down; dying at boot
// for that would take the server away exactly when the per-search unavailability message is the only
// thing telling the model not to answer from memory.
func TestVerifyEmbedder_UnreachableBackend_StartsAnyway(t *testing.T) {
	logged := captureLogs(t)
	down := &stubTransport{err: errors.New("dial tcp 127.0.0.1:8080: connect: connection refused")}

	if err := verifyEmbedder(context.Background(), testConfig(), down); err != nil {
		t.Fatalf("a momentarily unreachable embedder aborted startup, which is D-21 reintroduced: %v", err)
	}

	// The operator half: the check did not run, and the log has to say so. A server that silently
	// skipped it is back to the state D-32 describes, only with a different reason.
	//
	// "never again" and the restart are in that list because they are what an operator would
	// otherwise assume the other way round: a warning about a service being down reads as something
	// that resolves itself when the service comes back, and this one does not.
	for _, want := range []string{
		"could not confirm", "starting unverified", "connection refused",
		"never again", "restart", testConfig().EmbedderEndpoint,
	} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("the startup warning omits %q:\n%s", want, logged.String())
		}
	}
	if down.infoCalls != 1 {
		t.Errorf("the backend was asked %d times, want exactly 1", down.infoCalls)
	}
}

// TestVerifyEmbedder_SkippedCheck_IsNeverRetried states the residual this change leaves behind, so
// that it is a decision on record rather than something a reader has to infer from the absence of
// code. The check runs once: no retry inside the call, no goroutine, no re-verification on the first
// search. An embedder that comes back on a different revision after a skipped boot check is never
// caught, and the searches that follow look perfectly healthy.
//
// Widening this — a lazy re-check on first search, a ticker — is the registered follow-up and not
// this change. Whoever builds it will land here first, which is the point.
func TestVerifyEmbedder_SkippedCheck_IsNeverRetried(t *testing.T) {
	captureLogs(t)
	down := &stubTransport{err: errors.New("dial tcp 127.0.0.1:8080: connect: connection refused")}

	if err := verifyEmbedder(context.Background(), testConfig(), down); err != nil {
		t.Fatalf("an unreachable embedder aborted startup: %v", err)
	}
	// One attempt, not two: bootProfile sets MaxRetries to 1 and Handshake makes exactly one request
	// per call, so a second ask here means something re-verified.
	if down.infoCalls != 1 {
		t.Errorf("the skipped check was retried: the backend was asked %d times, want exactly 1", down.infoCalls)
	}

	// The server serves searches afterwards without the handshake ever being asked again. The
	// searcher is a fake, so what this pins is that nothing in this package's request path reaches
	// for the handshake — which is what makes the skip total rather than deferred.
	cs := connect(t, testConfig(), &fakeSearcher{results: sampleResults()})
	if res := callRaw(t, cs, `{"query":"anything"}`); res.IsError {
		t.Fatalf("the call failed: %s", resultText(t, res))
	}
	if down.infoCalls != 1 {
		t.Errorf("a search re-ran the handshake: the backend was asked %d times, want exactly 1", down.infoCalls)
	}
}

// TestVerifyEmbedder_WireFailures pins where an answer this build cannot use actually goes, because
// it is not obvious from the code and the three cases do not agree.
//
// A 500 and a page of HTML from the wrong port fail in the transport, arrive wrapped as ErrBackend,
// and take the same branch as a refused connection: the server starts unverified. A body that
// decodes cleanly to `{}` never touches that path — it arrives as a report whose every field is
// empty, and embed's policy that an unreported field is a divergence rather than consent (ADR-001
// §4) refuses it by name.
//
// That asymmetry is deliberate policy, not a gap, and neither half is changed here. The table exists
// so the next person to touch this reads which is which off one screen instead of inferring it from
// three files, and sees a red test if they move a case across the line.
func TestVerifyEmbedder_WireFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		refuses bool   // whether the server declines to start at all
		want    string // what the operator has to be told, in the warning or in the refusal
	}{
		{
			name:    "an error status",
			handler: func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "model not loaded", 500) },
			want:    "HTTP 500",
		},
		{
			// The wrong port, or a /handshake whose schema changed under this build.
			name:    "a body that is not the handshake",
			handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>not this service</html>")) },
			want:    "decode response",
		},
		{
			// A service that answers about its own config with nothing is not one to embed with, so
			// this is the case that fails closed rather than starting unverified.
			name:    "a well-formed answer that reports nothing",
			handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) },
			refuses: true,
			want:    "did not report ModelRevision",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logged := captureLogs(t)
			backend := httptest.NewServer(tc.handler)
			t.Cleanup(backend.Close)

			cfg := testConfig()
			cfg.EmbedderEndpoint = backend.URL
			transport, err := embed.NewHTTPTransport(backend.URL)
			if err != nil {
				t.Fatalf("building the transport failed: %v", err)
			}

			err = verifyEmbedder(context.Background(), cfg, transport)
			if tc.refuses {
				if err == nil {
					t.Fatal("the server started against a backend that reported no config at all")
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Errorf("the refusal does not name what was missing (%q): %v", tc.want, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("an unreadable answer aborted startup: %v", err)
			}
			if !strings.Contains(logged.String(), tc.want) {
				t.Errorf("the warning does not say what came back (%q):\n%s", tc.want, logged.String())
			}
			// The wording is the fix: this backend answered, and a warning asserting silence sends an
			// operator to check whether the service is running when it is running and replying.
			if !strings.Contains(logged.String(), "could not confirm") {
				t.Errorf("the warning does not say the backend went unconfirmed:\n%s", logged.String())
			}
			if strings.Contains(logged.String(), "did not answer") {
				t.Errorf("the warning claims nothing answered, but the backend replied:\n%s", logged.String())
			}
		})
	}
}

// TestVerifyEmbedder_MatchingBackend_StartsQuietly is the half that decides whether the warning is
// worth reading. A warning on every healthy boot is a warning nobody reads on the boot that matters.
func TestVerifyEmbedder_MatchingBackend_StartsQuietly(t *testing.T) {
	logged := captureLogs(t)

	if err := verifyEmbedder(context.Background(), testConfig(), &stubTransport{info: embed.Expected()}); err != nil {
		t.Fatalf("a backend matching every pin was refused: %v", err)
	}
	if logged.Len() != 0 {
		t.Errorf("a healthy startup logged something:\n%s", logged.String())
	}
}

// TestVerifyEmbedder_HungBackend_BoundsBoot covers the failure mode that has no error to classify: a
// connection that is accepted and then never answered. Without a bound on this side the process
// never reaches Run, so it is not down, not up, and not visible to the client either.
//
// The fake tells the two outcomes apart by which error it returns, so this fails rather than merely
// runs slowly when nothing bounds boot — and shrinking bootHandshakeTimeout is what proves the bound
// is the one this package sets, not one it inherited.
func TestVerifyEmbedder_HungBackend_BoundsBoot(t *testing.T) {
	restore := bootHandshakeTimeout
	bootHandshakeTimeout = 100 * time.Millisecond
	t.Cleanup(func() { bootHandshakeTimeout = restore })

	logged := captureLogs(t)
	if err := verifyEmbedder(context.Background(), testConfig(), &stubTransport{hangs: true}); err != nil {
		t.Fatalf("a black-holed backend is unreachable, not divergent, and must not abort startup: %v", err)
	}
	if strings.Contains(logged.String(), "never cancelled") {
		t.Errorf("the startup handshake ran on a budget this package does not control:\n%s", logged.String())
	}
}

// TestRun_DivergentBackend_NeverServes is the wiring, and it is the defect D-32 actually was: the
// check existed, correct and tested, in a package that never called it. Everything above proves what
// verifyEmbedder decides; only this proves run() asks.
//
// Hermetic: the embedder is an httptest server on the loopback and Qdrant is never dialled — the
// client is lazy, and run returns at the handshake, before the first search would need it.
func TestRun_DivergentBackend_NeverServes(t *testing.T) {
	// run() installs its own default logger; captureLogs both keeps that off the test output and
	// restores the original on cleanup.
	captureLogs(t)

	// Everything matches except the revision, which is the upgrade this whole check is about: the
	// operator moved the service to a new model and the index still holds vectors from the old one.
	// Building the body from the pins keeps the divergence to that one field by construction.
	want := embed.Expected()
	body := fmt.Sprintf(
		`{"model_revision":%q,"tokenizer_revision":%q,"dense_dim":%d,"normalized":true,`+
			`"pooling":%q,"precision":%q,"sparse":{"kind":%q,"id_space":%q}}`,
		"0000000000000000000000000000000000000000", want.TokenizerRevision, want.Dim,
		want.Pooling, want.Precision, want.SparseParams["kind"], want.SparseParams["id_space"])

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/handshake" {
			t.Errorf("startup called %s; only the handshake belongs on this path", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(backend.Close)

	t.Setenv(envCollection, "interno")
	t.Setenv(envTenantID, "malka")
	t.Setenv(envQdrantEndpoint, "127.0.0.1:6334")
	t.Setenv(envQdrantAPIKey, "runtime-read-key")
	t.Setenv(envEmbedderEndpoint, backend.URL)
	t.Setenv(envAreas, "infra,research")

	err := run()
	if err == nil {
		t.Fatal("run() served a knowledge base whose index was built by a different model")
	}
	if !errors.Is(err, embed.ErrHandshake) || !strings.Contains(err.Error(), "ModelRevision") {
		t.Errorf("run() failed for some other reason than the diverging revision: %v", err)
	}
}
