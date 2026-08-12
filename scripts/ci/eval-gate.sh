#!/usr/bin/env bash
#
# Runs one quality gate and decides what its outcome means for CI.
#
# There are three outcomes and they are not two:
#
#   the gate passed          -> exit 0, nothing to say
#   the gate failed          -> exit non-zero, CI is red, somebody broke recall or isolation
#   the gate does not exist  -> exit 0 with a loud warning naming the story that builds it
#
# The third is why this script exists instead of a one-line `run:`. These two jobs used to be
# `if: false` with a TODO, which reports nothing on every push and would also report nothing on the
# push that broke them. Making them permanently red would be worse: a gate that is always failing is
# a gate everybody learns to scroll past, and then it is off too, just more expensively.
#
# So the job runs the real command and fails on everything except the one pending reason. That
# reason is recognised by a sentinel, not by a flag somebody has to remember to flip: the day the
# harness lands, `eval` stops answering with it and this branch stops being taken by itself.
#
# Hermetic by construction, which is what the job names claim: `eval` builds no Qdrant client and
# reads no vault, so this touches no network and no real data. If a future harness needs either,
# that dependency has to arrive here explicitly rather than by the command quietly acquiring it.
set -uo pipefail

mode="${1:?usage: eval-gate.sh golden|isolation}"
shift

# Which modes may still answer "pending", and the list is empty because both gates this build ships
# measure: S10 wired the golden harness to cmd/cli/eval.go, S11 wired the isolation suite behind
# `eval --isolation`, and neither can produce the sentinel any more — internal/eval/eval.go answers
# a misused golden gate with ErrNoSearcher, and its isolation gate has no refusal path at all.
#
# A mode on this list is a mode whose failure exits 0 with a warning. That is the right reading for
# a story nobody has built and the wrong one for a gate that exists, so the list being empty is what
# keeps every failure a failure.
#
# The branch below is therefore unreachable today, and it stays for the case that created it: a mode
# is added here before its harness is written, and the alternative for that job is what this script
# was written to replace — reporting nothing on every push, including the push that broke it.
pending_modes=""

# Must equal eval.ErrNotImplemented.Error() (internal/eval/eval.go). Nothing at run time can check
# that equality — this is a shell script and that is a Go value — so
# TestCIWorkflow_PendingSentinelMatchesTheErrorItLooksFor in cmd/cli reads this line and compares
# the two.
not_implemented="eval: not implemented"

# The one part this script can check about its own sentinel, and it checks it because the failure is
# silent in the worst direction: `grep -qF ""` matches every line, so an emptied sentinel turns the
# pending branch below into "any failure at all is a pending harness, exit 0" — compile errors,
# missing modules and panics included. The Go test above is the real guard; this is what still holds
# if somebody edits this file without running it.
if [ "${#not_implemented}" -lt 8 ]; then
  printf 'eval-gate: the pending sentinel is %d character(s) — too short to identify anything\n' \
    "${#not_implemented}" >&2
  exit 1
fi

# "$@" is whatever the workflow passes after the mode — the golden job names its fixture golden set
# and corpus and its --min-recall there, so the numbers live next to the job that asserts them
# rather than inside this script.
output=$(go run ./cmd/cli eval "--${mode}" "$@" --json 2>&1)
status=$?
printf '%s\n' "$output"

if [ "$status" -eq 0 ]; then
  exit 0
fi

if [[ " $pending_modes " != *" $mode "* ]]; then
  printf '::error title=%s gate failed::the %s gate has a harness and it did not pass\n' "$mode" "$mode" >&2
  exit "$status"
fi

if grep -qF "$not_implemented" <<<"$output"; then
  printf '::warning title=%s gate pending::the %s gate has no harness yet — nothing was measured\n' \
    "$mode" "$mode"
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    printf '**%s gate: pending.** No harness yet, so nothing was measured. This job is not a pass.\n' \
      "$mode" >>"$GITHUB_STEP_SUMMARY"
  fi
  exit 0
fi

exit "$status"
