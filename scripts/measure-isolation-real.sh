#!/usr/bin/env bash
#
# The two-minute T6b check: is `cli eval --isolation` capable of running against a real deployed
# Qdrant today, or does S12 T6b (docs/tasks/S12-gates-and-runbook.md) still need code before it can?
#
# It does not attempt to run anything against real infrastructure — this repository's convention
# (CLAUDE.md, and the task this script was written for) is that only the operator runs commands
# against their own deployment. What this script does is answer the code question definitively, by
# grepping the three lines that decide it, so nobody has to re-derive the answer by reading three
# files every time T6b comes up.
#
# The evidence, as of the commit this script ships in:
#
#   1. internal/eval/eval.go: IsolationGate's signature is `func(ctx, _ Options)` — the Options
#      parameter (which carries Searcher, Collection, TenantID for --golden) is discarded, unused,
#      for --isolation. There is no way to hand it a real Qdrant connection through cmd/cli/eval.go's
#      existing plumbing.
#   2. internal/eval/isolation/probe.go: `newProbe` is a package-level var, always building the same
#      hostile in-memory `probeStore` (a small fixture, not a real Qdrant client) wrapped by a
#      `FakeEmbedder`. Nothing reads an endpoint, a credential or a collection name from anywhere.
#   3. cmd/cli/eval.go's own comment on the --isolation branch says as much on purpose: "an
#      `eval --isolation` that dialled Qdrant would be a hermetic job acquiring a dependency by
#      accident" — the hermetic property is deliberate, not an oversight to patch around.
#
# So the verdict is not "just point it at the right config" — the flag exists
# (`cli eval --isolation`) but the mode it selects has no code path to a real backend at all. T6b
# (S12-gates-and-runbook.md) needs new code: a way to run the same adversarial cases
# (internal/eval/isolation/cases.go) against a real *retrieval.Searcher backed by a real Qdrant
# client and a real embedder, with the synthetic fixture loaded into the real deployed `clientes`
# collection first. That is real infrastructure work this script's own task explicitly excludes
# ("não execute nada contra infraestrutura real") and it is S12 T6b's own scope, not a side effect of
# a measurement harness.
#
# If a future change adds that wiring, the three greps below stop matching and this script says so
# instead of reporting the stale verdict.
set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
eval_go="$repo_root/internal/eval/eval.go"
probe_go="$repo_root/internal/eval/isolation/probe.go"

still_hermetic=true

if ! grep -qE 'func IsolationGate\(ctx context\.Context, _ Options\)' "$eval_go"; then
  still_hermetic=false
fi
if ! grep -qE 'var newProbe = func\(\) \(caseSearcher, \*probeStore\)' "$probe_go"; then
  still_hermetic=false
fi
if ! grep -qE 'embed\.FakeEmbedder\{\}' "$probe_go"; then
  still_hermetic=false
fi

if [ "$still_hermetic" = true ]; then
  cat <<'EOF'
T6b verdict: BLOCKED — needs new code, not just a different invocation.

`cli eval --isolation` (cmd/cli/eval.go) runs internal/eval.IsolationGate, which ignores its Options
argument and always builds internal/eval/isolation's hardcoded hostile in-memory store and
FakeEmbedder (probe.go: newProbe). There is no flag, env var or config field anywhere in that path
that names a real Qdrant endpoint, a real credential or a real collection — the hermetic property is
intentional (cmd/cli/eval.go's own comment on the --isolation branch says exactly this).

Evidence (grep the lines yourself; this script checked them above):
  internal/eval/eval.go:192        func IsolationGate(ctx context.Context, _ Options)
  internal/eval/isolation/probe.go:212  var newProbe = func() (caseSearcher, *probeStore)
  internal/eval/isolation/probe.go:221  retrieval.NewSearcher(embed.FakeEmbedder{}, store, ...)
  cmd/cli/eval.go:80               "an `eval --isolation` that dialled Qdrant would be..."

What T6b (docs/tasks/S12-gates-and-runbook.md) needs, and this script's own task deliberately does
not build: a real-backed variant of internal/eval/isolation's suite (real *retrieval.Searcher over a
real Qdrant client and a real embedder, S11's fixture loaded into the real deployed `clientes`
collection with the administrative CLI credential), wired to a new `cli eval --isolation --real` (or
equivalent) flag. That is infrastructure-touching, real-deployment work — out of scope here, in scope
for whoever picks up T6b.

Exit status: 1 (blocked).
EOF
  exit 1
fi

cat <<'EOF'
T6b verdict: the three lines this script has always checked no longer match — the isolation suite
may now have a real-deployment code path. Do not trust the "BLOCKED" text above; re-read
internal/eval/eval.go's IsolationGate and internal/eval/isolation/probe.go's newProbe by hand, and if
a real-backed mode exists, update this script (or replace it with the real invocation) rather than
reporting a stale verdict.

Exit status: 2 (verdict needs re-deriving, not a clean pass or fail).
EOF
exit 2
