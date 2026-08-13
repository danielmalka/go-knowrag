#!/usr/bin/env bash
#
# Checks deploy/docker-compose.yml for the properties that are cheap to get wrong and expensive to
# notice: an unpinned image, a port on every interface, a credential written into a tracked file.
#
# It reads the file and nothing else — no Docker, no network, no deployed machine — so it runs in
# CI and on any checkout. What it therefore cannot tell you is whether the deployed instance matches
# this file; that is what redeploying from it is for.
#
# Takes a path so a test can point it at a deliberately broken copy. A verification script that has
# never been shown failing is a verification script nobody has verified.
set -uo pipefail

compose="${1:-deploy/docker-compose.yml}"
failures=0

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  failures=$((failures + 1))
}

if [ ! -f "$compose" ]; then
  fail "$compose does not exist"
  exit 1
fi

# Comments are stripped first, and that is not tidiness. This file's comments explain what it must
# not do — they say the words `:latest` and `0.0.0.0` in order to forbid them — so a checker reading
# raw text would fail on the explanation of the rule it is enforcing. (Learned the hard way one
# story earlier, in a CI workflow whose comment about no longer being disabled contained the literal
# that meant disabled.)
body=$(sed 's/[[:space:]]*#.*$//' "$compose")

# 1. The image is pinned to an exact patch on the line go.mod's client is pinned against.
image=$(grep -E '^\s*image:' <<<"$body" | head -1 | sed -E 's/^\s*image:\s*//; s/\s*$//')
case "$image" in
  qdrant/qdrant:v1.19.*)
    # A trailing wildcard would also accept `v1.19.x-unstable` or an empty patch; require digits.
    if ! grep -qE '^qdrant/qdrant:v1\.19\.[0-9]+$' <<<"$image"; then
      fail "image is '$image' — pin an exact patch on the v1.19.x line"
    fi
    ;;
  "")   fail "no image is declared" ;;
  *)    fail "image is '$image' — the deploy target is the v1.19.x line (S01 D3), never :latest and never another minor" ;;
esac

# 2. Every published port names one interface, and it is not a wildcard.
#
# The `- "…"` entries under `ports:` are the whole surface: docker publishes a two-field mapping on
# every address the host has, and does it past most host firewalls, so a missing bind address is a
# public Qdrant however the firewall is configured.
ports=$(awk '/^[[:space:]]*ports:/{inports=1; next} /^[[:space:]]*[a-z_]+:/{inports=0} inports && /^[[:space:]]*-/{print}' <<<"$body")
while IFS= read -r entry; do
  [ -n "$entry" ] || continue
  mapping=$(sed -E 's/^\s*-\s*//; s/^"//; s/"\s*$//' <<<"$entry")
  # Fields, counted on the colons docker separates on. Two fields = host:container = every
  # interface. Three = bind:host:container, which is the only shape allowed here.
  if [ "$(tr -cd ':' <<<"$mapping" | wc -c)" -lt 2 ]; then
    fail "port mapping '$mapping' publishes on every interface — give it an explicit bind address"
    continue
  fi
  bind=${mapping%%:*}
  case "$bind" in
    '${'*)  ;;  # from the environment, which deploy/.env.example documents as the mesh address
    0.0.0.0|'[::]'|'*'|'') fail "port mapping '$mapping' binds the wildcard '$bind'" ;;
    *) fail "port mapping '$mapping' hardcodes the bind address '$bind' — this file is tracked in a public repository, so the address belongs in deploy/.env" ;;
  esac
done <<<"$ports"

# 3. The API key comes from the environment and is mandatory.
key=$(grep -E 'QDRANT__SERVICE__API_KEY' <<<"$body" | head -1)
if [ -z "$key" ]; then
  fail "QDRANT__SERVICE__API_KEY is not set — Qdrant would come up with authentication disabled"
elif ! grep -q '\${' <<<"$key"; then
  fail "QDRANT__SERVICE__API_KEY is a literal — it must come from the environment"
elif ! grep -q ':?' <<<"$key"; then
  fail "QDRANT__SERVICE__API_KEY has no ':?' default-is-an-error marker — an unset key would start an unauthenticated Qdrant"
fi

# 4. The read-only key gets the same three checks, because it fails the same three ways.
readonly_key=$(grep -E 'QDRANT__SERVICE__READ_ONLY_API_KEY' <<<"$body" | head -1)
if [ -z "$readonly_key" ]; then
  fail "QDRANT__SERVICE__READ_ONLY_API_KEY is not set — the MCP server would have to present the administrative key"
elif ! grep -q '\${' <<<"$readonly_key"; then
  fail "QDRANT__SERVICE__READ_ONLY_API_KEY is a literal — it must come from the environment"
elif ! grep -q ':?' <<<"$readonly_key"; then
  fail "QDRANT__SERVICE__READ_ONLY_API_KEY has no ':?' default-is-an-error marker — an unset key would start a Qdrant with no read-only credential, and the MCP server would need the administrative one"
fi

# 5. The two keys are different values, and this is the check the whole read-only arrangement rests
# on. Setting both variables to the same string is not a weaker version of the separation — it is the
# administrative key wearing a second name, and Qdrant grants the client full write access. Nothing
# in the compose file can show it: to Qdrant they are two accepted keys, and to this script they are
# two variable references that both look right.
#
# So this is the one check that reads outside the compose file, and only when there is something to
# read. deploy/.env is gitignored and absent in CI, which means this check does not run there — that
# is correct rather than a gap, because CI has no keys to compare. It runs on the machine that has
# them, which is the machine that can get this wrong.
env_file="$(dirname "$compose")/.env"
if [ ! -f "$env_file" ]; then
  printf 'NOT CHECKED: whether the two keys are different values — %s does not exist here.\n' "$env_file"
  printf '             This check runs on the machine that holds the keys, not in CI.\n'
else
  admin_value=$(grep -E '^QDRANT_API_KEY=' "$env_file" | head -1 | cut -d= -f2-)
  readonly_value=$(grep -E '^QDRANT_READ_ONLY_API_KEY=' "$env_file" | head -1 | cut -d= -f2-)
  # Values are compared, never printed: this script's output goes to CI logs and terminals.
  if [ -z "$admin_value" ] || [ -z "$readonly_value" ]; then
    fail "$env_file leaves QDRANT_API_KEY or QDRANT_READ_ONLY_API_KEY empty — compose would refuse to start, and an empty value is not a key"
  elif [ "$admin_value" = "$readonly_value" ]; then
    fail "$env_file sets both keys to the same value — the read-only key is then the administrative key under another name, and the MCP server gets write access"
  fi
fi

# 6. The memory limit is NOT checked, and this script says so on every run rather than staying quiet
# about a check it does not make. It is absent by decision of 2026-08-13, not for want of a
# measurement: the only number available was NFR-6's budget for two processes sharing the host, and
# the embedder has left it. See the comment in the compose file itself.
printf 'NOT CHECKED: the container memory limit, which is absent by decision rather than oversight.\n'
printf '             Revisit on an OOM kill or a second memory consumer on that host — not because\n'
printf '             the line is missing.\n'

if [ "$failures" -gt 0 ]; then
  printf '\n%s: %d check(s) failed\n' "$compose" "$failures" >&2
  exit 1
fi
printf '%s: image pin, bind addresses and both API keys check out\n' "$compose"
