package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"time"
)

// defaultGracePeriod is how long a note already in flight gets to finish after the first SIGINT.
//
// It covers what one note may still be doing: its embedding, its upsert and its prune. How many
// requests that embedding takes is not fixed here — a note is split into batches of embedProfile's
// BatchSize (ingest.go), so a long note is several round trips rather than one, and this budget is
// not a per-request timeout. It is a flag because the right number is a fact about somebody else's
// hardware, and a default because an operator hitting Ctrl-C should not have had to choose one.
const defaultGracePeriod = 30 * time.Second

// exitInterrupted is the process exit code for a run the operator stopped. 130 is the shell's
// convention for 128+SIGINT, so a scheduler already knows it means "somebody pressed Ctrl-C" rather
// than "the ingestion is broken". It is declared here and mapped in main.go beside exitLockHeld.
const exitInterrupted = 130

// errInterrupted is returned after the partial report is printed, so main can pick the exit code
// without the report having to decide it.
var errInterrupted = errors.New("the ingestion was interrupted before it finished")

// withInterrupt splits one Ctrl-C into the two things an ingestion needs to hear.
//
// The first signal closes `stop`, which makes RunBatch finish the note it is on and schedule no
// more (internal/ingest/batch.go). The note in flight keeps the context, so an upsert that was
// already confirmed still gets its prune — the window ADR-006 §3 exists to protect.
//
// The returned context is cancelled by whichever comes first: the grace period expiring, or a second
// signal. The second signal is the operator saying the grace period was a bad guess, and the only
// honest response is to stop arguing and drop the work.
func withInterrupt(ctx context.Context, grace time.Duration) (context.Context, <-chan struct{}, func()) {
	ctx, cancel := context.WithCancel(ctx)
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt)
	stop := make(chan struct{})

	go func() {
		select {
		case <-signals:
		case <-ctx.Done():
			return
		}
		close(stop)
		slog.Warn("interrupted: finishing the note in flight and starting no more",
			"grace_period", grace)

		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-signals:
			slog.Warn("interrupted twice: dropping the note in flight")
		case <-timer.C:
			slog.Warn("the grace period expired with a note still running", "grace_period", grace)
		case <-ctx.Done():
			return
		}
		cancel()
	}()

	return ctx, stop, func() { signal.Stop(signals); cancel() }
}
