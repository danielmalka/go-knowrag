package chunk

import (
	"context"
	"strings"
)

// TokenCounter counts tokens the way the embedding model sees them.
//
// "Token" here means the real BGE-M3 tokenizer (PRD-stories-fundacao §3, S03): counting words or
// characters/4 makes the clamp diverge from what the model reads and invalidates the calibration.
// This interface is the seam that keeps that promise honest — the chunking logic never approximates,
// it asks.
//
// The real, HTTP-backed implementation is deliberately absent from this package today. Whether the
// embedding runtime exposes a tokenization endpoint at all, and with which request/response shape,
// is ADR-001's decision (S00) and it has not landed; writing a client against a guessed shape would
// mean rewriting it and its call sites once ADR-001 contradicts the guess. What ships now is the
// interface and everything measured through it.
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
