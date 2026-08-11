//go:build integration

package chunk

import (
	"context"
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/danielmalka/go-knowrag/internal/config"
	"github.com/danielmalka/go-knowrag/internal/vault"
)

// The bounds this calibration run measures. They are the subject of the report, not a claim about
// anything else in the tree — S03 T10 exists to decide whether they survive contact with the corpus.
//
// `cmd/cli/ingest.go` spells the same pair as `defaultFloorTokens`/`defaultCeilingTokens`, and that
// is where an operator's run actually gets its bounds. Go cannot check the two agree: those are
// unexported constants of a `main` package, unreachable from here. So the agreement is a fact about
// today, verifiable only by opening that file — if it changes, this measurement describes bounds
// nobody runs, and the report in docs/eval/chunking-calibration.md is stale. The env overrides below
// exist so alternative bounds can be measured without editing anything.
const (
	calibrationFloorTokens   = 256
	calibrationCeilingTokens = 1024
)

const (
	envFloor   = "KNOWRAG_CALIBRATION_FLOOR_TOKENS"
	envCeiling = "KNOWRAG_CALIBRATION_CEILING_TOKENS"
)

// probeTimeout bounds the one call that decides whether the tokenizer is reachable at all. Without
// it, an embedder that is down turns this test into 700-odd notes each failing after
// HTTPTokenCounter's three attempts (tokenizer.go: tokenizeAttempts, tokenizeDelay) — minutes spent
// proving what one refused connection already said.
const probeTimeout = 30 * time.Second

