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

# 4. The memory limit is NOT checked, and this script says so on every run rather than staying
# quiet about a check it does not make.
#
# The number is supposed to come from ADR-001, which records none for Qdrant: §6.1 cancelled the
# headroom measurement because it was budgeting for an embedder that had left the machine, and §8
# says NFR-6's "Qdrant + embedder ≤ 3 GB" describes a machine that no longer exists. A threshold
# invented here would be a gate passing against a number nobody measured — which is worse than this
# line, because this line is visible.
printf 'NOT CHECKED: the container memory limit. ADR-001 records no figure for Qdrant (§6.1, §8);\n'
printf '             measure MemAvailable and Qdrant RSS on the deployed host, record it there, and\n'
printf '             the limit and its check land in this script together.\n'

if [ "$failures" -gt 0 ]; then
  printf '\n%s: %d check(s) failed\n' "$compose" "$failures" >&2
  exit 1
fi
printf '%s: image pin, bind addresses and API key all check out\n' "$compose"
