#!/bin/sh
# Daytime incremental ingest: process whatever changed, leave the embedder as it is.
#
# The 03:00 job starts the model, ingests, and stops it. This one runs inside the
# 07:00–23:00 search window, so it must not take the model down. If the model is
# already up, just ingest. If it is not, bring it up and leave it — the down
# timer still owns the end of the day.
#
# Same env contract as nightly-ingest.sh.
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
"$BIN" ingest --vault both
