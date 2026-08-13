#!/usr/bin/env bash
#
# The prune drill (S12 T8): run --prune against the real index, with a real note removed.
#
# --prune has tests and has never been pointed at the deployed index with an actual note missing.
# The tests prove the logic; this proves the procedure — that the orphan the operator would see is
# the note they removed, that the deletion lands, and that nothing else moves.
#
# It is far simpler than scripts/recovery-drill.sh and follows the same rule: counts before, counts
# after, and a difference is the verdict rather than a footnote.
#
# THE NOTE IS MOVED ASIDE, NOT DELETED, and it is moved back by a trap on every exit path including
# a Ctrl-C. To the vault scan the two are the same event — the file is not there — so the drill gets
# the real behaviour without asking anyone to delete something they wrote. The restore is also what
# the last phase proves worked: the index has to come back to the exact numbers it started with.
#
# WHAT THIS DRILL DOES NOT PROVE — also printed on every run:
#   - that a note deleted *for real* behaves the same. It is the same event to vault.ScanVault, but
#     nothing here re-derives that; it is an argument, not a measurement.
#   - anything about --prune over more than one deleted note at a time.
#   - anything about a prune that fails partway through. internal/ingest/prune.go stops at the first
#     failed delete and reports what it removed; that path is unit-tested and not exercised here.
#
# HOW IT IS DRIVEN AND TESTED: `knowrag` is invoked by name through PATH, so cmd/cli/drill_test.go
# fakes it and drives every phase without a real index.
set -uo pipefail

tenant="interno"
yes=0
vault=""
note=""

usage() {
  cat <<'EOF'
usage: prune-drill.sh [--tenant NAME] --yes <vault> <path/inside/the/vault.md>

Moves one note out of a vault, reingests, confirms it shows up as the only orphan, prunes it,
confirms the points went away and that nothing else did, then restores the note and reingests
until the counts are back where they started.

  <vault>   a name from KNOWRAG_VAULTS
  <path>    the note's path relative to that vault's root, exactly as the index holds it
  --tenant  the tenant to count and to ingest (default: interno)
  --yes     authorize the deletion. Required. Even with it, stdin must be a terminal.

Environment: the same KNOWRAG_VAULT_<NAME>_PATH the CLI reads (internal/config/config.go).
  KNOWRAG_DRILL_STATE_DIR   where the measurements are written (default: .drill)
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --tenant)
      [ $# -ge 2 ] || { printf -- '--tenant needs a value\n' >&2; exit 2; }
      tenant="$2"; shift 2 ;;
    --yes) yes=1; shift ;;
    -h|--help) usage; exit 0 ;;
    -*) printf 'unknown flag: %s\n\n' "$1" >&2; usage >&2; exit 2 ;;
    *)
      if [ -z "$vault" ]; then vault="$1"
      elif [ -z "$note" ]; then note="$1"
      else printf 'unexpected argument: %s\n\n' "$1" >&2; usage >&2; exit 2
      fi
      shift ;;
  esac
done

[ -n "$vault" ] && [ -n "$note" ] || { usage >&2; exit 2; }
[ -n "$tenant" ] || { printf -- '--tenant is empty: it matches no point\n' >&2; exit 2; }

: "${KNOWRAG_DRILL_STATE_DIR:=.drill}"

run_id="$(date -u +%Y%m%dT%H%M%SZ)"
before_file="$KNOWRAG_DRILL_STATE_DIR/$run_id-prune-before.txt"
after_file="$KNOWRAG_DRILL_STATE_DIR/$run_id-prune-after.txt"
restored_file="$KNOWRAG_DRILL_STATE_DIR/$run_id-prune-restored.txt"
transcript="$KNOWRAG_DRILL_STATE_DIR/$run_id-prune-transcript.txt"

say() { printf '%s\n' "$*"; printf '%s\n' "$*" >>"$transcript" 2>/dev/null; }
die() { printf 'ABORTED: %s\n' "$*" >&2; printf 'ABORTED: %s\n' "$*" >>"$transcript" 2>/dev/null; exit 1; }

