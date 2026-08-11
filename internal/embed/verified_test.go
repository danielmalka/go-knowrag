package embed

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// The tests in this file are about one guarantee: nothing is embedded through a backend this
// embedder has not compared against the pins (D-33). They are separate from service_test.go
// because that file's subject is what happens *after* that point — batching, retry, validation —
// and its stub answers the handshake correctly so it can get there.

// TestServiceEmbedder_Embed_VerifiesBeforeSendingAnyText is the property itself, in the order that
// matters. Asserting only that Info was called would pass on an implementation that handshakes
// after embedding, which confirms the backend the moment after it stopped mattering.
func TestServiceEmbedder_Embed_VerifiesBeforeSendingAnyText(t *testing.T) {
	var tr *stubTransport
	tr = &stubTransport{
		embed: func(ctx context.Context, texts []string) ([]Embedding, error) {
			if n := tr.infoCallCount(); n != 1 {
				t.Errorf("the backend was asked to embed after %d handshakes, want exactly 1 before "+
					"the first text goes out", n)
			}
			return echoEmbed(ctx, texts)
		},
	}
	e := newTestEmbedder(t, tr)

	if _, err := e.EmbedQuery(context.Background(), "alpha"); err != nil {
		t.Fatalf("EmbedQuery: %v", err)
	}
}

// TestServiceEmbedder_DivergentBackend_EmbedsNothing is D-32's damage, blocked at the last place it
// can be blocked. A backend on another revision embeds into a different vector space; the call has
// to fail rather than return vectors nobody downstream can tell from good ones.
func TestServiceEmbedder_DivergentBackend_EmbedsNothing(t *testing.T) {
	tr := &stubTransport{
		info: func(context.Context) (BackendHandshakeInfo, error) {
			got := Expected()
			got.ModelRevision = "some-other-commit"
			return got, nil
		},
		embed: func(context.Context, []string) ([]Embedding, error) {
			t.Error("a text was sent to a backend whose configuration diverges from this build's pins")
			return nil, nil
		},
	}
	e := newTestEmbedder(t, tr)

	_, err := e.EmbedQuery(context.Background(), "alpha")
	if !errors.Is(err, ErrHandshake) {
		t.Fatalf("error = %v, want it to match ErrHandshake so a caller can tell a divergence from an "+
			"outage — only one of the two is worth retrying", err)
	}
}

// TestServiceEmbedder_Verification_HappensOnce keeps the guarantee from becoming a per-call round
// trip. The handshake is not free and the answer does not change between two embeds.
func TestServiceEmbedder_Verification_HappensOnce(t *testing.T) {
	tr := &stubTransport{}
	e := newTestEmbedder(t, tr)

	for i := range 5 {
		if _, err := e.EmbedQuery(context.Background(), "alpha"); err != nil {
			t.Fatalf("EmbedQuery %d: %v", i, err)
		}
	}
	if n := tr.infoCallCount(); n != 1 {
		t.Errorf("handshakes = %d, want 1: the latch is not holding, so every embedding pays a round "+
			"trip for an answer already known", n)
	}
}

// TestServiceEmbedder_ExplicitHandshake_SatisfiesTheLatch is why cmd/cli pays nothing for this.
// It handshakes at startup because it needs the report for point_hash; that same call must be what
// the latch counts, or the ingestion would repeat it on its first batch.
//
// What it does NOT pin, said plainly because a reader would assume otherwise: it would pass just as
// happily if ensureVerified were deleted outright, since the explicit call alone produces one
// handshake either way. It catches the narrower defect of Handshake not arming the latch. The tests
// above are what fail when the verification itself goes missing — this one is about the cost, not
// about the guarantee.
func TestServiceEmbedder_ExplicitHandshake_SatisfiesTheLatch(t *testing.T) {
	tr := &stubTransport{}
	e := newTestEmbedder(t, tr)

	if _, err := e.Handshake(context.Background()); err != nil {
		t.Fatalf("Handshake: %v", err)
	}
	if _, err := e.EmbedDocuments(context.Background(), []string{"alpha", "beta"}); err != nil {
		t.Fatalf("EmbedDocuments: %v", err)
	}
	if n := tr.infoCallCount(); n != 1 {
		t.Errorf("handshakes = %d, want 1: the explicit startup handshake did not count, so every "+
			"caller that follows the documented contract now pays for it twice", n)
	}
}

