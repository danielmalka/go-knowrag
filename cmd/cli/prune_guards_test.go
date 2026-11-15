package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func TestVaultRoot_SameVaultRules(t *testing.T) {
	a := vaultRoot{Path: "/x/a", GitRoot: "111"}
	for name, tc := range map[string]struct {
		other vaultRoot
		want  bool
	}{
		"the same path and repository":             {vaultRoot{Path: "/x/a", GitRoot: "111"}, true},
		"moved to another folder, same repository": {vaultRoot{Path: "/y/b", GitRoot: "111"}, true},
		"same path, a different repository":        {vaultRoot{Path: "/x/a", GitRoot: "222"}, false},
		"another path, another repository":         {vaultRoot{Path: "/y/b", GitRoot: "222"}, false},
		"no git root now, same path":               {vaultRoot{Path: "/x/a"}, true},
		"no git root now, another path":            {vaultRoot{Path: "/y/b"}, false},
	} {
		if got := a.sameVault(tc.other); got != tc.want {
			t.Errorf("%s: sameVault = %v, want %v", name, got, tc.want)
		}
	}
}

func TestIdentifyRoot_ReadsTheRealRepositoryAndFollowsSymlinks(t *testing.T) {
	one, other := gitVault(t, 1), gitVault(t, 2)
	idOne, err := identifyRoot(t.Context(), one)
	if err != nil || idOne.GitRoot == "" {
		t.Fatalf("identifyRoot = %+v, %v; want a git root", idOne, err)
	}
	idOther, _ := identifyRoot(t.Context(), other)
	if idOne.GitRoot == idOther.GitRoot || idOne.sameVault(idOther) {
		t.Errorf("two different repositories share an identity: %+v %+v", idOne, idOther)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(one, link); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	if viaLink, _ := identifyRoot(t.Context(), link); viaLink.Path != idOne.Path {
		t.Errorf("a symlink to the vault has path %q, want the canonical %q", viaLink.Path, idOne.Path)
	}
	plain, _ := identifyRoot(t.Context(), t.TempDir())
	if plain.GitRoot != "" {
		t.Errorf("a folder with no repository got git root %q", plain.GitRoot)
	}
}

func TestCheckRoots(t *testing.T) {
	recs := rootRecords{Roots: map[string]vaultRoot{"s|dono": {Path: "/x/a", GitRoot: "111"}}}
	got := checkRoots(recs, "s", map[string]vaultRoot{
		"dono":  {Path: "/x/a", GitRoot: "111"},
		"novo":  {Path: "/n"},
		"outro": {Path: "/x/a", GitRoot: "222"},
	})
	if v := got["dono"]; v.Reason != "" || v.Mismatch {
		t.Errorf("the recorded root was not recognized: %+v", v)
	}
	if v := got["novo"]; v.Reason == "" || v.Mismatch {
		t.Errorf("a vault with no record must be a reason but not a mismatch: %+v", v)
	}
	if _, found := checkRoots(recs, "other-scope", map[string]vaultRoot{"dono": {Path: "/x/a"}})["dono"]; !found {
		t.Error("a record of another index scope was trusted")
	}
}

// The switch the review named: the path now points at a DIFFERENT vault that has notes. Its notes are
// scanned fine, the ceiling may even pass (a vault of similar size), and the uids of the real vault are
// all orphans. The identity is what refuses.
func TestPrune_RootPointedAtAnotherVaultWithNotes_IsRefused(t *testing.T) {
	recs := rootRecords{Roots: map[string]vaultRoot{"s|dono": {Path: "/real", GitRoot: "111"}}}
	verdicts := checkRoots(recs, "s", map[string]vaultRoot{"dono": {Path: "/real", GitRoot: "999"}}) // other repo, same path
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
	old := rootRecords{Roots: map[string]vaultRoot{"s|dono": {Path: "/real", GitRoot: "111"}}}
	current := map[string]vaultRoot{"dono": {Path: "/other", GitRoot: "999"}, "novo": {Path: "/n"}}
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
	want := rootRecords{Version: 1, Roots: map[string]vaultRoot{"s|dono": {Path: "/x", GitRoot: "abc"}}}
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
