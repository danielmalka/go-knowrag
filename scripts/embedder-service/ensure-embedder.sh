#!/bin/sh
# Make sure the embedder service is up and its model is loaded, before an ingestion.
#
# The service process is always up (see knowrag-embedder.service); the model is loaded on demand and
# unloaded after an idle time. A scheduled ingestion would load it on its first call anyway, but its
# first call is the handshake-then-/tokenize path with short deadlines, so this loads it explicitly
# (POST /load) and fails here, with a clear message, if the GPU or the load is the problem.
#
# Host and port come from the same env file the unit reads. Override either on the command line if
# you must; do not hardcode a machine's loopback into a caller.
set -eu

HOST="${KNOWRAG_EMBEDDER_HOST:-127.0.0.1}"
PORT="${KNOWRAG_EMBEDDER_PORT:-7999}"
# How long the PROCESS may take to answer /health after `systemctl start`, and how long the LOAD may
# take. A first-ever load also downloads the weights (about 0.5 GB), so give it room.
UP_TIMEOUT="${KNOWRAG_EMBEDDER_UP_TIMEOUT:-60}"
LOAD_TIMEOUT="${KNOWRAG_EMBEDDER_LOAD_TIMEOUT:-300}"

if [ -f "${XDG_CONFIG_HOME:-$HOME/.config}/knowrag/embedder.env" ]; then
	set -a
	# shellcheck disable=SC1091
	. "${XDG_CONFIG_HOME:-$HOME/.config}/knowrag/embedder.env"
	set +a
	HOST="${KNOWRAG_EMBEDDER_HOST:-$HOST}"
	PORT="${KNOWRAG_EMBEDDER_PORT:-$PORT}"
fi

BASE="http://${HOST}:${PORT}"
systemctl --user start knowrag-embedder

deadline=$(( $(date +%s) + UP_TIMEOUT ))
body=""
while [ "$(date +%s)" -lt "$deadline" ]; do
	body=$(curl -fsS --max-time 2 "${BASE}/health" 2>/dev/null || true)
	[ -n "$body" ] && break
	sleep 1
done
if [ -z "$body" ]; then
	echo "ensure-embedder: ${BASE} did not answer /health within ${UP_TIMEOUT}s" >&2
	exit 1
fi
case "$body" in
*'"cuda": true'*) ;;
*)
	echo "ensure-embedder: the service reports no CUDA device (${body}); it has no CPU fallback" >&2
	exit 1
	;;
esac

if ! curl -fsS --max-time "$LOAD_TIMEOUT" -X POST "${BASE}/load" >/dev/null; then
	echo "ensure-embedder: ${BASE}/load failed or took over ${LOAD_TIMEOUT}s; see: journalctl --user -u knowrag-embedder" >&2
	exit 1
fi