// TestChunkNote_Calibration_RealCorpus is S03 T10: it chunks every note of every configured vault
// with the real BGE-M3 tokenizer and reports the two distributions the acceptance criterion names —
// chunks per note and tokens per chunk. The numbers it logs are what
// docs/eval/chunking-calibration.md is written from; the file is transcribed from this output rather
// than emitted here, because docs/ is outside git (CLAUDE.md) and a test that writes into an
// ignored directory would leave no trace of having done so.
//
// It measures; it asserts almost nothing, and the two things it does assert are the two that would
// make the report a lie: that notes were actually read, and that chunks were actually produced. A
// calibration run that measured nothing must not print a green line. Everything else worth checking
// about ChunkNote — contiguous indices, the hard-window refusal, a no-H2 note yielding a chunk — is
// already proven in chunk_test.go against a deterministic counter, and a second copy here would be
// a second assertion free to drift from the first.
//
// KNOWN LIMIT, found by planting it: a defect that makes readCorpus below return only part of the
// corpus — five notes per vault, say — leaves this test GREEN, and the summary it prints is an
// accurate summary of the wrong sample. Nothing here can catch a harness that lies about its own
// input: there is no second source of truth for "how many notes exist" that is not equally easy to
// mutate, and inventing one inside the same function would only move the target. The defense is
// reading the per-vault "N note(s)" lines next to the "notes read: N" total — they are produced at
// different points and have to add up. A number taken from this test without checking those two
// lines is a number about a sample nobody counted.
//
// Behind the `integration` tag and skipped when the configuration, the vaults or the tokenizer are
// absent, so `make test` never reaches it and `make test-integration` stays usable on a host that
// has only some of the equipment.
func TestChunkNote_Calibration_RealCorpus(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Skipf("no usable configuration: %v", err)
	}
	if err := cfg.Require(config.NeedEmbedder); err != nil {
		t.Skipf("no tokenizer to measure with: %v", err)
	}
	names := cfg.VaultNames()
	if len(names) == 0 {
		t.Skip("KNOWRAG_VAULTS names no vault")
	}

	clampCfg := Config{
		FloorTokens:   boundFromEnv(t, envFloor, calibrationFloorTokens),
		CeilingTokens: boundFromEnv(t, envCeiling, calibrationCeilingTokens),
	}
	if err := clampCfg.Validate(); err != nil {
		t.Fatal(err)
	}

	real, err := NewHTTPTokenCounter(cfg.EmbedderEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	probeCtx, cancel := context.WithTimeout(t.Context(), probeTimeout)
	defer cancel()
	if _, err := real.CountTokens(probeCtx, "calibration probe"); err != nil {
		t.Skipf("tokenizer is not answering: %v", err)
	}
	counter := NewCountingTokenCounter(real)

	notes := readCorpus(t, cfg, names)
	if len(notes) == 0 {
		t.Fatal("no note was read: there is nothing to calibrate against")
	}

	var (
		chunksPerNote  []int
		tokensPerChunk []int
		oversize       int
		singleChunk    int
		emptyNotes     int
		belowFloor     int
		// The two parts of belowFloor the floor never had a chance at, split out because they answer
		// a different question than the rest: `last` is the final chunk of a note and `alone` is the
		// only chunk of one, and neither has a following sibling to merge with. What remains —
		// belowFloor minus these two — is the merge pass stopping while it still had material, which
		// is the number that says something about the floor rather than about the corpus.
		belowFloorAlone int
		belowFloorLast  int
		// Where the largest chunk is. The distribution's max is the number the ceiling has to live
		// with, and a report that states it without saying which note it came from leaves the one
		// chunk an operator might want to split unfindable.
		maxTokens int
		maxWhere  string
	)

	start := time.Now()
	for _, n := range notes {
		chunks, err := ChunkNote(t.Context(), n, clampCfg, counter)
		if err != nil {
			t.Fatalf("ChunkNote(%s/%s): %v", n.Vault, n.Path, err)
		}
		if len(chunks) == 0 {
			// ChunkNote returns nothing for a note whose body is blank (chunk.go). Counted, not
			// failed: it is a property of the corpus, and the report has to say how many.
			emptyNotes++
			continue
		}
		chunksPerNote = append(chunksPerNote, len(chunks))
		if len(chunks) == 1 {
			singleChunk++
		}
		for _, c := range chunks {
			// The final text, re-measured. This is the number that matters for the report: what the
			// model will read for this point, breadcrumb included. The clamp's internal counts are
			// not observable from here, and inferring them would be describing the algorithm instead
			// of measuring its output.
			tokens, err := counter.CountTokens(t.Context(), c.Text)
			if err != nil {
				t.Fatalf("counting chunk %d of %s/%s: %v", c.Index, n.Vault, n.Path, err)
			}
			tokensPerChunk = append(tokensPerChunk, tokens)
			if tokens > maxTokens {
				maxTokens = tokens
				maxWhere = fmt.Sprintf("%s/%s chunk %d %q",
					n.Vault, n.Path, c.Index, strings.Join(c.Breadcrumb, " > "))
			}
			if c.Oversize {
				oversize++
			}
			if tokens < clampCfg.FloorTokens {
				belowFloor++
				switch {
				case len(chunks) == 1:
					belowFloorAlone++
				case c.Index == len(chunks)-1:
					belowFloorLast++
				}
			}
		}
	}
	elapsed := time.Since(start)

	if len(tokensPerChunk) == 0 {
		t.Fatalf("%d note(s) read and not one chunk produced", len(notes))
	}

	t.Logf("floor=%d ceiling=%d (chunker %s, hard window %d)",
		clampCfg.FloorTokens, clampCfg.CeilingTokens, Version, HardTokenLimit)
	t.Logf("notes read: %d, of which produced no chunk (blank body): %d", len(notes), emptyNotes)
	t.Logf("chunks produced: %d", len(tokensPerChunk))
	t.Logf("chunks per note: %s", describe(chunksPerNote))
	t.Logf("tokens per chunk: %s", describe(tokensPerChunk))
	t.Logf("notes that produced exactly one chunk: %d of %d", singleChunk, len(chunksPerNote))
	t.Logf("chunks above the ceiling (Chunk.Oversize): %d", oversize)
	t.Logf("chunks still below the floor after the merge pass: %d (alone in their note: %d, "+
		"last of their note: %d, merge stopped mid-note: %d)",
		belowFloor, belowFloorAlone, belowFloorLast, belowFloor-belowFloorAlone-belowFloorLast)
	t.Logf("largest chunk: %d tokens at %s", maxTokens, maxWhere)
	t.Logf("wall clock for the chunking pass: %s", elapsed.Round(time.Second))
	t.Logf("%s", counter.Snapshot())
}