// TestServiceEmbedder_FailedVerification_DoesNotLatch is D-33 itself.
//
// The sequence is ordinary: updating the embedding service means stop, swap, start, and an MCP
// client relaunches its server whenever it likes — including during the stop. The server boots, its
// startup check cannot reach the backend, and it starts anyway because refusing to boot through an
// outage is what D-21 exists to prevent. What must not follow is a process that spends the rest of
// its life never confirming the backend it came back to.
//
// A sync.Once would fail exactly here, which is why the implementation does not use one.
func TestServiceEmbedder_FailedVerification_DoesNotLatch(t *testing.T) {
	down := errors.New("connection refused")
	var mu sync.Mutex
	up := false

	tr := &stubTransport{
		info: func(context.Context) (BackendHandshakeInfo, error) {
			mu.Lock()
			defer mu.Unlock()
			if !up {
				return BackendHandshakeInfo{}, down
			}
			return Expected(), nil
		},
	}
	e := newTestEmbedder(t, tr)

	_, err := e.EmbedQuery(context.Background(), "alpha")
	if !errors.Is(err, ErrBackend) {
		t.Fatalf("with the backend down, error = %v, want ErrBackend", err)
	}

	mu.Lock()
	up = true
	mu.Unlock()

	if _, err := e.EmbedQuery(context.Background(), "alpha"); err != nil {
		t.Fatalf("the backend came back and the embedder never re-checked it: %v — a verification "+
			"that was skipped once must not stay skipped for the life of the process (D-33)", err)
	}
	if n := tr.infoCallCount(); n != 2 {
		t.Errorf("handshakes = %d, want 2 (one that failed, one that succeeded)", n)
	}
}

// TestServiceEmbedder_Handshake_IsBoundedByVerifyTimeout pins which of the two timeouts actually
// bounds the handshake, at the wire, rather than trusting that the field is read.
//
// It exists because the arithmetic test that guards the search budget
// (cmd/mcp-server's TestSearchDeadline_ExceedsTheEmbedderBudget) reads VerifyTimeout out of the
// profile and adds it up. That proves the numbers are chosen so they fit; it proves nothing about
// whether Handshake honours the number it read. Change one line back to profile.Timeout and the
// arithmetic test stays green while the real first-call budget is the wrong one again — which is
// how this whole area went wrong twice already.
//
// The two values are deliberately far apart, so a build that reads the wrong one does not merely
// fail: it fails by an amount nobody can mistake for jitter.
func TestServiceEmbedder_Handshake_IsBoundedByVerifyTimeout(t *testing.T) {
	p := testProfile()
	p.VerifyTimeout = 50 * time.Millisecond
	p.Timeout = 10 * time.Second

	e, err := NewServiceEmbedder(p, &stubTransport{
		info: func(ctx context.Context) (BackendHandshakeInfo, error) {
			<-ctx.Done()
			return BackendHandshakeInfo{}, ctx.Err()
		},
	})
	if err != nil {
		t.Fatalf("NewServiceEmbedder: %v", err)
	}

	start := time.Now()
	if _, err := e.Handshake(context.Background()); err == nil {
		t.Fatal("a backend that never answered produced a successful handshake")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("the handshake ran for %v against a VerifyTimeout of %v: it is bounded by Timeout "+
			"(%v) instead, so every caller's verification budget is the one sized for inference",
			elapsed, p.VerifyTimeout, p.Timeout)
	}
}

// TestServiceEmbedder_Handshake_CallerCancellation_IsNotAnOutage holds the line this package draws
// everywhere else: a caller that stopped is not a backend that failed.
//
// outerCtxErr states the policy and gives the reason — widening ErrBackend to cover cancellation
// "would report every disconnect as an outage". Handshake used to be exempt from it for a reason
// that expired: nothing called Handshake inside a request, so no caller could cancel one. Since
// D-33 the first search of every process does, and a client that disconnects mid-search — or a
// SIGTERM during shutdown, which cancels the root context the same way — would have been logged and
// reported as "the embedding service is down".
//
// The deadline half is asserted alongside it because the fix must not swallow both: VerifyTimeout
// expiring genuinely is "the backend did not answer", and must stay ErrBackend.
func TestServiceEmbedder_Handshake_CallerCancellation_IsNotAnOutage(t *testing.T) {
	t.Run("the caller cancels", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		e := newTestEmbedder(t, &stubTransport{
			info: func(ctx context.Context) (BackendHandshakeInfo, error) {
				cancel() // mid-flight, the way a disconnect or a SIGTERM arrives
				<-ctx.Done()
				return BackendHandshakeInfo{}, ctx.Err()
			},
		})

		_, err := e.Handshake(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want it to carry context.Canceled", err)
		}
		if errors.Is(err, ErrBackend) {
			t.Errorf("a cancelled call was reported as a backend outage: %v — every client that "+
				"disconnects mid-search would be logged as the embedding service going down", err)
		}
	})

	t.Run("VerifyTimeout expires", func(t *testing.T) {
		p := testProfile()
		p.VerifyTimeout = 20 * time.Millisecond
		e, err := NewServiceEmbedder(p, &stubTransport{
			info: func(ctx context.Context) (BackendHandshakeInfo, error) {
				<-ctx.Done()
				return BackendHandshakeInfo{}, ctx.Err()
			},
		})
		if err != nil {
			t.Fatalf("NewServiceEmbedder: %v", err)
		}

		_, err = e.Handshake(context.Background())
		if !errors.Is(err, ErrBackend) {
			t.Errorf("error = %v, want ErrBackend: this embedder's own verification budget running "+
				"out IS the backend failing to answer, and the cancellation check above must not "+
				"have swallowed it", err)
		}
	})
}

