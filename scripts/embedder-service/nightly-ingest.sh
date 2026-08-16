#!/bin/sh
# Nightly ingest: bring the embedder up, process whatever changed, take it down.
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

"$ENSURE"
# Always stop, even if ingest failed: a leftover resident model is the thing this
# job exists to avoid. The exit status of ingest is what systemd reports.
status=0
"$BIN" ingest --vault both || status=$?
systemctl --user stop knowrag-embedder || true
exit "$status"
