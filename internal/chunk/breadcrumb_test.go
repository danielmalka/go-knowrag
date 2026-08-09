package chunk

import (
	"context"
	"strings"
	"testing"
)

func TestPrefixBreadcrumb_FormatsAsArrowSeparatedPath(t *testing.T) {
	got := PrefixBreadcrumb([]string{"Title", "Setup", "Prerequisites"}, "body text\n")

	const want = "Title > Setup > Prerequisites\n\nbody text\n"
	if got != want {
		t.Errorf("PrefixBreadcrumb = %q, want %q", got, want)
	}
}

// TestPrefixBreadcrumb_EmptyBreadcrumbLeavesBodyUntouched matters because a note with no headings
// at all still produces a chunk. A blank line or a stray " > " glued to the front would be text the
// model embeds and the hash covers, invented by this function out of nothing.
func TestPrefixBreadcrumb_EmptyBreadcrumbLeavesBodyUntouched(t *testing.T) {
	for _, bc := range [][]string{nil, {}} {
		if got := PrefixBreadcrumb(bc, "body text\n"); got != "body text\n" {
			t.Errorf("PrefixBreadcrumb(%v, ...) = %q, want the body unchanged", bc, got)
		}
	}
}

func TestPrefixBreadcrumb_SingleLevel(t *testing.T) {
	if got, want := PrefixBreadcrumb([]string{"Title"}, "b"), "Title\n\nb"; got != want {
		t.Errorf("PrefixBreadcrumb = %q, want %q", got, want)
	}
}

func TestSectionTokenCount_IncludesBreadcrumb(t *testing.T) {
	ctx := context.Background()
	tc := FakeTokenCounter{}
	breadcrumb := []string{"Title", "Setup", "Prerequisites"}
	body := "some body text here\n"

	bare, err := tc.CountTokens(ctx, body)
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	crumb, err := tc.CountTokens(ctx, strings.Join(breadcrumb, " > "))
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}

	got, err := measureTokens(ctx, tc, breadcrumb, body)
	if err != nil {
		t.Fatalf("measureTokens: %v", err)
	}

	if got <= bare {
		t.Errorf("measureTokens = %d, want strictly more than the bare body's %d", got, bare)
	}
	if got < bare+crumb {
		t.Errorf("measureTokens = %d, want at least %d (body %d + breadcrumb %d)",
			got, bare+crumb, bare, crumb)
	}
}

// TestMeasureTokens_PropagatesCounterError proves the budget is never guessed when the tokenizer is
// unreachable. Swallowing the error would silently fall back to counting nothing, and every clamp
// decision downstream would be made against zero.
func TestMeasureTokens_PropagatesCounterError(t *testing.T) {
	_, err := measureTokens(context.Background(), failingCounter{}, []string{"T"}, "body")
	if err == nil {
		t.Fatal("measureTokens = nil error, want the counter's failure propagated")
	}
	if !strings.Contains(err.Error(), "tokenizer is down") {
		t.Errorf("error = %v, want it to carry the counter's message", err)
	}
}
