package retrieval

import (
	"errors"
	"testing"
)

// validQuery is the baseline every case below mutates one field of, so a failure names the one
// thing that was different rather than the whole struct.
func validQuery() Query {
	return Query{Collection: "interno_eg2", TenantID: "tenant-a", Text: "how do I configure cron", TopK: 5}
}

func TestQuery_Validate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Query)
		want   error
	}{
		{"valid", func(*Query) {}, nil},
		{"valid with offset", func(q *Query) { q.Offset = 10 }, nil},
		{"valid with every optional filter", func(q *Query) {
			q.Area, q.Type, q.Vault, q.Tags = "infra", "concept", "trabalho", []string{"golang"}
			q.IncludeArchived, q.IncludePrivate = true, true
		}, nil},

		{"no collection", func(q *Query) { q.Collection = "" }, ErrEmptyCollection},
		{"no tenant", func(q *Query) { q.TenantID = "" }, ErrEmptyTenant},
		{"no text", func(q *Query) { q.Text = "" }, ErrEmptyQuery},
		{"blank text", func(q *Query) { q.Text = "   \n\t " }, ErrEmptyQuery},
		{"zero top_k", func(q *Query) { q.TopK = 0 }, ErrInvalidTopK},
		{"negative top_k", func(q *Query) { q.TopK = -1 }, ErrInvalidTopK},
		{"absurd top_k", func(q *Query) { q.TopK = maxTopK + 1 }, ErrInvalidTopK},
		{"negative offset", func(q *Query) { q.Offset = -1 }, ErrInvalidOffset},
		{"absurd offset", func(q *Query) { q.Offset = maxOffset + 1 }, ErrInvalidOffset},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := validQuery()
			tc.mutate(&q)

			err := q.Validate()
			if tc.want == nil {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestQuery_Validate_TenantIsCheckedBeforeAnythingOptional pins the ordering rather than only the
// outcome: a Query that is wrong in several ways at once must still report the missing tenant,
// because that is the error a caller has to be able to match on to tell "you have no scope" apart
// from "your paging is off".
func TestQuery_Validate_TenantIsCheckedBeforeAnythingOptional(t *testing.T) {
	q := Query{Collection: "interno_eg2", TopK: -5, Offset: -5}
	if err := q.Validate(); !errors.Is(err, ErrEmptyTenant) {
		t.Fatalf("Validate = %v, want %v", err, ErrEmptyTenant)
	}
}
