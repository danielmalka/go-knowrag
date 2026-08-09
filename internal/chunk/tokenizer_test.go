package chunk

import (
	"context"
	"errors"
	"testing"
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

// failingCounter is the tokenizer being unreachable. Every pass that counts tokens must surface
// this rather than proceeding with a guess.
type failingCounter struct{}

func (failingCounter) CountTokens(context.Context, string) (int, error) {
	return 0, errors.New("tokenizer is down")
}
