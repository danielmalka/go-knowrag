package eval

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danielmalka/go-knowrag/internal/goldenset"
)

// gitRepo is a throwaway repository with a golden-set file in it. Every commit gets an explicit,
// increasing author date so the "after the baseline" comparison is about the commits and not about
// how fast the test machine runs.
type gitRepo struct {
	t     *testing.T
	dir   string
	path  string
	clock time.Time
}

func newGitRepo(t *testing.T) *gitRepo {
	t.Helper()
	dir := t.TempDir()
	r := &gitRepo{t: t, dir: dir, path: filepath.Join(dir, "golden-set.yaml"),
		clock: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)}

	r.run("init")
	r.run("config", "user.email", "fixture@example.invalid")
	r.run("config", "user.name", "fixture")
	return r
}

func (r *gitRepo) run(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...) // #nosec G204 -- literals from this file, in a t.TempDir()
	cmd.Dir = r.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// commit writes body and commits it, advancing the fixture clock by an hour so each commit is
// strictly later than the last.
func (r *gitRepo) commit(body, message string) string {
	r.t.Helper()
	if err := os.WriteFile(r.path, []byte(body), 0o600); err != nil {
		r.t.Fatalf("writing the fixture: %v", err)
	}
	r.clock = r.clock.Add(time.Hour)
	stamp := r.clock.Format(time.RFC3339)

	cmd := exec.Command("git", "commit", "-m", message) // #nosec G204 -- a test-local literal
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+stamp, "GIT_COMMITTER_DATE="+stamp)
	r.run("add", "golden-set.yaml")
	if out, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("git commit: %v\n%s", err, out)
	}
	return strings.TrimSpace(r.run("rev-parse", "HEAD"))
}

func entry(question, uid, area string) string {
	return "  - question: \"" + question + "\"\n" +
		"    uid: \"" + uid + "\"\n" +
		"    area: " + area + "\n" +
		"    author: owner\n" +
		"    date: \"2026-08-01\"\n"
}

const fixtureCoverage = "coverage:\n  min_total: 1\n  max_total: 10\n  groups:\n" +
	"    - name: high\n      areas: [alfa, beta]\n      min: 1\n      max: 5\n"

// TestFlagStaleEntries_EntryCommitAfterBaseline_IsFlagged is S10 T6's first RED test.
func TestFlagStaleEntries_EntryCommitAfterBaseline_IsFlagged(t *testing.T) {
	repo := newGitRepo(t)
	first := goldenset.GoldenQuestion{Question: "the first question about restarts", UID: uidA, Area: "alfa"}
	second := goldenset.GoldenQuestion{Question: "the second question about retention", UID: uidB, Area: "beta"}

	body := fixtureCoverage + "questions:\n" + entry(first.Question, first.UID, "alfa")
	repo.commit(body, "add the first question")
	baseline := repo.clock

	body += entry(second.Question, second.UID, "beta")
	lateCommit := repo.commit(body, "add the second question")

	questions := []goldenset.GoldenQuestion{first, second}
	_, perEntry, err := GoldenSetCommit(t.Context(), repo.path, questions)
	if err != nil {
		t.Fatalf("GoldenSetCommit: %v", err)
	}

	flagged := FlagStaleEntries(perEntry, baseline)
	if len(flagged) != 1 {
		t.Fatalf("%d entry/entries flagged, want exactly the one committed after the baseline: %v",
			len(flagged), ResolveStale(flagged, perEntry, questions))
	}
	if flagged[0] != goldenset.EntryIdentity(second) {
		t.Errorf("the wrong entry was flagged: %v", ResolveStale(flagged, perEntry, questions))
	}

	stale := ResolveStale(flagged, perEntry, questions)
	if stale[0].Commit != lateCommit {
		t.Errorf("the flagged entry's commit is %q, want %q", stale[0].Commit, lateCommit)
	}
	if !strings.Contains(stale[0].Question, "retention") {
		t.Errorf("the flagged entry was not resolved back to readable text: %+v", stale[0])
	}
}

