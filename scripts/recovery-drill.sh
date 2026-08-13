#!/usr/bin/env bash
#
# The recovery drill (S12 T9): destroy the deployed index and prove the content came back.
#
# docs/runbook.md documents the recovery — drop the volume, reingest everything — and nobody has
# ever run it. A recovery procedure that has never been executed is a guess written in the typeface
# of a fact, and the moment you need it is the worst moment to find out which one it was.
#
# THE DANGER IS THE POINT OF THE DESIGN. Step 4 deletes the whole index before rebuilding it, and
# there is no backup: the source of truth is the vaults, which live in git. If the rebuild dies
# halfway — embedder down, mesh flapping, disk full — the owner has no search until somebody fixes
# it by hand. So this script is more cowardly than the operator running it: phase 1 confirms that
# the rebuild is *possible* before anything is destroyed, and a preflight that fails aborts with the
# index untouched.
#
# It also insists on numbers. "It ran without errors" is not evidence of recovery — the index can
# come back smaller and look exactly like success. Points and distinct uids are counted per
# collection before and after, written to a file (not only to the terminal, which can close), and
# compared. Any difference fails the drill.
#
# --------------------------------------------------------------------------------------------
# WHAT THIS DRILL DOES NOT MEASURE — printed on every run, below, for the same reason
# scripts/verify-deploy.sh announces its own gaps: an unstated gap reads as a covered one.
# --------------------------------------------------------------------------------------------
#
# HOW IT IS DRIVEN AND TESTED: every external command — ssh, knowrag — is invoked by name and
# resolved through PATH, so cmd/cli/drill_test.go puts fakes in front of it and drives every phase
# without touching real infrastructure. That test is also where the dangerous behaviours are pinned:
# a failing preflight must not destroy, a missing --yes or a non-tty stdin must not destroy, a
# smaller index afterwards must fail, and the volume that gets removed must be the one discovered
# from the running container.
#
# CONFIGURATION comes from the environment, never from this file: it is tracked in a public
# repository and the address of the machine is not something it may carry. scripts/drill.env.example
# holds the placeholders.
set -uo pipefail

# The service name and the container path the volume is mounted at. Both are decided in
# deploy/docker-compose.yml, not here — this is a copy, and the reason it is a copy rather than a
# read is that this script must work against the VPS's compose file, which has diverged from the
# repository's. Change either there and this line has to follow.
readonly service="qdrant"
readonly mount="/qdrant/storage"

# The tenant --tenant defaults to. The value lives in cmd/cli/ingest.go as defaultTenantID; this is
# a copy of it, and the flag exists so a drill can be run against another one.
tenant="interno"
yes=0

usage() {
  cat <<'EOF'
usage: recovery-drill.sh [--tenant NAME] [--yes]

Destroys the deployed Qdrant volume and rebuilds the index from the vaults, then proves the
content came back by comparing point and uid counts taken before and after.

  --tenant NAME   the tenant to count and to reingest (default: interno)
  --yes           authorize the destruction. Required. Even with it, stdin must be a terminal.

Environment (see scripts/drill.env.example):
  KNOWRAG_DRILL_SSH           ssh destination of the machine running Qdrant
  KNOWRAG_DRILL_COMPOSE_DIR   directory on that machine holding its docker-compose.yml
  KNOWRAG_DRILL_STATE_DIR     where the before/after measurements are written (default: .drill)
  KNOWRAG_DRILL_UP_TIMEOUT    seconds to wait for Qdrant to answer after the rebuild (default: 300)

`knowrag` must be on PATH, configured for the same Qdrant and the same vaults the owner uses:
  go build -o "$HOME/bin/knowrag" ./cmd/cli
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    # The `[ $# -ge 2 ]` guard is not decoration: a bare `--tenant` at the end would leave `shift 2`
    # nothing to shift, and without `set -e` the loop would spin on the same argument forever.
    --tenant)
      [ $# -ge 2 ] || { printf -- '--tenant needs a value\n' >&2; exit 2; }
      tenant="$2"; shift 2 ;;
    --yes)    yes=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) printf 'unknown argument: %s\n\n' "$1" >&2; usage >&2; exit 2 ;;
  esac
