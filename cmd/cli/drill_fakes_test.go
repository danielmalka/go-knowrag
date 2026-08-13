package main

// The two fakes both drill scripts run against, split out of drill_test.go to keep that file
// under this repository's 500-line limit. They are shell rather than Go on purpose: the scripts
// resolve ssh and knowrag through PATH, so a fake is a file on disk with the right name.
//
// Nothing here is a credential or a real address — this repository is public, and the rule for
// fixtures is the same as for everything else tracked in it.

// fakeSSH stands in for the ssh that reaches the machine running Qdrant. It records every remote
// command it is handed — that log, and not the script's own output, is what the tests assert on:
// what the script *printed* it would do is not evidence of what it did.
//
// It answers the three reads scripts/recovery-drill.sh performs (the container id, the volume name,
// the space) from environment variables, so each test decides what the machine looks like.
const fakeSSH = `#!/usr/bin/env bash
printf '%s\n' "$2" >>"$FAKE_SSH_LOG"
if [ "${FAKE_SSH_FAIL:-0}" = 1 ]; then exit 255; fi
case "$2" in
  *"compose ps -q"*)
    # Counted, because the drill discovers twice — once in the preflight and once inside the
    # destruction — and FAKE_CID_SECOND is how a test makes the container disappear between them.
    c=$(cat "$FAKE_PS_COUNT" 2>/dev/null || printf 0)
    c=$((c + 1)); printf '%s' "$c" >"$FAKE_PS_COUNT"
    if [ "$c" -ge 2 ] && [ -n "${FAKE_CID_SECOND+set}" ]; then printf '%s\n' "$FAKE_CID_SECOND"
    else printf '%s\n' "${FAKE_CID-cid-0001}"; fi ;;
  *"docker inspect"*) printf '%s\n' "${FAKE_VOLUME-qdrant_storage}" ;;
  # ${VAR-default}, never ${VAR:-default}: a test that sets FAKE_DU_KB="" is saying the machine
  # answered with nothing, and the colon form would hand it the default instead — which is how the
  # first version of this fake reported a passing disk check over a size that was never read.
  *"du -sk"*)         printf '%s\t/qdrant/storage\n' "${FAKE_DU_KB-1024}" ;;
  *"df -Pk"*)
    printf 'Filesystem 1024-blocks Used Available Capacity Mounted on\n'
    printf '/dev/vda1 200000 1024 %s 1%% /\n' "${FAKE_DF_KB-100000}" ;;
  *"volume rm"*)      if [ "${FAKE_VOLUME_RM_FAIL:-0}" = 1 ]; then exit 1; fi ;;
  *"compose up -d"*)  if [ "${FAKE_UP_FAIL:-0}" = 1 ]; then exit 1; fi ;;
esac
exit 0
`

