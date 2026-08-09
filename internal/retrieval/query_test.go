package retrieval

import (
	"errors"
	"testing"
)

// validQuery is the baseline every case below mutates one field of, so a failure names the one
// thing that was different rather than the whole struct.
func validQuery() Query {
	return Query{Collection: "interno", TenantID: "malka", Text: "how do I configure cron", TopK: 5}
}

func TestQuery_Validate(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(*Query)
		multiplier int
		want       error
	}{
		{"valid", func(*Query) {}, DefaultPrefetchMultiplier, nil},
		{"valid with offset", func(q *Query) { q.Offset = 10 }, DefaultPrefetchMultiplier, nil},
		{"valid with every optional filter", func(q *Query) {
			q.Area, q.Type, q.Vault, q.Tags = "infra", "concept", "malkaway", []string{"golang"}
			q.IncludeArchived, q.IncludePrivate = true, true
		}, DefaultPrefetchMultiplier, nil},

		{"no collection", func(q *Query) { q.Collection = "" }, DefaultPrefetchMultiplier, ErrEmptyCollection},
		{"no tenant", func(q *Query) { q.TenantID = "" }, DefaultPrefetchMultiplier, ErrEmptyTenant},
		{"no text", func(q *Query) { q.Text = "" }, DefaultPrefetchMultiplier, ErrEmptyQuery},
		{"blank text", func(q *Query) { q.Text = "   \n\t " }, DefaultPrefetchMultiplier, ErrEmptyQuery},
		{"zero top_k", func(q *Query) { q.TopK = 0 }, DefaultPrefetchMultiplier, ErrInvalidTopK},
		{"negative top_k", func(q *Query) { q.TopK = -1 }, DefaultPrefetchMultiplier, ErrInvalidTopK},
		{"absurd top_k", func(q *Query) { q.TopK = maxTopK + 1 }, DefaultPrefetchMultiplier, ErrInvalidTopK},
		{"negative offset", func(q *Query) { q.Offset = -1 }, DefaultPrefetchMultiplier, ErrInvalidOffset},
		{"absurd offset", func(q *Query) { q.Offset = maxOffset + 1 }, DefaultPrefetchMultiplier, ErrInvalidOffset},

		// The floor: the per-leg limit is (top_k + offset) * multiplier, so a multiplier of exactly
		// 1 lands on the floor and anything at or below zero falls under it.
		{"multiplier lands exactly on the floor", func(q *Query) { q.Offset = 3 }, 1, nil},
		{"zero multiplier", func(q *Query) { q.Offset = 3 }, 0, ErrPrefetchLimitTooLow},
		{"negative multiplier", func(q *Query) { q.Offset = 3 }, -2, ErrPrefetchLimitTooLow},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := validQuery()
			tc.mutate(&q)

			err := q.Validate(tc.multiplier)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("Validate(%d) = %v, want nil", tc.multiplier, err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("Validate(%d) = %v, want %v", tc.multiplier, err, tc.want)
			}
		})
	}
}

// TestQuery_Validate_TenantIsCheckedBeforeAnythingOptional pins the ordering rather than only the
// outcome: a Query that is wrong in several ways at once must still report the missing tenant,
// because that is the error a caller has to be able to match on to tell "you have no scope" apart
// from "your paging is off".
func TestQuery_Validate_TenantIsCheckedBeforeAnythingOptional(t *testing.T) {
	q := Query{Collection: "interno", TopK: -5, Offset: -5}
	if err := q.Validate(0); !errors.Is(err, ErrEmptyTenant) {
		t.Fatalf("Validate = %v, want %v", err, ErrEmptyTenant)
	}
}

func TestPrefetchLimit(t *testing.T) {
	cases := []struct {
		name                     string
		topK, offset, multiplier int
		want                     int
		wantErr                  bool
	}{
		{"floor times multiplier", 5, 3, 4, 32, false},
		{"multiplier of one is the floor", 5, 3, 1, 8, false},
		{"no offset", 10, 0, 3, 30, false},
		{"zero multiplier", 5, 3, 0, 0, true},
		{"overflowing multiplier", 5, 3, 1 << 62, 0, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := prefetchLimit(tc.topK, tc.offset, tc.multiplier)
			if tc.wantErr {
				if !errors.Is(err, ErrPrefetchLimitTooLow) {
					t.Fatalf("prefetchLimit = (%d, %v), want %v", got, err, ErrPrefetchLimitTooLow)
				}
				return
			}
			if err != nil {
				t.Fatalf("prefetchLimit = (%d, %v), want no error", got, err)
			}
			if got != tc.want {
				t.Errorf("prefetchLimit = %d, want %d", got, tc.want)
			}
			if floor := tc.topK + tc.offset; got < floor {
				t.Errorf("prefetchLimit = %d, below the top_k+offset floor of %d", got, floor)
			}
		})
	}
}
