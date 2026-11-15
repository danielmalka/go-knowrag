#!/bin/sh
# Scheduled ingest: make sure the embedder is up with its model loaded, then process whatever changed.
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

"$ENSURE"
exec "$BIN" ingest --vault both