done
[ -n "$tenant" ] || { printf -- '--tenant is empty: it matches no point, so the counts would come back zero over a healthy index\n' >&2; exit 2; }

: "${KNOWRAG_DRILL_SSH:=}"
: "${KNOWRAG_DRILL_COMPOSE_DIR:=}"
: "${KNOWRAG_DRILL_STATE_DIR:=.drill}"
: "${KNOWRAG_DRILL_UP_TIMEOUT:=300}"

run_id="$(date -u +%Y%m%dT%H%M%SZ)"
before_file="$KNOWRAG_DRILL_STATE_DIR/$run_id-before.txt"
after_file="$KNOWRAG_DRILL_STATE_DIR/$run_id-after.txt"
transcript="$KNOWRAG_DRILL_STATE_DIR/$run_id-transcript.txt"

# Discovered by preflight, consumed by the destruction. Empty until then, and the destruction
# refuses on empty — see destroy_and_rebuild.
container_id=""
volume_name=""
volume_used_kb=""
fs_avail_kb=""

say() { printf '%s\n' "$*"; printf '%s\n' "$*" >>"$transcript" 2>/dev/null; }
die() { printf 'ABORTED: %s\n' "$*" >&2; printf 'ABORTED: %s\n' "$*" >>"$transcript" 2>/dev/null; exit 1; }

# remote runs one command on the machine that holds Qdrant. Everything this script does to that
# machine goes through here, so the transcript is a complete record of what was done to it.
remote() {
  printf 'ssh> %s\n' "$1" >>"$transcript" 2>/dev/null
  ssh "$KNOWRAG_DRILL_SSH" "$1"
}

compose() {
  local dir
  dir=$(printf %q "$KNOWRAG_DRILL_COMPOSE_DIR")
  remote "cd $dir && docker compose $1"
}

# ---------------------------------------------------------------------------------------------
# Discovery. The volume name is READ FROM THE RUNNING CONTAINER and never taken from a compose
# file, and that is not defensiveness — it is a known open defect: the repository's
# deploy/docker-compose.yml declares `qdrant-storage` and the file deployed on the VPS declares
# `qdrant_storage`. A script that assumed either name would, against the machine using the other,
# delete nothing and create an empty volume — turning the drill into precisely the data loss it
# exists to rule out.
#
# An empty answer is a refusal, not a default. A bind mount reports no name, and so does a
# container that is not running; both leave this script without a thing to delete, and "no name"
# must never resolve to "the one in the repository".
# ---------------------------------------------------------------------------------------------
discover() {
  container_id=$(compose "ps -q $service" 2>/dev/null | tr -d '\r' | head -1)
  [ -n "$container_id" ] || return 1
  volume_name=$(remote "docker inspect -f '{{range .Mounts}}{{if eq .Destination \"$mount\"}}{{.Name}}{{end}}{{end}}' $container_id" 2>/dev/null | tr -d '[:space:]')
  [ -n "$volume_name" ] || return 1
  return 0
}

# du and df run INSIDE the running container rather than on the host, because the volume's files
# live under docker's root and reading them from the host needs privileges this script does not ask
# for. `df` on the mount reports the host filesystem backing the volume, which is the number that
# matters.
#
# This assumes the image ships du and df — true of the debian-slim base qdrant/qdrant:v1.19.x is
# built on (the tag deploy/docker-compose.yml pins), and NOT something this repository controls. If
# a future image drops them the commands print nothing, the integer check below rejects the empty
# answer, and the preflight fails with "could not read the volume's size" rather than skipping the
# check. Wrong in the safe direction, which is the only direction available here.
measure_space() {
  volume_used_kb=$(remote "docker exec $container_id du -sk $mount" 2>/dev/null | tr -d '\r' | awk 'NR==1{print $1}')
  fs_avail_kb=$(remote "docker exec $container_id df -Pk $mount" 2>/dev/null | tr -d '\r' | awk 'NR==2{print $4}')
  # Each on its own, never concatenated: `"" . "100000"` is a perfectly good integer, so a single
  # test over the pair accepts a run where one of the two numbers was never read — and the
  # comparison in the caller would then be `[ 100000 -lt "" ]`, which bash reports as an error and
  # every `if` reads as false. The disk check would pass by failing.
  case "$volume_used_kb" in *[!0-9]*|"") return 1 ;; esac
  case "$fs_avail_kb" in *[!0-9]*|"") return 1 ;; esac
  return 0
}