// TestGoldenSetCommit_SurvivesReformatting_IdentityUnaffectedByLineShift is S10 T6's second RED
// test, and the regression guard for the defect the identity scheme replaces: a git blame keyed on
// line number reattributes every entry below an insertion, so reordering the file rewrites the
// provenance of questions nobody touched.
func TestGoldenSetCommit_SurvivesReformatting_IdentityUnaffectedByLineShift(t *testing.T) {
	repo := newGitRepo(t)
	target := goldenset.GoldenQuestion{Question: "the question whose line number moves", UID: uidA, Area: "alfa"}
	other := goldenset.GoldenQuestion{Question: "an unrelated question added later", UID: uidB, Area: "beta"}

	body := fixtureCoverage + "questions:\n" + entry(target.Question, target.UID, "alfa")
	introduced := repo.commit(body, "add the target question")

	// The reformat: a second entry inserted *above* the target, plus blank lines. Every line of the
	// target's block now has a different number, and none of its question/uid bytes changed.
	reordered := fixtureCoverage + "\nquestions:\n" +
		entry(other.Question, other.UID, "beta") + "\n" + entry(target.Question, target.UID, "alfa")
	reformat := repo.commit(reordered, "reorder and rewrap, touching no question text")

	if introduced == reformat {
		t.Fatal("the fixture made one commit, not two")
	}

	_, perEntry, err := GoldenSetCommit(t.Context(), repo.path, []goldenset.GoldenQuestion{target, other})
	if err != nil {
		t.Fatalf("GoldenSetCommit: %v", err)
	}

	got := perEntry[goldenset.EntryIdentity(target)]
	if !got.Found {
		t.Fatal("the target entry lost its provenance across the reformat")
	}
	if got.Hash != introduced {
		t.Errorf("the target is attributed to %s, want the commit that introduced it (%s) — the "+
			"reformat commit is %s, and attributing it there is the line-number defect this test guards",
			got.Hash, introduced, reformat)
	}
	if other := perEntry[goldenset.EntryIdentity(other)]; other.Hash != reformat {
		t.Errorf("the entry actually added by the reformat is attributed to %s, want %s",
			other.Hash, reformat)
	}
}

// TestGoldenSetCommit_ReAddedEntryKeepsItsOriginalIntroduction is what proves --reverse is doing
// something, and it exists because removing --reverse turned no test red: in every other fixture
// here, `git log -S` matches exactly one commit per entry, so the order it returns them in cannot
// change the answer. Removing an entry and adding it back is the one history where it can — three
// commits change the occurrence count, and the introducing commit is the oldest, not the newest.
func TestGoldenSetCommit_ReAddedEntryKeepsItsOriginalIntroduction(t *testing.T) {
	repo := newGitRepo(t)
	kept := goldenset.GoldenQuestion{Question: "a question that stays put", UID: uidC, Area: "alfa"}
	target := goldenset.GoldenQuestion{Question: "a question removed and added back", UID: uidA, Area: "alfa"}

	base := fixtureCoverage + "questions:\n" + entry(kept.Question, kept.UID, "alfa")
	introduced := repo.commit(base+entry(target.Question, target.UID, "alfa"), "add both questions")
	removed := repo.commit(base, "drop the target question")
	readded := repo.commit(base+entry(target.Question, target.UID, "alfa"), "add the target question back")

	if introduced == readded || removed == readded {
		t.Fatal("the fixture did not make three distinct commits")
	}

	_, perEntry, err := GoldenSetCommit(t.Context(), repo.path, []goldenset.GoldenQuestion{kept, target})
	if err != nil {
		t.Fatalf("GoldenSetCommit: %v", err)
	}

	got := perEntry[goldenset.EntryIdentity(target)]
	if got.Hash != introduced {
		t.Errorf("the re-added entry is attributed to %s, want the commit that first introduced it "+
			"(%s); %s is the commit that added it back", got.Hash, introduced, readded)
	}
}

// TestParseCommitLine_PartialOutputIsNotFound covers the branch no git invocation can reach: `git
// log --format=%H%x00%aI` always emits a parseable RFC3339 stamp, so a truncated or garbled line
// only arrives from a git that failed halfway. Planting a defect on it turned nothing red through
// the public path, and the reachable-only-from-here branch is worth pinning directly: a CommitInfo
// with a hash and a zero time would read as an entry attributed to a moment before every baseline.
func TestParseCommitLine_PartialOutputIsNotFound(t *testing.T) {
	good, ok := parseCommitLine("abc123\x002026-08-11T09:30:00Z\ndef456\x002026-08-12T09:30:00Z\n")
	if !ok || good.Hash != "abc123" || !good.Found {
		t.Fatalf("a well-formed log did not parse: %+v (ok=%t)", good, ok)
	}
	if good.Time.Format(time.RFC3339) != "2026-08-11T09:30:00Z" {
		t.Errorf("the first line's timestamp is %s, want the first of the two", good.Time)
	}

	for name, out := range map[string]string{
		"empty":           "",
		"no separator":    "abc123 2026-08-11T09:30:00Z\n",
		"no timestamp":    "abc123\x00\n",
		"garbled stamp":   "abc123\x00not-a-date\n",
		"no hash":         "\x002026-08-11T09:30:00Z\n",
		"only whitespace": "   \n",
	} {
		t.Run(name, func(t *testing.T) {
			info, ok := parseCommitLine(out)
			if ok || info.Found {
				t.Errorf("%q parsed as found: %+v", out, info)
			}
			if info.Hash != "" {
				t.Errorf("%q produced a hash (%q) with no usable time, which reads as an entry "+
					"attributed to the zero instant", out, info.Hash)
			}
		})
	}
}

