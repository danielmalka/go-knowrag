package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/clicmd"
	"github.com/danielmalka/go-knowrag/internal/store"
)

// runStats drives the real command over a reader that records the scope it was asked for.
func runStats(t *testing.T, counts map[string]store.Stats, readErr error, args ...string) (
	stdout string, tenants []string, err error,
) {
	t.Helper()

	var seen []string
	cmd := newStatsCmd(func(_ context.Context, tenantID string) (map[string]store.Stats, error) {
		seen = append(seen, tenantID)
		return counts, readErr
	})

	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), seen, err
}

func sampleCounts() map[string]store.Stats {
	return map[string]store.Stats{
		"interno":   {Points: 3200, UIDs: 780},
		"clientes":  {Points: 0, UIDs: 0},
		"base_paga": {Points: 12, UIDs: 12},
	}
}

// TestStatsCmd_PrintsBothCountsPerCollection is the command's whole contract. Both numbers per
// collection, because either one alone answers a different question than the operator asked: points
// without notes cannot show a note that shrank and left a tail behind.
func TestStatsCmd_PrintsBothCountsPerCollection(t *testing.T) {
	out, _, err := runStats(t, sampleCounts(), nil)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}

	for _, want := range []string{"interno", "3200", "780", "clientes", "base_paga", "12"} {
		if !strings.Contains(out, want) {
			t.Errorf("the output does not carry %q:\n%s", want, out)
		}
	}
	// A collection that holds nothing is still reported, and reported as zero. Skipping it would
	// make "provisioned and empty" — which is what `clientes` and `base_paga` are in this build —
	// indistinguishable from "not provisioned at all".
	if lines := strings.Count(strings.TrimSpace(out), "\n") + 1; lines != len(sampleCounts()) {
		t.Errorf("the output has %d line(s) for %d collections:\n%s", lines, len(sampleCounts()), out)
	}
}

// TestStatsCmd_OrderIsStable covers the thing an operator actually does with this command: run it
// before an ingestion and after, and diff. Map iteration order is random, so an unsorted printer
// produces a diff full of moved lines and no information.
func TestStatsCmd_OrderIsStable(t *testing.T) {
	first, _, err := runStats(t, sampleCounts(), nil)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	for range 20 {
		next, _, err := runStats(t, sampleCounts(), nil)
		if err != nil {
			t.Fatalf("stats: %v", err)
		}
		if next != first {
			t.Fatalf("two runs over the same counts printed different output:\n%s\nvs\n%s", first, next)
		}
	}
	// Sorted, specifically — two runs agreeing would also be satisfied by any fixed order, and the
	// order a reader can predict is the alphabetical one.
	want := "base_paga"
	if !strings.HasPrefix(strings.TrimSpace(first), want) {
		t.Errorf("the first line is not %q, so the order is not alphabetical:\n%s", want, first)
	}
}

// TestStatsCmd_TenantFlagIsPassedThrough asserts on what the reader was asked for rather than on
// what was printed: the counts come from the fake either way, so the output cannot tell a scoped
// read from an unscoped one.
func TestStatsCmd_TenantFlagIsPassedThrough(t *testing.T) {
	tests := map[string]struct {
		args []string
		want string
	}{
		"scoped":   {args: []string{"--tenant", "malka"}, want: "malka"},
		"unscoped": {args: nil, want: ""},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, tenants, err := runStats(t, sampleCounts(), nil, tc.args...)
			if err != nil {
				t.Fatalf("stats %v: %v", tc.args, err)
			}
			if len(tenants) != 1 || tenants[0] != tc.want {
				t.Errorf("the reader was asked for %v, want exactly [%q]", tenants, tc.want)
			}
		})
	}
}

// TestStatsCmd_JSON_CarriesTheScopeAndTheCounts pins the wire shape. The tenant is in the document
// because the numbers mean different things with and without it, and a consumer that received only
// counts would have to remember which command line produced them.
func TestStatsCmd_JSON_CarriesTheScopeAndTheCounts(t *testing.T) {
	out, _, err := runStats(t, sampleCounts(), nil, "--tenant", "malka", "--json")
	if err != nil {
		t.Fatalf("stats --json: %v", err)
	}

	var envelope struct {
		OK   bool      `json:"ok"`
		Data statsJSON `json:"data"`
	}
	if uerr := json.Unmarshal([]byte(out), &envelope); uerr != nil {
		t.Fatalf("stdout does not parse as JSON on its own: %v\n%s", uerr, out)
	}
	if !envelope.OK {
		t.Errorf("a successful read reported ok=false: %s", out)
	}
	if envelope.Data.Tenant != "malka" {
		t.Errorf("the envelope reports tenant %q, want malka: %s", envelope.Data.Tenant, out)
	}
	got := envelope.Data.Collections["interno"]
	if got.Points != 3200 || got.UIDs != 780 {
		t.Errorf("interno round-tripped as %+v, want points 3200 and uids 780: %s", got, out)
	}
}

// TestStatsCmd_ReadFailure_IsNotZeroCounts is the rule the whole project keeps restating: not having
// finished looking must never render as having found nothing. A collection reported as empty because
// the connection died reads exactly like a collection nobody ingested.
func TestStatsCmd_ReadFailure_IsNotZeroCounts(t *testing.T) {
	out, _, err := runStats(t, nil, errors.New("qdrant is unreachable"), "--json")
	if err == nil {
		t.Fatal("a failed read exited clean")
	}
	if got := clicmd.CategoryOf(err); got != clicmd.CategoryBackend {
		t.Errorf("CategoryOf(%v) = %q, want %q", err, got, clicmd.CategoryBackend)
	}

	var envelope clicmd.Result
	if uerr := json.Unmarshal([]byte(out), &envelope); uerr != nil {
		t.Fatalf("a failed --json run wrote no parseable envelope: %v\n%s", uerr, out)
	}
	if envelope.OK {
		t.Errorf("the envelope reports ok=true for a read that failed: %s", out)
	}
	if strings.Contains(out, `"collections"`) {
		t.Errorf("the envelope carries counts for a read that never finished: %s", out)
	}
}