# ---------------------------------------------------------------------------------------------
# Phase 1. Everything that has to be alive for the rebuild to succeed, checked while the index is
# still intact. Nothing here writes or deletes anything.
# ---------------------------------------------------------------------------------------------
preflight_failures=0
check_fail() {
  printf 'PREFLIGHT FAIL: %s\n' "$1" >&2
  printf 'PREFLIGHT FAIL: %s\n' "$1" >>"$transcript" 2>/dev/null
  preflight_failures=$((preflight_failures + 1))
}

preflight() {
  preflight_failures=0
  say "== phase 1: preflight (nothing is destroyed here) =="

  [ -n "$KNOWRAG_DRILL_SSH" ] || check_fail "KNOWRAG_DRILL_SSH is unset — see scripts/drill.env.example"
  [ -n "$KNOWRAG_DRILL_COMPOSE_DIR" ] || check_fail "KNOWRAG_DRILL_COMPOSE_DIR is unset — see scripts/drill.env.example"
  if [ "$preflight_failures" -gt 0 ]; then return 1; fi

  if ! remote true >/dev/null 2>&1; then
    check_fail "cannot reach the Qdrant machine over ssh — the rebuild would have nothing to rebuild into"
  elif ! discover; then
    check_fail "could not read the $service container and its $mount volume from the running compose project — without the volume's real name this drill has nothing safe to delete"
  else
    say "  container:  $container_id"
    say "  volume:     $volume_name  (read from the container, never from a compose file)"
    if ! measure_space; then
      check_fail "could not read the volume's size and the free space behind it"
    elif [ "$fs_avail_kb" -lt "$volume_used_kb" ]; then
      check_fail "the filesystem behind $mount has ${fs_avail_kb} KiB free and the volume holds ${volume_used_kb} KiB — the rebuild would not fit"
    else
      # ponytail: a deliberately conservative floor. The deletion returns the volume's space before
      # the rebuild starts, so requiring that much free *on top* is roughly a 2x margin. It costs
      # nothing and it fails here, where failing is free, instead of during the rebuild.
      say "  disk:       ${volume_used_kb} KiB in use, ${fs_avail_kb} KiB free behind $mount"
    fi
  fi

  # The strongest reconstruct-ability check available, and it is one command because the dry run
  # already exercises every leg of the rebuild except the writing: cmd/cli/ingest.go's ingestScans
  # runs embedder.Handshake before ingest.Orchestrate in EVERY mode, dry run included, and
  # runIngest scans every configured vault and opens the same Qdrant connection a real run opens.
  # So a green dry run means the embedder answered and confirmed its backend, every vault was
  # readable and passed the scan contract, and Qdrant is reachable from this host.
  say "  reconstruct-ability: running 'knowrag ingest --dry-run' (writes nothing) ..."
  if ! knowrag ingest --dry-run --tenant "$tenant" >>"$transcript" 2>&1; then
    check_fail "'knowrag ingest --dry-run' failed — the embedder, a vault or Qdrant is not in a state that could rebuild the index (transcript: $transcript)"
  fi

  if [ "$preflight_failures" -gt 0 ]; then
    printf '\n%d preflight check(s) failed. Nothing was destroyed.\n' "$preflight_failures" >&2
    return 1
  fi
  say "  preflight passed"
  return 0
}