// fakeKnowrag stands in for the CLI. `stats` answers FAKE_STATS_BEFORE the first time and
// FAKE_STATS_AFTER every time after that, which is what lets a test say "the index came back
// smaller" without anything real being destroyed.
//
// FAKE_STATS_SEQUENCE=1 adds a third answer, back at FAKE_STATS_BEFORE: scripts/prune-drill.sh
// counts three times — before, after the prune, after the note is restored — and its full cycle
// only closes if the third read is back where the first was.
const fakeKnowrag = `#!/usr/bin/env bash
# signal_leader delivers $1 to the drill's own bash — the process-group leader — and shouts if it
# could not. A fake that silently fails to signal turns the test that asked for it into a green
# no-op, which is the shape this repository has been bitten by.
signal_leader() {
  leader=$(ps -o pgid= -p $$ | tr -d ' ')
  kill -"$1" "$leader" 2>/dev/null || printf 'FAKE COULD NOT SIGNAL THE LEADER\n' >&2
}
printf '%s\n' "$*" >>"$FAKE_CLI_LOG"
case "$1" in
  stats)
    n=$(cat "$FAKE_STATS_COUNT" 2>/dev/null || printf 0)
    n=$((n + 1))
    printf '%s' "$n" >"$FAKE_STATS_COUNT"
    # FAKE_SIGNAL_LEADER_STATS=INT|TERM signals the leader from inside the FIRST count and then
    # exits 0, which is the window a bare SIGINT is discarded in: bash waiting on an ordinary
    # foreground command that succeeds (jobs.c, wait_sigint_handler). It is the same window the
    # drill's own ` + "`mv`" + ` occupies, reached without a clock. FAKE_SIGNAL_LEADER below covers the
    # other one — a signal pending at a command boundary.
    if [ "$n" = 1 ] && [ -n "${FAKE_SIGNAL_LEADER_STATS-}" ]; then
      signal_leader "$FAKE_SIGNAL_LEADER_STATS"
    fi
    if [ "$n" = 1 ]; then printf '%s\n' "$FAKE_STATS_BEFORE"
    elif [ "$n" = 2 ]; then printf '%s\n' "$FAKE_STATS_AFTER"
    elif [ "${FAKE_STATS_SEQUENCE:-0}" = 1 ]; then printf '%s\n' "$FAKE_STATS_BEFORE"
    else printf '%s\n' "$FAKE_STATS_AFTER"; fi
    exit 0 ;;
  schema)
    if [ "${FAKE_SCHEMA_FAIL:-0}" = 1 ]; then exit 1; fi
    # FAKE_SCHEMA_FAIL_TIMES=n refuses the first n attempts and then answers, which is what a Qdrant
    # still coming up looks like to the readiness probe.
    n=$(cat "$FAKE_SCHEMA_COUNT" 2>/dev/null || printf 0)
    n=$((n + 1)); printf '%s' "$n" >"$FAKE_SCHEMA_COUNT"
    if [ "$n" -le "${FAKE_SCHEMA_FAIL_TIMES:-0}" ]; then exit 1; fi
    printf 'interno: ok\n'; exit 0 ;;
  ingest)
    # Slow only for the plain ingest, which is the phase that holds the note out of the vault: it is
    # the window a signal has to land in for TestPruneDrill_RestoresTheNoteOnASignal to mean anything.
    #
    # FAKE_INGEST_STARTED names a file this fake creates immediately before it goes to sleep. It is
    # what makes that test deterministic: once the file exists this process exists too, in the drill's
    # process group, with SIGINT and SIGTERM at their defaults — so a group signal from then on always
    # kills something, and the drill's shell always reaches a command boundary at once. Signalling any
    # earlier is signalling a group whose membership is still being built.
    if [ "${2:-}" != "--dry-run" ] && [ -n "${FAKE_INGEST_SLEEP-}" ]; then
      if [ -n "${FAKE_INGEST_STARTED-}" ]; then : >"$FAKE_INGEST_STARTED"; fi
      sleep "$FAKE_INGEST_SLEEP"
    fi
    if [ "${2:-}" = "--dry-run" ]; then
      if [ "${FAKE_DRYRUN_FAIL:-0}" = 1 ]; then printf 'embedder handshake failed\n' >&2; exit 1; fi
      printf '730 note(s): skipped=730\norphans: none\n'; exit 0
    fi
    case "$*" in
      *--prune*) printf '%s\n' "${FAKE_PRUNE_REPORT-}"; exit "${FAKE_PRUNE_CODE:-0}" ;;
    esac
    if [ "${FAKE_INGEST_FAIL:-0}" = 1 ]; then exit 1; fi
    # FAKE_SIGNAL_LEADER=TERM|INT signals the process-group LEADER — the drill's own bash — and then
    # exits 0. That makes the signal pending while bash is between two commands, which is the window
    # a wall-clock signal from the test hits about one time in twelve. Signalling the whole group
    # instead would kill this fake too, and bash would see a child killed by a signal: the other
    # window, the one that was never broken.
    if [ -n "${FAKE_SIGNAL_LEADER-}" ]; then signal_leader "$FAKE_SIGNAL_LEADER"; fi
    printf '%s\n' "${FAKE_INGEST_REPORT-730 note(s): upserted=730}"; exit 0 ;;
esac
exit 0
`