// readCorpus scans every configured vault, skipping the ones this host does not have.
//
// The roster comes from config.Load, never from a table here — the same reason
// vault/scan_integration_test.go gives: a hard-coded vault name that no longer matches the
// operator's roster does not fail, it skips, and a skipped test reads like a passing one.
//
// The per-vault line it logs is not decoration. It is the only thing in the run output that
// contradicts a truncated return from this function, which nothing here can assert against — see
// the KNOWN LIMIT paragraph on the test above.
func readCorpus(t *testing.T, cfg *config.Config, names []string) []vault.Note {
	t.Helper()
	var notes []vault.Note
	for _, name := range names {
		settings := cfg.Vaults[name]
		root := settings.Path
		if root == "" {
			t.Logf("vault %s has no configured path; skipped", name)
			continue
		}
		// #nosec G703 -- root is the operator-configured vault path this test exists to read
		if _, err := os.Stat(root); err != nil {
			t.Logf("vault %s is not readable, skipped: %v", name, err)
			continue
		}
		result, err := vault.ScanVault(root, name, settings.AreaNames(), vault.Exclusions{
			Folders:   settings.Folders(),
			RootFiles: settings.RootFiles(),
		})
		if err != nil {
			t.Fatalf("scanning vault %s: %v", name, err)
		}
		t.Logf("vault %s: %d note(s), %d skipped by the scanner",
			name, len(result.Notes), len(result.Skipped))
		notes = append(notes, result.Notes...)
	}
	return notes
}

// boundFromEnv reads an override, falling back to the constant above. A value that is not a positive
// integer is fatal rather than ignored: silently calibrating at the default while the operator
// believes they asked for something else is how a report ends up describing bounds nobody chose.
func boundFromEnv(t *testing.T, name string, def int) int {
	t.Helper()
	raw, ok := os.LookupEnv(name)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		t.Fatalf("%s = %q is not a positive integer", name, raw)
	}
	return n
}

// distribution is the five numbers the acceptance criterion asks each distribution to report.
type distribution struct {
	N      int
	Min    int
	Median int
	P95    int
	Max    int
}

func (d distribution) String() string {
	return "n=" + strconv.Itoa(d.N) +
		" min=" + strconv.Itoa(d.Min) +
		" median=" + strconv.Itoa(d.Median) +
		" p95=" + strconv.Itoa(d.P95) +
		" max=" + strconv.Itoa(d.Max)
}

// describe sorts a copy — a helper that reordered its caller's slice would silently change what any
// later pass over the same data means.
func describe(values []int) distribution {
	if len(values) == 0 {
		return distribution{}
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	return distribution{
		N:      len(sorted),
		Min:    sorted[0],
		Median: quantile(sorted, 0.50),
		P95:    quantile(sorted, 0.95),
		Max:    sorted[len(sorted)-1],
	}
}

// quantile is the nearest-rank percentile of a non-empty ascending slice: the smallest value at or
// below which at least p of the sample falls. No interpolation — an interpolated median of token
// counts reports a chunk length that no chunk has.
//
// It carries no range guards because for 0 < p ≤ 1 and len ≥ 1 the rank is always in range, and
// describe is the only caller: it never passes an empty slice and never passes p outside that range.
// Guards for those cases were written first and then removed — nothing could make them fire, so
// nothing could prove them right either.
func quantile(sorted []int, p float64) int {
	return sorted[int(math.Ceil(p*float64(len(sorted))))-1]
}

// TestDescribe_NearestRankOverKnownSample pins the only logic this file adds. The sample is 1..10 so
// every expected number is countable by eye: nearest rank puts the median at the 5th value and p95
// at the 10th, and both differ from what an interpolating or off-by-one implementation would say.
func TestDescribe_NearestRankOverKnownSample(t *testing.T) {
	// Deliberately unsorted, and with the caller's slice checked afterwards: describe must not
	// depend on receiving sorted input, nor reorder what it was given.
	input := []int{10, 1, 9, 2, 8, 3, 7, 4, 6, 5}
	before := slices.Clone(input)

	got := describe(input)
	want := distribution{N: 10, Min: 1, Median: 5, P95: 10, Max: 10}
	if got != want {
		t.Errorf("describe(1..10) = %v, want %v", got, want)
	}
	if !slices.Equal(input, before) {
		t.Errorf("describe reordered its argument: %v, was %v", input, before)
	}

	// A one-element sample: every statistic is that element. This is the case where an off-by-one
	// rank indexes out of range instead of returning a wrong number.
	if got, want := describe([]int{7}), (distribution{N: 1, Min: 7, Median: 7, P95: 7, Max: 7}); got != want {
		t.Errorf("describe([7]) = %v, want %v", got, want)
	}
	// An empty sample must not panic: it is what an all-blank corpus would hand over, and the
	// caller reports it as the zero line rather than crashing the run that produced it.
	if got, want := describe(nil), (distribution{}); got != want {
		t.Errorf("describe(nil) = %v, want %v", got, want)
	}
}