# ---------------------------------------------------------------------------------------------
# Phase 2. The numbers, to a file first. A terminal that closes mid-drill must not take the only
# copy of the "before" with it — without it there is nothing left to compare against, and the drill
# degrades into "it ran".
# ---------------------------------------------------------------------------------------------
measure_before() {
  say "== phase 2: measuring the index as it stands =="
  if ! knowrag stats --tenant "$tenant" >"$before_file" 2>>"$transcript"; then
    die "could not count the index before destroying it, so there would be nothing to compare against"
  fi
  cat "$before_file"
  cat "$before_file" >>"$transcript" 2>/dev/null
  say "  written to $before_file"

  # An empty index passes the comparison trivially — zero before, zero after, verdict green — which
  # is the one way this drill can report a recovery it never performed.
  if ! awk '$3 + 0 > 0 { found = 1 } END { exit !found }' "$before_file"; then
    die "every collection holds 0 points for tenant $tenant: there is nothing to lose and nothing to recover, so this drill would certify itself"
  fi
}

# ---------------------------------------------------------------------------------------------
# Phase 4. The destruction, and the authorization lives INSIDE it rather than in front of it.
# That placement is this repository's own lesson (CLAUDE.md, "quando um plante não fica vermelho"):
# a guard at the call site is a guard somebody can forget to call. discover() is re-run here for the
# same reason — it is the check that decides *what* gets deleted, so it belongs next to the deleting.
#
# What is NOT re-run here is the reconstruct-ability preflight, which costs a full vault scan. Its
# call site therefore does matter, and cmd/cli/drill_test.go pins it: a preflight that fails on the
# embedder must leave the volume alone.
# ---------------------------------------------------------------------------------------------
destroy_and_rebuild() {
  [ "$yes" = 1 ] || die "this deletes the entire index and --yes was not passed. Nothing was destroyed"
  # Never a prompt. A drill that hangs waiting for an answer nobody is there to give is worse than
  # either answer, and this is stricter than --prune's gate (cmd/cli/ingest_modes.go, confirmPrune,
  # which accepts --yes OR an answered prompt): here --yes is necessary and a terminal is necessary
  # too, so no scheduler can reach this line however its command was written.
  [ -t 0 ] || die "stdin is not a terminal. This is not something a scheduler may run: it destroys the index and needs a human watching. Nothing was destroyed"
  say "== phase 3: authorized — --yes was passed and a terminal is attached =="
  # Re-read, because the machine may have changed since the preflight approved it and this is the
  # answer the delete below is about to act on.
  if ! discover; then
    die "the $service container or its $mount volume could not be read — refusing to delete a volume whose name this script had to guess. Nothing was destroyed"
  fi

  say "== phase 4: destroying volume $volume_name and rebuilding =="
  # `docker compose down` without -v, then an explicit `docker volume rm` of the DISCOVERED name.
  # `down -v` would remove the volume the compose file declares, and the deployed file's declaration
  # is exactly the thing this script refuses to trust.
  compose "down" >>"$transcript" 2>&1 || die "'docker compose down' failed; the volume was not touched"
  remote "docker volume rm $volume_name" >>"$transcript" 2>&1 || die "removing volume $volume_name failed; run 'docker compose up -d' on that machine to bring the old index back"
  say "  volume $volume_name removed"
  compose "up -d" >>"$transcript" 2>&1 || die "'docker compose up -d' failed after the volume was removed — the index is GONE and Qdrant is not running. Fix compose on that machine, bring it up, then run 'knowrag schema apply' and 'knowrag ingest'"

  say "  waiting for Qdrant to answer (up to ${KNOWRAG_DRILL_UP_TIMEOUT}s) ..."
  local deadline=$((SECONDS + KNOWRAG_DRILL_UP_TIMEOUT))
  # `schema apply` is the readiness probe and the provisioning step at once: it is idempotent by
  # declaration (cmd/cli/schema.go's Long: "Running it twice performs no writes the second time"),
  # it needs the server up, and the collections have to be recreated anyway now that the volume that
  # held them is gone.
  until knowrag schema apply >>"$transcript" 2>&1; do
    if [ "$SECONDS" -ge "$deadline" ]; then
      die "Qdrant did not come back within ${KNOWRAG_DRILL_UP_TIMEOUT}s. The index is EMPTY: once it answers, run 'knowrag schema apply' and 'knowrag ingest --tenant $tenant'"
    fi
    sleep 5
  done
  say "  collections recreated"

  # No --full. The index is empty, so every note is new and the incremental path writes all of them;
  # --full only skips the integrity short-circuit, which has nothing to short-circuit here.
  say "  reingesting (this is the long one) ..."
  if ! knowrag ingest --tenant "$tenant" >>"$transcript" 2>&1; then
    die "the reingestion failed. The index is PARTIAL: rerun 'knowrag ingest --tenant $tenant' — it is incremental, so it resumes rather than starting over (transcript: $transcript)"
  fi
  say "  reingestion finished"
}

