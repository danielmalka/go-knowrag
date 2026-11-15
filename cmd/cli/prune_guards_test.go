package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danielmalka/go-knowrag/internal/config"
	"github.com/danielmalka/go-knowrag/internal/ingest"
	"github.com/danielmalka/go-knowrag/internal/vault"
)

// The two limits on an unattended prune: a ceiling on how much one run may delete, and the identity of the
// vault root. They exist because a root pointed at another vault, or a sync that left half of one behind,
// turns every missing uid into a "provably absent" note that `--prune --yes` would delete.

// --- the ceiling ---------------------------------------------------------------------------------

func TestCeiling_Limit(t *testing.T) {
	p := defaultCeiling()
	for _, tc := range []struct{ indexed, want int }{
		{0, 3},     // never below the floor: a tiny vault may still lose a note or two
		{10, 3},    // 5% of 10 is 1, the floor wins
		{60, 3},    // 5% of 60 is exactly 3
		{100, 5},   // 5% bounds a small vault
		{400, 20},  // 5% of 400 is 20: the two numbers meet
		{1000, 20}, // the absolute cap bounds a big vault: 5% of 1000 would be 50
	} {
		if got := p.limit(tc.indexed); got != tc.want {
			t.Errorf("limit(%d indexed) = %d, want %d", tc.indexed, got, tc.want)
		}
	}
	if got := (ceilingPolicy{MaxNotes: 2, MaxPercent: 50}).limit(1000); got != 2 {
		t.Errorf("a MaxNotes of 2 gave a limit of %d: the floor must not exceed the operator's own cap", got)
	}
}

func TestCeiling_Validate(t *testing.T) {
	for _, bad := range []ceilingPolicy{{0, 5}, {-1, 5}, {20, 0}, {20, -1}, {20, 101}} {
		if err := bad.validate(); err == nil {
			t.Errorf("%+v was accepted", bad)
		}
	}
	if err := defaultCeiling().validate(); err != nil {
		t.Errorf("the defaults were refused: %v", err)
	}
}

func orphansOf(vaultName string, n int) []ingest.Orphan {
	out := make([]ingest.Orphan, n)
	for i := range out {
		out[i] = ingest.Orphan{UID: uuid.New(), Vault: vaultName, Path: "wiki/gone.md", Points: 2}
	}
	return out
}

func pruneWith(t *testing.T, scope *pruneScope, orphans []ingest.Orphan, opts ingestOptions) (*recordingStore, ingest.Report, error) {
	t.Helper()
	store := &recordingStore{}
	report := ingest.Report{OrphansScanned: true, Orphans: orphans}
	err := pruneOrphans(t.Context(), store, defaultTenantID,
		map[string]string{"dono": t.TempDir(), "outro": t.TempDir()}, scope, opts, &report)
	return store, report, err
}

// A sync that left a few notes behind is a normal prune; one that lost a tenth of the vault is not.
func TestPrune_PartialSyncBelowTheCeilingPrunes_AboveItRefuses(t *testing.T) {
	below := &pruneScope{NoteCounts: map[string]int{"dono": 97}, Ceiling: defaultCeiling()} // 3 of 100: ceiling is 5
	store, report, err := pruneWith(t, below, orphansOf("dono", 3), ingestOptions{yes: true})
	if err != nil || report.PointsPruned != 6 || len(store.calls) != 3 {
		t.Fatalf("3 of 100 notes: err=%v pruned=%d calls=%v; want it to prune", err, report.PointsPruned, store.calls)
	}

	above := &pruneScope{NoteCounts: map[string]int{"dono": 90}, Ceiling: defaultCeiling()} // 10 of 100
	store, report, err = pruneWith(t, above, orphansOf("dono", 10), ingestOptions{yes: true})
	if err == nil || !strings.Contains(err.Error(), "10 of 100 indexed notes") || !strings.Contains(err.Error(), "ceiling of 5") {
		t.Fatalf("10 of 100 notes: err=%v, want a refusal that states the numbers", err)
	}
	if len(store.calls) != 0 || report.PointsPruned != 0 {
		t.Errorf("a refused prune touched the store: calls=%v pruned=%d", store.calls, report.PointsPruned)
	}
	if !strings.Contains(report.PruneSkipped, "above the ceiling") {
		t.Errorf("the report does not carry the reason (it is what the scheduled log line shows): %q", report.PruneSkipped)
	}
}