// TestFlagStaleEntries_UnattributedIsFlaggedNotWaived is the silent-pass this task would otherwise
// ship. An entry git cannot attribute has a zero time, and a zero time is before every baseline —
// so without the Found flag, "we do not know when this landed" comes out as "it predates the
// baseline, nothing to see".
func TestFlagStaleEntries_UnattributedIsFlaggedNotWaived(t *testing.T) {
	repo := newGitRepo(t)
	committed := goldenset.GoldenQuestion{Question: "a committed question", UID: uidA, Area: "alfa"}
	uncommitted := goldenset.GoldenQuestion{
		Question: "a question that only exists in the working tree", UID: uidB, Area: "beta",
	}

	body := fixtureCoverage + "questions:\n" + entry(committed.Question, committed.UID, "alfa")
	repo.commit(body, "add the committed question")

	// Written but never committed, which is exactly what an author has on disk mid-edit.
	if err := os.WriteFile(repo.path, []byte(body+entry(uncommitted.Question, uncommitted.UID, "beta")), 0o600); err != nil {
		t.Fatalf("writing the working-tree edit: %v", err)
	}

	questions := []goldenset.GoldenQuestion{committed, uncommitted}
	file, perEntry, err := GoldenSetCommit(t.Context(), repo.path, questions)
	if err != nil {
		t.Fatalf("GoldenSetCommit: %v", err)
	}

	if info := perEntry[goldenset.EntryIdentity(uncommitted)]; info.Found {
		t.Fatalf("an uncommitted entry was attributed to %s", info.Hash)
	}

	flagged := FlagStaleEntries(perEntry, file.Time)
	if !slices.Contains(flagged, goldenset.EntryIdentity(uncommitted)) {
		t.Error("the uncommitted entry was not flagged, so an entry with unknown authoring order " +
			"reads as one that predates the baseline")
	}
	if slices.Contains(flagged, goldenset.EntryIdentity(committed)) {
		t.Error("the committed entry was flagged against its own file commit")
	}

	stale := ResolveStale(flagged, perEntry, questions)
	if !strings.Contains(stale[0].Reason, "unknown") {
		t.Errorf("the reason %q does not say the authoring order is unknown", stale[0].Reason)
	}
}

// TestGoldenSetCommit_OutsideAGitRepositoryIsAnError is T6's "not a panic" done-when, and it is the
// same rule as everywhere else here: no provenance is a fact to report, not a section to omit.
func TestGoldenSetCommit_OutsideAGitRepositoryIsAnError(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "golden-set.yaml")
	if err := os.WriteFile(outside, []byte(fixtureCoverage), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}

	_, _, err := GoldenSetCommit(t.Context(), outside, nil)
	if err == nil {
		t.Fatal("a file outside any git repository resolved a provenance")
	}
	if !strings.Contains(err.Error(), "git repository") {
		t.Errorf("the error %q does not say what is missing", err)
	}
}

// TestGoldenSetCommit_UncommittedFileIsAnError separates "the file has no commit at all" from "some
// of its entries do not". The first means this run cannot say which version of the golden set it
// measured, and a hash of "" printed as a hash is worse than an error.
func TestGoldenSetCommit_UncommittedFileIsAnError(t *testing.T) {
	repo := newGitRepo(t)
	repo.commit("placeholder\n", "seed the repo so HEAD exists")

	untracked := filepath.Join(repo.dir, "other-set.yaml")
	if err := os.WriteFile(untracked, []byte(fixtureCoverage), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}

	_, _, err := GoldenSetCommit(t.Context(), untracked, nil)
	if err == nil {
		t.Fatal("a file that exists only in the working tree reported a commit")
	}
	if !strings.Contains(err.Error(), "working tree") {
		t.Errorf("the error %q does not say the file was never committed", err)
	}
}