// TestServiceEmbedder_WaiterWhoseContextDied_GetsAnErrorNotAZeroVector is the worst failure this
// type can have, and it was reachable.
//
// The sequence: caller A takes the verification lock and handshakes. Caller B arrives, waits, and
// its own deadline expires while it waits. A succeeds and latches. B then acquires the lock, sees
// verified, and walks on into the fan-out with a context that is already dead — where every
// sub-batch goroutine took the runCtx.Done() branch and returned WITHOUT recording an error. The
// result was `out` untouched and firstErr nil: a slice of zero-valued Embeddings handed back as a
// success, so retrieval would search with an all-zero query vector and get plausible nonsense.
//
// The bug predates D-33 — the window between embed's own ctx check and the goroutines starting was
// always nonzero — but it was microseconds wide and unreachable in practice. Putting a lock and a
// network call in front of the fan-out is what made it a window you could drive through. Measured
// before the fix: 10 failures in 20 runs, because the select in the fan-out has both cases ready and
// Go picks between them at random.
//
// What this test does NOT do is tell the two fixes apart, and that is worth stating rather than
// leaving for someone to discover. Two things changed: ensureVerified now abandons the wait when the
// caller's context dies, and the fan-out records a reason instead of returning silently. Either one
// alone makes this test pass — removing one and running 20 times gives zero failures; removing both
// restores 10 in 20. They are kept as two because they answer different questions. The first says a
// caller should not queue for a slot it can no longer use, and it is what makes the wide window
// disappear. The second is the root-cause fix for a silent return leaving firstErr nil, which was
// wrong on every path and not only this one — with the first fix in place it guards a window that is
// microseconds wide again, which is to say it guards the bug this test could no longer reach.
func TestServiceEmbedder_WaiterWhoseContextDied_GetsAnErrorNotAZeroVector(t *testing.T) {
	var once sync.Once
	handshaking := make(chan struct{})
	e := newTestEmbedder(t, &stubTransport{
		info: func(context.Context) (BackendHandshakeInfo, error) {
			once.Do(func() { close(handshaking) })
			time.Sleep(150 * time.Millisecond)
			return Expected(), nil
		},
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = e.EmbedQuery(context.Background(), "the caller that holds the lock")
	}()
	<-handshaking // the lock is held, and will be for ~150 ms

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	got, err := e.EmbedQuery(ctx, "the caller that gives up while waiting")
	if err == nil {
		t.Fatalf("a caller whose deadline expired was given a successful result: dense=%v sparse=%v — "+
			"an all-zero vector returned as a real embedding is the one failure nothing downstream "+
			"can detect", got.Dense, got.Sparse)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want it to carry the caller's own deadline", err)
	}
	<-done
}

// TestServiceEmbedder_ConcurrentFirstEmbeds_HandshakeOnce covers the moment this type is most
// likely to be hit by more than one goroutine at a time: the first queries after a start. Without
// the lock every one of them handshakes; with a lock but no re-check under it, every one that
// queued handshakes in turn. Run under -race, this also proves the latch itself is not a data race.
//
// The handshake is deliberately slowed down, and the delay is the difference between a test that
// catches a missing re-check and one that mostly does not. With an instant stub the first caller is
// in and out before its siblings reach the mutex, so a build with no re-check under the lock still
// records one or two handshakes and passes: measured on this machine, 3 failures in 20 runs. The
// delay holds the lock open long enough for all of them to queue, which is the state the re-check
// exists for — and the state a real backend, which answers over a socket, produces on its own.
func TestServiceEmbedder_ConcurrentFirstEmbeds_HandshakeOnce(t *testing.T) {
	tr := &stubTransport{
		info: func(context.Context) (BackendHandshakeInfo, error) {
			time.Sleep(20 * time.Millisecond)
			return Expected(), nil
		},
	}
	e := newTestEmbedder(t, tr)

	const callers = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := e.EmbedQuery(context.Background(), "alpha"); err != nil {
				t.Errorf("EmbedQuery: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if n := tr.infoCallCount(); n != 1 {
		t.Errorf("handshakes = %d, want 1: %d concurrent first calls stampeded the backend", n, callers)
	}
}