func TestPrune_TheAbsoluteCapBoundsABigVault(t *testing.T) {
	scope := &pruneScope{NoteCounts: map[string]int{"dono": 979}, Ceiling: defaultCeiling()} // 21 of 1000 = 2.1%
	_, _, err := pruneWith(t, scope, orphansOf("dono", 21), ingestOptions{yes: true})
	if err == nil || !strings.Contains(err.Error(), "ceiling of 20") {
		t.Fatalf("21 notes of a 1000-note vault: err=%v, want the 20-note cap to refuse it", err)
	}
	if _, _, err := pruneWith(t, &pruneScope{NoteCounts: map[string]int{"dono": 980}, Ceiling: defaultCeiling()},
		orphansOf("dono", 20), ingestOptions{yes: true}); err != nil {
		t.Fatalf("exactly 20 notes of 1000: %v, want it to prune", err)
	}
}

func TestPrune_CeilingIsPerVault(t *testing.T) {
	scope := &pruneScope{NoteCounts: map[string]int{"dono": 97, "outro": 5}, Ceiling: defaultCeiling()}
	orphans := append(orphansOf("dono", 3), orphansOf("outro", 5)...) // the small vault loses half
	if _, _, err := pruneWith(t, scope, orphans, ingestOptions{yes: true}); err == nil || !strings.Contains(err.Error(), `"outro"`) {
		t.Fatalf("err = %v, want the small vault named as the one over its ceiling", err)
	}
}

// --- a person can go past the limits, and only a person -------------------------------------------

func TestPrune_AboveTheLimits_AManualConfirmationGoesAheadAndAcceptsTheRoots(t *testing.T) {
	var asked []string
	scope := &pruneScope{
		NoteCounts: map[string]int{"dono": 90}, Ceiling: defaultCeiling(),
		Confirm: func(reasons []string) bool { asked = reasons; return true },
	}
	store, report, err := pruneWith(t, scope, orphansOf("dono", 10), ingestOptions{}) // no --yes: a terminal
	if err != nil || report.PointsPruned != 20 || len(store.calls) != 10 {
		t.Fatalf("err=%v pruned=%d calls=%d; a confirmed manual prune must go ahead", err, report.PointsPruned, len(store.calls))
	}
	if len(asked) == 0 || !scope.accepted {
		t.Errorf("asked=%v accepted=%v; the person must have been shown the reasons and be recorded as having accepted", asked, scope.accepted)
	}
}

func TestPrune_AboveTheLimits_ANoOrNobodyToAskRefuses(t *testing.T) {
	base := func(confirm func([]string) bool) *pruneScope {
		return &pruneScope{NoteCounts: map[string]int{"dono": 90}, Ceiling: defaultCeiling(), Confirm: confirm}
	}
	for name, tc := range map[string]struct {
		scope *pruneScope
		opts  ingestOptions
	}{
		"the person says no":        {base(func([]string) bool { return false }), ingestOptions{}},
		"no terminal, nobody asked": {base(nil), ingestOptions{}},
		// --yes is authorization without a person, even if a terminal happens to be attached.
		"--yes never asks": {base(func([]string) bool { t.Error("--yes asked a question"); return true }), ingestOptions{yes: true}},
	} {
		t.Run(name, func(t *testing.T) {
			store, _, err := pruneWith(t, tc.scope, orphansOf("dono", 10), tc.opts)
			if err == nil || len(store.calls) != 0 {
				t.Fatalf("err=%v calls=%v; want a refusal that deletes nothing", err, store.calls)
			}
			if tc.scope.accepted {
				t.Error("a refused prune recorded the roots as accepted")
			}
		})
	}
}

// repo and plain build the two kinds of identity a test needs without touching git.
func repo(path, commit string) vaultRoot {
	return vaultRoot{Path: path, Git: gitCommit, GitRoot: commit}
}
func plain(path string) vaultRoot { return vaultRoot{Path: path, Git: gitNone} }
func unknown(path string) vaultRoot {
	return vaultRoot{Path: path, Git: gitUnknown, GitNote: "git could not be run"}
}

// --- the identity of the vault root ---------------------------------------------------------------

