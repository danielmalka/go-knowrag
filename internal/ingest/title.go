package ingest

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/danielmalka/go-knowrag/internal/chunk"
)

// fitTitle returns the note title as it is sent to the embedding service: unchanged when it counts at
// most maxTokens, otherwise its longest rune prefix that does.
//
// A note title has no length limit, and the service puts it in front of every chunk of the note
// ("title: <title> | text: <chunk>"). The chunk window leaves a fixed reserve for that prefix
// (schema.ModelProfile.PromptReserveTokens); without this cut, a long title would push a chunk that
// legitimately fills its window past the model's, and the service would refuse it or the model would
// truncate it. It is the title that gives way, never the chunk text, and only the copy sent to the
// embedder: the title in the payload and in point_hash stays whole.
//
// Tokens are counted with the same TokenCounter that sizes the chunks (the verified service's own
// tokenizer), so the bound is in the model's tokens and not an approximation. Counting is monotonic in
// the prefix, which is what lets a binary search find the cut in a handful of calls.
func fitTitle(ctx context.Context, counter chunk.TokenCounter, title string, maxTokens int) (string, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return "", nil
	}
	n, err := counter.CountTokens(ctx, title)
	if err != nil {
		return "", fmt.Errorf("counting the tokens of the note title: %w", err)
	}
	if n <= maxTokens {
		return title, nil
	}
	return longestTitlePrefix(ctx, counter, title, maxTokens)
}

func longestTitlePrefix(ctx context.Context, counter chunk.TokenCounter, title string, maxTokens int) (string, error) {
	runes := []rune(title)
	lo, hi := 0, len(runes) // invariant: runes[:lo] fits, runes[:hi] is known not to (hi starts past it)
	for hi-lo > 1 {
		mid := (lo + hi) / 2
		n, err := counter.CountTokens(ctx, string(runes[:mid]))
		if err != nil {
			return "", fmt.Errorf("counting the tokens of a cut of the note title: %w", err)
		}
		if n <= maxTokens {
			lo = mid
		} else {
			hi = mid
		}
	}
	cut := strings.TrimSpace(string(runes[:lo]))
	if !utf8.ValidString(cut) {
		return "", fmt.Errorf("title cut produced invalid UTF-8")
	}
	return cut, nil
}
