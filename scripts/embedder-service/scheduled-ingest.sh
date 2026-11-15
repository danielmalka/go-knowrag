#!/bin/sh
# Scheduled ingest: make sure the embedder is up with its model loaded, then process whatever changed
# and prune the points of notes that no longer exist in the vault.
#
# Nothing is stopped afterwards. The service process stays up and gives the model's VRAM back by
# itself after KNOWRAG_EMBEDDER_IDLE_UNLOAD seconds without a call (knowrag-embedder.service), so
# this job has no cleanup to forget and a failed ingest cannot leave the card held.
#
# Required in the environment (or in the files named by KNOWRAG_ENV / KNOWRAG_DOTENV):
#   QDRANT_ENDPOINT, KNOWRAG_ADMIN_QDRANT_API_KEY (or QDRANT_API_KEY as fallback),
#   EMBEDDER_ENDPOINT, DEFAULT_COLLECTION, KNOWRAG_VAULTS and the per-vault PATH/AREAS.
# KNOWRAG_BIN is the operator CLI. Working directory should be the checkout so
# schema/applied_state.json resolves.
set -eu

if [ -n "${KNOWRAG_ENV:-}" ] && [ -f "$KNOWRAG_ENV" ]; then
	set -a
	# shellcheck disable=SC1090
	. "$KNOWRAG_ENV"
	set +a
fi
if [ -n "${KNOWRAG_DOTENV:-}" ] && [ -f "$KNOWRAG_DOTENV" ]; then
	set -a
	# shellcheck disable=SC1090
	. "$KNOWRAG_DOTENV"
	set +a
fi

export KNOWRAG_ADMIN_QDRANT_API_KEY="${KNOWRAG_ADMIN_QDRANT_API_KEY:-${QDRANT_API_KEY:-}}"

if [ -n "${KNOWRAG_WORKDIR:-}" ]; then
	cd "$KNOWRAG_WORKDIR"
fi

ENSURE="${KNOWRAG_ENSURE:-$(dirname "$0")/ensure-embedder.sh}"
BIN="${KNOWRAG_BIN:-knowrag}"

# The scheduled run always prunes (owner's decision of 2026-11-15): the vault is the only source of
# truth, so the points of a note deleted from it leave Qdrant on the next run, and the vault's own git
# history is what remembers it. `--yes` is what authorizes that without a prompt: a timer has no
# terminal, and `knowrag ingest --prune` without it refuses outright when stdin is not one. A MANUAL
# `knowrag ingest --prune` still asks for confirmation; nothing here changes that.
#
# The CLI itself refuses to prune a run that could not read the index, that was interrupted, that was
# filtered, a note whose file is still on disk, a vault whose scan found no notes (an unmounted drive
# looks exactly like a vault whose every note was deleted), a run that would delete more than the ceiling
# allows (a sync that left half a vault behind), and a vault root that is not the one the last successful
# run used (the path now points at another vault). Those refusals come back as a non-zero exit with the
# reason in the report, which this wrapper passes on untouched. To go past one, a person runs
# `knowrag ingest --dry-run`, then `knowrag ingest --prune` in a terminal (it asks), or, for a vault that
# was moved on purpose, `knowrag ingest --accept-vault-root` once.
"$ENSURE"

report="$(mktemp)"
trap 'rm -f "$report"' EXIT

# The two limits on an unattended prune (see `knowrag ingest --help`): at most the smaller of N notes and
# P percent of a vault's indexed notes per run. The defaults are in the CLI; these two variables are how
# the operator moves them for the scheduled run, in the same env file as everything else.
set -- ingest --vault both --prune --yes
if [ -n "${KNOWRAG_PRUNE_MAX_NOTES:-}" ]; then
	set -- "$@" --prune-max-notes "$KNOWRAG_PRUNE_MAX_NOTES"
fi
if [ -n "${KNOWRAG_PRUNE_MAX_PERCENT:-}" ]; then
	set -- "$@" --prune-max-percent "$KNOWRAG_PRUNE_MAX_PERCENT"
fi

status=0
"$BIN" "$@" >"$report" 2>&1 || status=$?
cat "$report"

# One line a journal search can find, carrying the report's own orphans line (how many notes were
# pruned and how many points that removed) together with the exit status. The line is copied, not
# re-derived: the number an operator reads here is the one the report computed.
orphans="$(grep -m1 '^orphans:' "$report" || true)"
echo "scheduled-ingest: exit=$status; ${orphans:-orphans: no orphans line in the report}"
exit "$status"