// gitVault makes a git repository whose root commit differs from every other one made with a different
// seed (the commit date is part of the hash), and returns its path.
func gitVault(t *testing.T, seed int) string {
	t.Helper()
	root := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.email=t@example.com", "-c", "user.name=t"}, args...)...) // #nosec G204 -- fixed git arguments in a temp dir
		date := time.Date(2026, 1, 1, 0, 0, seed, 0, time.UTC).Format(time.RFC3339)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("commit", "-q", "--allow-empty", "-m", "root")
	return root
}

// The comparison fails CLOSED. A recorded repository is never demoted to "compare the path" because git
// failed this time, and only two PROVEN non-repositories are compared by path.
func TestVaultRoot_Comparison(t *testing.T) {
	for name, tc := range map[string]struct {
		recorded, now vaultRoot
		wantSame      bool
	}{
		"same repository, same path":              {repo("/x/a", "111"), repo("/x/a", "111"), true},
		"same repository moved to another folder": {repo("/x/a", "111"), repo("/y/b", "111"), true},
		"another repository at the same path":     {repo("/x/a", "111"), repo("/x/a", "222"), false},
		"another repository, another path":        {repo("/x/a", "111"), repo("/y/b", "222"), false},
		"recorded repository, git failed now":     {repo("/x/a", "111"), unknown("/x/a"), false},
		"recorded repository, no longer a repo":   {repo("/x/a", "111"), plain("/x/a"), false},
		"recorded plain folder, now a repo":       {plain("/x/a"), repo("/x/a", "111"), false},
		"two plain folders, same path":            {plain("/x/a"), plain("/x/a"), true},
		"two plain folders, another path":         {plain("/x/a"), plain("/y/b"), false},
		"recorded plain folder, git failed now":   {plain("/x/a"), unknown("/x/a"), false},
		"an old record with no git kind":          {vaultRoot{Path: "/x/a"}, plain("/x/a"), false},
		"a record that says unknown":              {unknown("/x/a"), unknown("/x/a"), false},
	} {
		got := tc.recorded.differenceFrom(tc.now) == ""
		if got != tc.wantSame {
			t.Errorf("%s: same = %v, want %v (%q)", name, got, tc.wantSame, tc.recorded.differenceFrom(tc.now))
		}
	}
}

// fakeGit replaces the git runner for one test.
func fakeGit(t *testing.T, fn func(args ...string) gitResult) {
	t.Helper()
	old := runGit
	runGit = func(_ context.Context, _ string, args ...string) gitResult { return fn(args...) }
	t.Cleanup(func() { runGit = old })
}

func TestGitIdentity_FailsClosed(t *testing.T) {
	notARepo := gitResult{exit: 128, stderr: "fatal: not a git repository (or any of the parent directories): .git"}
	for name, tc := range map[string]struct {
		answer func(args ...string) gitResult
		want   gitKind
	}{
		"git cannot be run (missing binary)": {func(...string) gitResult { return gitResult{err: exec.ErrNotFound} }, gitUnknown},
		"git timed out":                      {func(...string) gitResult { return gitResult{err: context.DeadlineExceeded} }, gitUnknown},
		"git crashed":                        {func(...string) gitResult { return gitResult{exit: 1, stderr: "fatal: bad object"} }, gitUnknown},
		"git says it is not a repository":    {func(...string) gitResult { return notARepo }, gitNone},
		"inside .git, not a work tree":       {func(...string) gitResult { return gitResult{stdout: "false\n"} }, gitUnknown},
		"the root commit cannot be read": {func(a ...string) gitResult {
			if a[0] == "rev-parse" {
				return gitResult{stdout: "true\n"}
			}
			return gitResult{err: context.DeadlineExceeded}
		}, gitUnknown},
		"a repository with no commit": {func(a ...string) gitResult {
			if a[0] == "rev-parse" {
				return gitResult{stdout: "true\n"}
			}
			return gitResult{exit: 128, stderr: "fatal: ambiguous argument 'HEAD'"}
		}, gitUnknown},
		"a repository with a root commit": {func(a ...string) gitResult {
			if a[0] == "rev-parse" {
				return gitResult{stdout: "true\n"}
			}
			return gitResult{stdout: "bbb\naaa\n"}
		}, gitCommit},
	} {
		t.Run(name, func(t *testing.T) {
			fakeGit(t, tc.answer)
			kind, commit, note := gitIdentity(t.Context(), t.TempDir())
			if kind != tc.want {
				t.Fatalf("kind = %q (%s), want %q", kind, note, tc.want)
			}
			if kind == gitCommit && commit != "aaa" {
				t.Errorf("commit = %q, want the smallest root %q", commit, "aaa")
			}
			if kind == gitUnknown && note == "" {
				t.Error("an unknown identity carries no note, so the refusal cannot say why")
			}
		})
	}
}