# The vault root comes from the same variable the CLI reads. The mangling rule — uppercase, `-`
# becomes `_` — is internal/config/config.go's, not this script's invention: a vault named
# `my-notes` is configured through KNOWRAG_VAULT_MY_NOTES_PATH.
vault_root_var="KNOWRAG_VAULT_$(printf '%s' "$vault" | tr '[:lower:]-' '[:upper:]_')_PATH"
vault_root="${!vault_root_var:-}"

# stats_points sums the point column of a `knowrag stats` file. The format is
# "<collection> points: <n> uids: <n>" from cmd/cli/stats.go's writeStats.
stats_points() { awk '{ n += $3 } END { print n + 0 }' "$1"; }
stats_uids()   { awk '{ n += $5 } END { print n + 0 }' "$1"; }

# take_stats writes the counts to a file rather than a variable for the same reason
# scripts/recovery-drill.sh does: a terminal that closes must not take the only copy of a number
# this drill is about to compare against.
take_stats() {
  knowrag stats --tenant "$tenant" >"$1" 2>>"$transcript" || die "could not count the index ($2)"
  cat "$1" >>"$transcript" 2>/dev/null
  say "  $2: $(stats_points "$1") point(s), $(stats_uids "$1") uid(s)  -> $1"
}

# The trap handler. It reads globals rather than arguments because a trap takes none, and it is
# written to be safe when nothing has been moved yet: the trap is installed after `stash` is set,
# but a handler that assumes that is a handler that breaks the day somebody moves the install.
restore() {
  if [ -n "${stash:-}" ] && [ -f "$stash" ]; then
    if mv "$stash" "${note_path:-}"; then
      say "  restored ${note_path:-}"
    else
      printf 'THE NOTE WAS NOT RESTORED. It is at %s — move it back to %s by hand.\n' "$stash" "${note_path:-}" >&2
    fi
  fi
}

