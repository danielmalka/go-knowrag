#!/usr/bin/env bash
#
# Refuses to let private material reach a tracked file in this public repository.
#
# It exists because the rule alone did not hold. CLAUDE.md has forbidden writing vault names, disk
# paths, people and infrastructure into tracked files since the first day, and on 2026-08-13 a real
# vault path — company name included — reached internal/config/config.go as a doc-comment example and
# survived a review, a merge and a CI run. Every other invariant in this project that cost something
# got a check; this one had a sentence.
#
# TWO HALVES, AND ONLY ONE CAN LIVE HERE.
#
# The structural half needs no secrets: a disk path, an IPv4 address and an e-mail have shapes. It
# runs everywhere, CI included.
#
# The names half cannot be written down here — a list of "never write these words" in a public file
# publishes the words. So the terms live in an untracked file the operator fills in, and this script
# says out loud when it did not find one rather than passing in silence. Same arrangement as
# verify-deploy.sh's key comparison: the check that needs private values runs where they are.
#
# The consequence is stated rather than hidden: CI cannot catch a leaked name. The operator's machine
# can, and `make check-leaks` is where it does.
set -uo pipefail

terms_file="${KNOWRAG_LEAK_TERMS:-.leakterms}"
failures=0

# Only tracked files, because that is the whole question — untracked material is not published.
# Excluded: this script, which must name the shapes it forbids in order to forbid them, and testdata
# fixtures, whose vocabulary is deliberately fake and reviewed as such.
# CLAUDE.md is excluded for the reason verify-deploy.sh strips comments before checking: it has to
# name the shapes in order to forbid them, and a checker that fails on the statement of its own rule
# is a checker somebody deletes. That file is the rule; this script is the enforcement.
files=$(git ls-files | grep -vE '^scripts/check-leaks\.sh$|^CLAUDE\.md$|testdata/')

report() {
  printf 'LEAK: %s\n' "$1" >&2
  failures=$((failures + 1))
}

# 1. Shapes. Each pattern is a thing that cannot appear in this repository for a legitimate reason.
#
# The e-mail pattern deliberately does not match a bare `@`: `Recall@5` is a metric this project
# names constantly, and a check that cries wolf on it is a check somebody turns off.
scan() {
  local label=$1 pattern=$2 allowed=${3:-}
  local hits
  hits=$(grep -nIE "$pattern" $files 2>/dev/null) || true
  [ -n "$allowed" ] && hits=$(printf '%s\n' "$hits" | grep -vE "$allowed") || true
  hits=$(printf '%s\n' "$hits" | grep -v '^$' | head -5)
  [ -z "$hits" ] && return
  report "$label"
  printf '%s\n' "$hits" | sed 's/^/       /' >&2
}

# `user`, `someone`, `you` and an angle-bracket placeholder are what a fabricated path looks like;
# excluding them keeps the check pointed at paths that name a real account.
placeholder='(/home/(user|someone|you|<)|/Users/(user|someone|you|<))'

scan "a home directory path"  '/home/[a-zA-Z<]'                  "$placeholder"
scan "a Windows mount path"   '/mnt/[a-z]/(Users|Documents)/[a-zA-Z<]' '/(Users|Documents)/<'
scan "a macOS user path"      '/Users/[a-zA-Z<]'                  "$placeholder"
# Addresses and e-mails reserved for documentation and testing are not leaks, and excluding them is
# not leniency: a check that fires on `203.0.113.5` — the address RFC 5737 exists so examples can use
# — teaches the reader that its output is noise, and the next real hit scrolls by with the rest.
#
#   127.x, 0.0.0.0, ::1        loopback and wildcard, which this project's tests use on purpose
#   192.0.2 / 198.51.100 / 203.0.113   RFC 5737, reserved for documentation
#   example.com/.org/.net/.invalid     RFC 2606, reserved for the same reason
reserved_ip='(^|[^0-9.])(127\.|0\.0\.0\.0|192\.0\.2\.|198\.51\.100\.|203\.0\.113\.)'
reserved_mail='@(example\.(com|org|net)|.*\.invalid|localhost)'

scan "an IPv4 address"   '\b(([0-9]{1,3}\.){3}[0-9]{1,3})\b'   "$reserved_ip"
scan "an e-mail address" '[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}' "$reserved_mail"

# 2. Names. Absent the file, this says so — the one outcome that must never look like a pass.
if [ ! -f "$terms_file" ]; then
  printf 'NOT CHECKED: private names. %s does not exist, so this run only looked at shapes.\n' "$terms_file"
  printf '             Create it with one term per line — vault names, folder names, people,\n'
  printf '             hostnames — and it is gitignored. CI never has it, and that is expected:\n'
  printf '             a list of forbidden words in a public repository publishes the words.\n'
else
  while IFS= read -r term; do
    term=$(printf '%s' "$term" | sed 's/[[:space:]]*$//')
    case "$term" in ''|'#'*) continue ;; esac
    hits=$(grep -nIiF -- "$term" $files 2>/dev/null | head -5) || true
    [ -z "$hits" ] && continue
    report "a term from $terms_file"
    printf '%s\n' "$hits" | sed 's/^/       /' >&2
  done < "$terms_file"
fi

if [ "$failures" -gt 0 ]; then
  printf '\n%d leak(s) in tracked files. This repository is public.\n' "$failures" >&2
  exit 1
fi
printf 'no leak found in %d tracked file(s)\n' "$(printf '%s\n' $files | wc -l)"