// "not a repository" is only believed when there is no .git entry: a .git that git rejects is a damaged
// repository.
func TestGitIdentity_ADamagedGitDirIsNotAPlainFolder(t *testing.T) {
	fakeGit(t, func(...string) gitResult {
		return gitResult{exit: 128, stderr: "fatal: not a git repository (or any of the parent directories): .git"}
	})
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if kind, _, _ := gitIdentity(t.Context(), root); kind != gitUnknown {
		t.Errorf("kind = %q for a folder with a .git git rejects, want unknown", kind)
	}
}

// With the real git binary gone from PATH nothing can be said about a repository, and nothing may be
// inferred from that.
func TestGitIdentity_RealBinaryAbsent(t *testing.T) {
	root := gitVault(t, 1)
	t.Setenv("PATH", t.TempDir())
	if kind, _, _ := gitIdentity(t.Context(), root); kind != gitUnknown {
		t.Errorf("kind = %q with no git on PATH, want unknown", kind)
	}
}

func TestIdentifyRoot_ReadsTheRealRepositoryAndFollowsSymlinks(t *testing.T) {
	one, other := gitVault(t, 1), gitVault(t, 2)
	idOne, err := identifyRoot(t.Context(), one)
	if err != nil || idOne.Git != gitCommit || idOne.GitRoot == "" {
		t.Fatalf("identifyRoot = %+v, %v; want a repository with a root commit", idOne, err)
	}
	idOther, _ := identifyRoot(t.Context(), other)
	if idOne.GitRoot == idOther.GitRoot || idOne.differenceFrom(idOther) == "" {
		t.Errorf("two different repositories share an identity: %+v %+v", idOne, idOther)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(one, link); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	if viaLink, _ := identifyRoot(t.Context(), link); viaLink.Path != idOne.Path {
		t.Errorf("a symlink to the vault has path %q, want the canonical %q", viaLink.Path, idOne.Path)
	}
	folder, _ := identifyRoot(t.Context(), t.TempDir())
	if folder.Git != gitNone || folder.GitRoot != "" {
		t.Errorf("a folder with no repository was classified %+v, want proven non-git", folder)
	}
}

func TestCheckRoots(t *testing.T) {
	recs := rootRecords{Roots: map[string]vaultRoot{"s|dono": repo("/x/a", "111")}}
	got := checkRoots(recs, "s", map[string]vaultRoot{
		"dono":  repo("/x/a", "111"),
		"novo":  plain("/n"),
		"outro": repo("/x/a", "222"),
	})
	if v := got["dono"]; v.Reason != "" || v.Mismatch {
		t.Errorf("the recorded root was not recognized: %+v", v)
	}
	if v := got["novo"]; v.Reason == "" || v.Mismatch {
		t.Errorf("a vault with no record must be a reason but not a mismatch: %+v", v)
	}
	if _, found := checkRoots(recs, "other-scope", map[string]vaultRoot{"dono": plain("/x/a")})["dono"]; !found {
		t.Error("a record of another index scope was trusted")
	}
}

// The switch the review named: the path now points at a DIFFERENT vault that has notes. Its notes are
// scanned fine, the ceiling may even pass (a vault of similar size), and the uids of the real vault are
// all orphans. The identity is what refuses.
func TestPrune_RootPointedAtAnotherVaultWithNotes_IsRefused(t *testing.T) {
	recs := rootRecords{Roots: map[string]vaultRoot{"s|dono": repo("/real", "111")}}
	verdicts := checkRoots(recs, "s", map[string]vaultRoot{"dono": repo("/real", "999")}) // other repo, same path
	// Same size, so the ceiling alone would let a swap of equal vaults through only if the orphans were
	// few; here the orphan count is 2 of 102 (under the ceiling) to prove the identity refuses by itself.
	scope := &pruneScope{NoteCounts: map[string]int{"dono": 100}, Ceiling: defaultCeiling(), Roots: verdicts}
	store, report, err := pruneWith(t, scope, orphansOf("dono", 2), ingestOptions{yes: true})
	if err == nil || !strings.Contains(err.Error(), "not the one last ingested") {
		t.Fatalf("err = %v, want a refusal about the vault root", err)
	}
	if len(store.calls) != 0 || !strings.Contains(report.PruneSkipped, "git root") {
		t.Errorf("calls=%v skipped=%q; nothing may be deleted and the reason must be in the report", store.calls, report.PruneSkipped)
	}
}

func TestPrune_NoOrphansNeedsNoIdentity(t *testing.T) {
	scope := &pruneScope{NoteCounts: map[string]int{"dono": 100}, Ceiling: defaultCeiling(),
		Roots: map[string]rootVerdict{"dono": {Mismatch: true, Reason: "the root changed"}}}
	if _, _, err := pruneWith(t, scope, nil, ingestOptions{yes: true}); err != nil {
		t.Fatalf("a run with nothing to prune was refused over the vault root: %v", err)
	}
}

func TestUpdateRecords(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	old := rootRecords{Roots: map[string]vaultRoot{"s|dono": repo("/real", "111")}}
	current := map[string]vaultRoot{"dono": repo("/other", "999"), "novo": plain("/n")}
	verdicts := checkRoots(old, "s", current)

	kept := updateRecords(old, "s", current, verdicts, false, now)
	if kept.Roots["s|dono"].GitRoot != "111" {
		t.Errorf("a mismatching root overwrote the record without anyone accepting it: %+v", kept.Roots["s|dono"])
	}
	if kept.Roots["s|novo"].Path != "/n" || !kept.Roots["s|novo"].RecordedAt.Equal(now) {
		t.Errorf("a vault with no record was not recorded: %+v", kept.Roots["s|novo"])
	}
	accepted := updateRecords(old, "s", current, verdicts, true, now)
	if accepted.Roots["s|dono"].GitRoot != "999" {
		t.Errorf("an accepted root was not recorded: %+v", accepted.Roots["s|dono"])
	}
	if old.Roots["s|dono"].GitRoot != "111" {
		t.Error("updateRecords edited its input")
	}
}

func TestRootRecords_RoundTripAndCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "vault-roots.json")
	if recs, err := loadRootRecords(path); err != nil || len(recs.Roots) != 0 {
		t.Fatalf("a missing file = %+v, %v; want empty and no error", recs, err)
	}
	want := rootRecords{Version: 1, Roots: map[string]vaultRoot{"s|dono": repo("/x", "abc")}}
	if err := saveRootRecords(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := loadRootRecords(path)
	if err != nil || got.Roots["s|dono"].GitRoot != "abc" {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRootRecords(path); err == nil {
		t.Error("a corrupt state file was read as if it were empty")
	}
}

// --- the scope a real run builds, and what it records ---------------------------------------------

func scopeFixture(t *testing.T, vaultPath string) (*config.Config, ingestOptions, []vault.ScanResult) {
	t.Helper()
	t.Setenv("KNOWRAG_STATE_DIR", t.TempDir())
	cfg := &config.Config{
		QdrantEndpoint: "q:6334", DefaultCollection: "interno_eg2",
		Vaults: map[string]config.VaultSettings{"dono": {Path: vaultPath}},
	}
	opts := ingestOptions{tenantID: defaultTenantID, yes: true, ceiling: defaultCeiling()}
	return cfg, opts, []vault.ScanResult{{Vault: "dono", Notes: make([]vault.Note, 100)}}
}

func TestPruneStep_FirstRunRecordsAndTheSecondRecognizesTheRoot(t *testing.T) {
	cfg, opts, scans := scopeFixture(t, gitVault(t, 1))
	first := newPruneScope(t.Context(), cfg, scans, opts, os.Stderr)
	if v := first.verdicts["dono"]; v.Reason == "" || v.Mismatch {
		t.Fatalf("first run verdict = %+v, want a reason (no record) that is not a mismatch", v)
	}
	first.record(opts, ingest.Report{}, os.Stderr)

	second := newPruneScope(t.Context(), cfg, scans, opts, os.Stderr)
	if v := second.verdicts["dono"]; v.Reason != "" {
		t.Fatalf("the run after a successful one does not recognize the root: %+v", v)
	}
}

func TestPruneStep_ADifferentVaultAtTheSamePathIsNotRecordedWithoutAcceptance(t *testing.T) {
	root := gitVault(t, 1)
	cfg, opts, scans := scopeFixture(t, root)
	s := newPruneScope(t.Context(), cfg, scans, opts, os.Stderr)
	s.record(opts, ingest.Report{}, os.Stderr)

	// The folder now holds another repository: same path, another root commit.
	if err := os.RemoveAll(filepath.Join(root, ".git")); err != nil {
		t.Fatal(err)
	}
	other := gitVault(t, 2)
	if err := os.Rename(filepath.Join(other, ".git"), filepath.Join(root, ".git")); err != nil {
		t.Fatal(err)
	}
	changed := newPruneScope(t.Context(), cfg, scans, opts, os.Stderr)
	if v := changed.verdicts["dono"]; !v.Mismatch {
		t.Fatalf("a different repository at the same path was recognized: %+v", v)
	}
	changed.record(opts, ingest.Report{}, os.Stderr)
	if again := newPruneScope(t.Context(), cfg, scans, opts, os.Stderr); !again.verdicts["dono"].Mismatch {
		t.Fatal("an unaccepted mismatch was recorded by an ordinary run, so the next prune would trust it")
	}

	opts.acceptVaultRoot = true
	changed.record(opts, ingest.Report{}, os.Stderr)
	if accepted := newPruneScope(t.Context(), cfg, scans, opts, os.Stderr); accepted.verdicts["dono"].Reason != "" {
		t.Fatalf("--accept-vault-root did not record the new root: %+v", accepted.verdicts["dono"])
	}
}

func TestPruneStep_DryRunAndFailedRunsRecordNothing(t *testing.T) {
	cfg, opts, scans := scopeFixture(t, gitVault(t, 1))
	dry := opts
	dry.dryRun = true
	s := newPruneScope(t.Context(), cfg, scans, dry, os.Stderr)
	s.record(dry, ingest.Report{}, os.Stderr)
	failed := ingest.Report{Results: []ingest.NoteResult{{State: ingest.StateFailed, Err: os.ErrInvalid}}}
	s.record(opts, failed, os.Stderr)
	if v := newPruneScope(t.Context(), cfg, scans, opts, os.Stderr).verdicts["dono"]; v.Reason == "" {
		t.Fatal("a dry run or a failed run recorded the vault root")
	}
}

func TestPruneStep_AnUnreadableStateFileNeverOpensTheGate(t *testing.T) {
	cfg, opts, scans := scopeFixture(t, gitVault(t, 1))
	stateFile, err := vaultStateFile()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(stateFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateFile, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newPruneScope(t.Context(), cfg, scans, opts, os.Stderr)
	if v := s.verdicts["dono"]; !v.Mismatch || !strings.Contains(v.Reason, "could not be checked") {
		t.Fatalf("verdict = %+v, want a refusal that says the records could not be checked", v)
	}
	s.record(opts, ingest.Report{}, os.Stderr)                      // must not overwrite what it could not read
	if raw, _ := os.ReadFile(stateFile); string(raw) != "garbage" { // #nosec G304 -- a file this test created
		t.Errorf("the unreadable state file was overwritten: %q", raw)
	}
}

// --- fail closed when git cannot be read -----------------------------------------------------------

// The case the review named: the recorded root has a commit, and a later check at the same path cannot read
// the repository. The identities must NOT "match by path", and the unattended prune must not go ahead.
func TestPruneStep_RecordedRepositoryButGitFailsNow_RefusesThePrune(t *testing.T) {
	root := gitVault(t, 1)
	cfg, opts, scans := scopeFixture(t, root)
	first := newPruneScope(t.Context(), cfg, scans, opts, os.Stderr)
	first.record(opts, ingest.Report{}, os.Stderr)
	if v := newPruneScope(t.Context(), cfg, scans, opts, os.Stderr).verdicts["dono"]; v.Reason != "" {
		t.Fatalf("setup: the recorded root is not recognized: %+v", v)
	}

	t.Setenv("PATH", t.TempDir()) // git is gone: same folder, same path, nothing readable
	broken := newPruneScope(t.Context(), cfg, scans, opts, os.Stderr)
	v := broken.verdicts["dono"]
	if !v.Mismatch || !strings.Contains(v.Reason, "could not be established") {
		t.Fatalf("verdict = %+v, want a mismatch saying the identity could not be established", v)
	}

	store, report, err := pruneWith(t, broken.scope, orphansOf("dono", 2), ingestOptions{yes: true})
	if err == nil || len(store.calls) != 0 || !strings.Contains(report.PruneSkipped, "could not be established") {
		t.Fatalf("err=%v calls=%v skipped=%q; the unattended prune must be refused with the reason", err, store.calls, report.PruneSkipped)
	}

	// Nor may the failed look rewrite the record as "path only": back with git, the repository is recognized.
	broken.record(opts, ingest.Report{}, os.Stderr)
	opts.acceptVaultRoot = true // even an acceptance cannot record an identity nobody could read
	broken.record(opts, ingest.Report{}, os.Stderr)
	t.Setenv("PATH", os.Getenv("PATH")+string(os.PathListSeparator)+"/usr/bin:/bin")
	if again := newPruneScope(t.Context(), cfg, scans, opts, os.Stderr).verdicts["dono"]; again.Reason != "" {
		t.Fatalf("a failed git check damaged the record: %+v", again)
	}
}

func TestPruneStep_PlainFoldersAreComparedByPath(t *testing.T) {
	root := t.TempDir() // no git here, and git says so
	cfg, opts, scans := scopeFixture(t, root)
	newPruneScope(t.Context(), cfg, scans, opts, os.Stderr).record(opts, ingest.Report{}, os.Stderr)
	if v := newPruneScope(t.Context(), cfg, scans, opts, os.Stderr).verdicts["dono"]; v.Reason != "" {
		t.Fatalf("a plain folder was not recognized by its path: %+v", v)
	}
	cfg.Vaults["dono"] = config.VaultSettings{Path: t.TempDir()}
	if v := newPruneScope(t.Context(), cfg, scans, opts, os.Stderr).verdicts["dono"]; !v.Mismatch {
		t.Fatalf("another plain folder was recognized as the same vault: %+v", v)
	}
}

// A vault that was a repository and no longer is (the .git was lost in a copy) is a different identity,
// not a plain folder to compare by path.
func TestPruneStep_ARepositoryThatStoppedBeingOneIsRefused(t *testing.T) {
	root := gitVault(t, 1)
	cfg, opts, scans := scopeFixture(t, root)
	newPruneScope(t.Context(), cfg, scans, opts, os.Stderr).record(opts, ingest.Report{}, os.Stderr)
	if err := os.RemoveAll(filepath.Join(root, ".git")); err != nil {
		t.Fatal(err)
	}
	if v := newPruneScope(t.Context(), cfg, scans, opts, os.Stderr).verdicts["dono"]; !v.Mismatch || !strings.Contains(v.Reason, "kind") {
		t.Fatalf("verdict = %+v, want a mismatch about the root changing kind", v)
	}
}

func TestUpdateRecords_NeverRecordsAnUnknownIdentity(t *testing.T) {
	old := rootRecords{Roots: map[string]vaultRoot{"s|dono": repo("/real", "111")}}
	cur := map[string]vaultRoot{"dono": unknown("/real"), "novo": unknown("/n")}
	got := updateRecords(old, "s", cur, checkRoots(old, "s", cur), true, time.Now()) // accepted, and still
	if got.Roots["s|dono"].GitRoot != "111" {
		t.Errorf("a recorded commit was demoted by an unreadable look: %+v", got.Roots["s|dono"])
	}
	if _, found := got.Roots["s|novo"]; found {
		t.Error("a vault whose identity is unknown was recorded")
	}
}

// --- concurrent ingestions do not erase each other's records --------------------------------------

func TestRecordRoots_ConcurrentRunsKeepEveryRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault-roots.json")
	const runs = 24
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			vaultName := fmt.Sprintf("v%02d", i)
			if err := recordRoots(path, "s", map[string]vaultRoot{vaultName: plain("/" + vaultName)}, false, time.Now()); err != nil {
				t.Errorf("recordRoots(%s): %v", vaultName, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	recs, err := loadRootRecords(path)
	if err != nil || len(recs.Roots) != runs {
		t.Fatalf("%d record(s) survived %d concurrent runs (%v): records were lost", len(recs.Roots), runs, err)
	}
}

func TestRecordRoots_NeverOverwritesAFileItCannotRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault-roots.json")
	if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recordRoots(path, "s", map[string]vaultRoot{"dono": plain("/x")}, false, time.Now()); err == nil {
		t.Error("recordRoots wrote over a file it could not decode")
	}
	if raw, _ := os.ReadFile(path); string(raw) != "garbage" { // #nosec G304 -- a file this test created
		t.Errorf("the unreadable file was changed: %q", raw)
	}
}
