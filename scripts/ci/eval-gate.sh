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

output=$(go run ./cmd/cli eval "--${mode}" --json 2>&1)
status=$?
printf '%s\n' "$output"

if [ "$status" -eq 0 ]; then
  exit 0
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