main() {
  mkdir -p "$KNOWRAG_DRILL_STATE_DIR" || die "cannot create $KNOWRAG_DRILL_STATE_DIR"

  cat <<'EOF'
This drill deletes points from the deployed index and moves one note out of a vault.

NOT PROVEN: that a note deleted for real behaves like one moved aside. It is the same event to the
            vault scan, but that is an argument, not something measured here.
NOT PROVEN: --prune over more than one deleted note, or a prune that fails partway through
            (internal/ingest/prune.go stops at the first failed delete; unit-tested, not run here).
NOT PROVEN: anything about a vault this run did not scan. --prune refuses --only for that reason.

EOF

  [ -n "$vault_root" ] || die "$vault_root_var is unset, so this script does not know where vault '$vault' is"
  note_path="$vault_root/$note"
  [ -f "$note_path" ] || die "$note_path is not a file — pass the note's path relative to the vault root, exactly as the index holds it"

  say "== 1: counting the index as it stands =="
  take_stats "$before_file" "before"
  local before_points before_uids
  before_points=$(stats_points "$before_file")
  before_uids=$(stats_uids "$before_file")
  [ "$before_points" -gt 0 ] || die "the index holds 0 points for tenant $tenant: there is nothing to prune, so this drill would certify itself"

  # The authorization sits here, immediately in front of the first thing that changes anything, and
  # is stricter than --prune's own gate (cmd/cli/ingest_modes.go, confirmPrune: --yes OR an answered
  # prompt). This one needs both, so no scheduler can reach the deletion however it was invoked, and
  # it never prompts — a drill that hangs waiting for an answer is worse than either answer.
  [ "$yes" = 1 ] || die "this deletes points from the real index and --yes was not passed. Nothing was changed"
  [ -t 0 ] || die "stdin is not a terminal. This needs a human watching. Nothing was changed"

  stash="$KNOWRAG_DRILL_STATE_DIR/$run_id-$(basename "$note")"
  trap restore EXIT INT TERM
  mv "$note_path" "$stash" || die "could not move $note_path aside"
  say "== 2: $note is out of the vault (parked at $stash) =="

  # No --prune yet. This is the run the operator would actually look at before deciding, and the
  # thing being confirmed is that the report names this note and only this note.
  say "== 3: ingest without --prune — the orphan should be reported, not deleted =="
  local report
  report=$(knowrag ingest --tenant "$tenant" 2>&1) || die "the ingestion failed before anything was pruned: $report"
  printf '%s\n' "$report" | tee -a "$transcript"
  printf '%s\n' "$report" | grep -qF "  - $vault/$note (" \
    || die "the run did not report $vault/$note as an orphan. Nothing was pruned"
  # Exactly one. The count is on the summary line — "orphans: N note(s) deleted from the vault" —
  # from internal/ingest/report.go's orphanLines. More than one means something else went missing
  # too, and pruning on that state would delete a note this drill never chose.
  printf '%s\n' "$report" | grep -qE '^orphans: 1 note\(s\) deleted from the vault,' \
    || die "the run reported orphans other than $vault/$note. Refusing to prune a state this drill did not set up"

  # The point count is read off the same line the report prints, so the number this drill checks
  # against is the number the operator was shown.
  local orphan_points
  orphan_points=$(printf '%s\n' "$report" | sed -n "s|^  - $vault/$note (\([0-9]*\) point(s).*|\1|p" | head -1)
  case "$orphan_points" in ""|*[!0-9]*) die "could not read the orphan's point count out of the report" ;; esac
  say "  reported: 1 orphan, $orphan_points point(s)"

  say "== 4: ingest --prune --yes =="
  report=$(knowrag ingest --tenant "$tenant" --prune --yes 2>&1) || die "the prune run failed: $report"
  printf '%s\n' "$report" | tee -a "$transcript"
  printf '%s\n' "$report" | grep -qF "$orphan_points point(s), all removed" \
    || die "the prune did not report all $orphan_points point(s) removed"

  say "== 5: counting what the prune actually moved =="
  take_stats "$after_file" "after the prune"
  local after_points after_uids
  after_points=$(stats_points "$after_file")
  after_uids=$(stats_uids "$after_file")

  # The two assertions that carry the whole drill. The first is that the prune removed what it said
  # it removed. The second is what proves nothing else was touched: if some other note's points had
  # gone too, the total would have fallen by more than the orphan's count — and the uid total by
  # more than one. A prune of one note is the only thing that produces exactly this pair.
  [ "$after_points" -eq "$((before_points - orphan_points))" ] \
    || die "the index went from $before_points to $after_points point(s); removing $vault/$note should have cost exactly $orphan_points"
  [ "$after_uids" -eq "$((before_uids - 1))" ] \
    || die "the index went from $before_uids to $after_uids uid(s); removing one note should have cost exactly one"
  say "  exactly $orphan_points point(s) and 1 uid left the index, and nothing else did"

  say "== 6: putting the note back =="
  restore
  trap - EXIT INT TERM
  stash=""
  knowrag ingest --tenant "$tenant" >>"$transcript" 2>&1 || die "the restoring ingestion failed — $note is back on disk but not back in the index; rerun 'knowrag ingest --tenant $tenant'"
  take_stats "$restored_file" "after restoring"
  local restored_points restored_uids
  restored_points=$(stats_points "$restored_file")
  restored_uids=$(stats_uids "$restored_file")
  [ "$restored_points" -eq "$before_points" ] && [ "$restored_uids" -eq "$before_uids" ] \
    || die "the index did not come back to where it started: $before_points/$before_uids before, $restored_points/$restored_uids now. $note is on disk; rerun the ingestion"

  say ""
  say "PRUNE CONFIRMED: $vault/$note was reported as the only orphan, its $orphan_points point(s)"
  say "were deleted, no other point moved, and the index is back where it started."
  say "  transcript: $transcript"
}

main