# ---------------------------------------------------------------------------------------------
# Phase 5. The verdict, and a difference is the verdict rather than a remark under it. Fewer points
# than before means content did not come back. More means something else wrote into this tenant
# while the drill was running, which does not prove recovery either — it means these two numbers
# are no longer about the same thing.
# ---------------------------------------------------------------------------------------------
verdict() {
  say "== phase 5: measuring what came back =="
  if ! knowrag stats --tenant "$tenant" >"$after_file" 2>>"$transcript"; then
    die "the rebuild finished but the index could not be counted, so this drill has no verdict. Before: $before_file"
  fi
  cat "$after_file"
  cat "$after_file" >>"$transcript" 2>/dev/null
  say "  written to $after_file"

  local diffs
  diffs=$(compare_counts "$before_file" "$after_file")
  if [ -n "$diffs" ]; then
    printf '%s\n' "$diffs" >&2
    printf '%s\n' "$diffs" >>"$transcript" 2>/dev/null
    die "the index that came back is not the index that was there. Before: $before_file  After: $after_file"
  fi
  say ""
  say "RECOVERED: every collection came back with the same point and uid counts."
  say "  before: $before_file"
  say "  after:  $after_file"
  say "  transcript: $transcript"
}

# compare_counts prints one line per disagreement and nothing at all when the two files agree.
# `knowrag stats` writes "<collection> points: <n> uids: <n>" per line, sorted, from
# cmd/cli/stats.go's writeStats — the sorting is what makes these two files comparable at all.
compare_counts() {
  awk '
    NR == FNR { bp[$1] = $3; bu[$1] = $5; seen[$1] = 1; next }
    {
      if (!($1 in seen)) { print "DIFFERENCE: collection " $1 " exists now and did not before" ; next }
      if ($3 != bp[$1]) print "LOST: collection " $1 " had " bp[$1] " point(s) and came back with " $3
      if ($5 != bu[$1]) print "LOST: collection " $1 " had " bu[$1] " uid(s) and came back with " $5
      delete seen[$1]
    }
    END { for (c in seen) print "LOST: collection " c " was there before and is absent now" }
  ' "$1" "$2"
}

main() {
  mkdir -p "$KNOWRAG_DRILL_STATE_DIR" || die "cannot create $KNOWRAG_DRILL_STATE_DIR"

  cat <<EOF
This drill destroys the deployed index and rebuilds it from the vaults.

NOT MEASURED: how long search was unavailable. This proves the content came back; it says nothing
              about the outage the owner would have felt.
NOT MEASURED: partial recovery. It reingests every vault; restoring one of them is not exercised.
NOT MEASURED: a failure in the middle of the reingestion. If the rebuild dies there, this script
              stops and the index stays incomplete until somebody reruns the ingestion. Shrinking
              that risk is what phase 1 is for; it does not remove it.
NOT MEASURED: anything about the machine except this one volume — no compose drift, no image pin,
              no bind address. scripts/verify-deploy.sh is what reads those.

EOF

  preflight || exit 1
  measure_before
  destroy_and_rebuild
  verdict
}

main
