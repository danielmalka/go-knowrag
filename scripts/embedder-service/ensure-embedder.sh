#!/bin/sh
# Start knowrag-embedder and wait until /health reports loaded=true.
#
# The service binds only after the weights are in memory (28-37 s cold). Callers that
# cannot wait that long — the MCP search path has a 10 s deadline — must start this
# *before* they search, not from inside the search.
#
# Host and port come from the same env file the unit reads. Override either on the
# command line if you must; do not hardcode a machine's loopback into a caller.
set -eu

HOST="${KNOWRAG_EMBEDDER_HOST:-127.0.0.1}"
PORT="${KNOWRAG_EMBEDDER_PORT:-7999}"
TIMEOUT="${KNOWRAG_EMBEDDER_READY_TIMEOUT:-60}"

if [ -f "${XDG_CONFIG_HOME:-$HOME/.config}/knowrag/embedder.env" ]; then
	# shellcheck disable=SC1091
	set -a
	. "${XDG_CONFIG_HOME:-$HOME/.config}/knowrag/embedder.env"
	set +a
	HOST="${KNOWRAG_EMBEDDER_HOST:-$HOST}"
	PORT="${KNOWRAG_EMBEDDER_PORT:-$PORT}"
fi

systemctl --user start knowrag-embedder

deadline=$(( $(date +%s) + TIMEOUT ))
while [ "$(date +%s)" -lt "$deadline" ]; do
	body=$(curl -fsS --max-time 2 "http://${HOST}:${PORT}/health" 2>/dev/null || true)
	case "$body" in
	*'"loaded": true'*|*'\"loaded\":true'*)
		exit 0
		;;
	esac
	sleep 1
done

echo "ensure-embedder: ${HOST}:${PORT} did not report loaded=true within ${TIMEOUT}s" >&2
exit 1
