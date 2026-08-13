package eval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/danielmalka/go-knowrag/internal/goldenset"
)

// EntryIdentity is what makes a golden-set entry "the same question" across edits to the file.
//
// It hashes content, not position, because the obvious alternative does not work: `git blame` by
// line number reattributes every entry below any insertion, so rewrapping the file or moving one
// question rewrites the provenance of all the rest without a single question having changed. The
// NUL separator is there so that ("ab", "c") and ("a", "bc") cannot hash the same — a UUID contains
// no NUL, so the split point is unambiguous.
func EntryIdentity(q goldenset.GoldenQuestion) string {
	sum := sha256.Sum256([]byte(q.Question + "\x00" + q.UID))
	return hex.EncodeToString(sum[:])
}

// CommitInfo is the commit that introduced one entry.
//
// Found is not decoration. An entry git could not attribute has a zero Time, and a zero Time is
// before every baseline — so without this flag "we could not find out when this landed" would come
// out as "it predates the baseline, nothing to flag", which is the silent pass this package exists
// to refuse. FlagStaleEntries flags an unfound entry.
type CommitInfo struct {
	Hash  string    `json:"hash"`
	Time  time.Time `json:"time"`
	Found bool      `json:"found"`
}

// StaleEntry is one flagged entry, resolved back to readable text for the report.
type StaleEntry struct {
	Identity string `json:"identity"`
	Question string `json:"question"`
	Commit   string `json:"commit,omitempty"`
	Reason   string `json:"reason"`
}

// GoldenSetCommit resolves the file's current commit and each entry's introducing commit.
//
// One `git log -S` per entry, bounded by the golden set's own 50–100 cap — the count comes from the
// coverage table in the golden-set file, not from a constant here. `-S` searches for the literal
// question text as it appears in the YAML, which is why the identity hash (which never appears in
// the file) only keys the returned map.
//
// Known limitation, documented rather than papered over: two entries with byte-identical `question`
// text are indistinguishable to `-S` and would be attributed to the same commit. Authoring
// discipline catches that; nothing here can.
// The file-level commit comes back as a CommitInfo rather than a bare hash so callers have its
// timestamp without a second git call — FlagStaleEntries needs an instant, and "the commit that
// last touched the golden set" is the instant a run with no separate baseline compares against.
func GoldenSetCommit(
	ctx context.Context, path string, questions []goldenset.GoldenQuestion,
) (CommitInfo, map[string]CommitInfo, error) {
	dir := filepath.Dir(path)
	if _, err := git(ctx, dir, "rev-parse", "--show-toplevel"); err != nil {
		return CommitInfo{}, nil, fmt.Errorf("eval: %s is not inside a git repository, so the golden "+
			"set has no provenance to record: %w", path, err)
	}

	base := filepath.Base(path)
	out, err := git(ctx, dir, "log", "-1", "--format=%H%x00%aI", "--", base)
	if err != nil {
		return CommitInfo{}, nil, fmt.Errorf("eval: resolving the commit of %s: %w", path, err)
	}
	file, ok := parseCommitLine(out)
	if !ok {
		return CommitInfo{}, nil, fmt.Errorf("eval: %s has no commit of its own — it exists only in "+
			"the working tree, so this run cannot record which version of the golden set it measured", path)
	}

	perEntry := make(map[string]CommitInfo, len(questions))
	for _, q := range questions {
		perEntry[EntryIdentity(q)] = introducingCommit(ctx, dir, base, q.Question)
	}
	return file, perEntry, nil
}

// introducingCommit is the first commit whose diff changed the number of occurrences of the
// question text in the file — with --reverse, the one that introduced it.
//
// A git failure comes back as Found:false rather than as an error, so one unattributable entry does
// not throw away the provenance of the other ninety-nine. Unattributed is flagged downstream, never
// waived.
func introducingCommit(ctx context.Context, dir, file, question string) CommitInfo {
	out, err := git(ctx, dir, "log", "--reverse", "--format=%H%x00%aI", "-S"+question, "--", file)
	if err != nil {
		return CommitInfo{}
	}
	info, _ := parseCommitLine(out)
	return info
}

// parseCommitLine reads the first `<hash>NUL<rfc3339>` line of a git log. Anything it cannot read
// in full comes back as not-found, never as a partially-filled CommitInfo: a hash with a zero time
// would be an entry that looks attributed and compares as older than every baseline.
func parseCommitLine(out string) (CommitInfo, bool) {
	first, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	hash, stamp, ok := strings.Cut(first, "\x00")
	if !ok || hash == "" {
		return CommitInfo{}, false
	}
	when, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return CommitInfo{}, false
	}
	return CommitInfo{Hash: hash, Time: when, Found: true}, true
}

// FlagStaleEntries returns the identities of entries that must not be read as predating the
// baseline: those introduced after baselineCommitTime, and those git could not attribute at all.
//
// The second class is the one worth stating twice. "I do not know when this entry landed" and "this
// entry landed before the baseline" produce the same zero-valued time and opposite conclusions, so
// the unknown is flagged.
func FlagStaleEntries(perEntry map[string]CommitInfo, baselineCommitTime time.Time) []string {
	var flagged []string
	for identity, info := range perEntry {
		if !info.Found || info.Time.After(baselineCommitTime) {
			flagged = append(flagged, identity)
		}
	}
	// Sorted so two runs over the same map produce the same report; Go randomises map iteration.
	slices.Sort(flagged)
	return flagged
}

// ResolveStale turns flagged identities back into readable entries for the report.
func ResolveStale(
	flagged []string, perEntry map[string]CommitInfo, questions []goldenset.GoldenQuestion,
) []StaleEntry {
	byIdentity := make(map[string]goldenset.GoldenQuestion, len(questions))
	for _, q := range questions {
		byIdentity[EntryIdentity(q)] = q
	}

	out := make([]StaleEntry, 0, len(flagged))
	for _, id := range flagged {
		info := perEntry[id]
		entry := StaleEntry{Identity: id, Question: byIdentity[id].Question, Commit: info.Hash}
		if info.Found {
			entry.Reason = fmt.Sprintf("introduced by %s at %s, after the baseline this run evaluates",
				info.Hash, info.Time.Format(time.RFC3339))
		} else {
			entry.Reason = "git could not attribute an introducing commit, so its authoring order " +
				"relative to the baseline is unknown"
		}
		out = append(out, entry)
	}
	return out
}

// git runs one read-only git command in dir.
//
// #nosec G204 — every argument below is a literal from this file except the golden-set file name
// and the question text, and both are passed as separate argv elements to exec.Command, which
// spawns git directly with no shell. There is no string a question could contain that becomes a
// second command.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...) // #nosec G204
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}
