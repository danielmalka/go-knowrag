.PHONY: test lint build test-integration test-embedder cover

# Interpreter of the virtualenv that has FlagEmbedding installed — the same variable
# knowrag-embedder.env already defines for the systemd unit, so an installed machine exports it
# once and both agree.
KNOWRAG_PYTHON ?= python3

# -race is on here and not only in a developer's habit, because this is the target CI runs. Every
# concurrency defect this repo has shipped so far was found by a person choosing to type `-race`;
# the detector is worth nothing on the runs nobody remembers to ask for. It costs a few seconds on
# a suite that finishes in twelve.
#
# What it does not cover is worth naming next to it: the detector sees shared memory inside one
# process. Two `knowrag ingest` runs against the same scope are two processes and one Qdrant, and
# this flag will stay green through all of it (D-31).
test:
	go test -race ./...

# The coverage number S07's acceptance criterion names (≥80% for internal/retrieval, error paths
# included). It runs the same hermetic suite as `test` above and then prints the per-function
# breakdown, because a package total can clear 80% with every error branch untested — reading the
# function list is what makes that visible.
cover:
	go test -coverprofile=cover.out ./...
	go tool cover -func=cover.out

# Tests behind the `integration` build tag need a live Qdrant, a real embedder and the real vault
# corpus, so `make test` above cannot run them — and excluding them is only safe because this
# target exists to run them somewhere. Trigger: the private runner, before every release, per the
# runbook (PRD-stories-fundacao.md §3 S01). Gate: blocking — a red run blocks the release.
#
# The timeout has to sit above NFR-4's own gate, and `go test`'s default 10 min does not: a full
# ingestion is a 30-minute contract that measured 7m52s, so the default kills the run at 10 min and
# reports a panic instead of a verdict. The margin exists so the gate can fail as an assertion —
# "took 31m, want ≤30m" — which is the only form of that failure anyone can act on.
#
# The budget is shared, which is why it is 60 and not 35: every test in a package runs in one
# process, and NFR-5 spends an unbounded first run converging the tenant before it measures anything.
# Run `make test-integration` right after bulk-editing the vault and that convergence is a full
# ingestion of its own, stacked on top of however long NFR-4 took to overrun.
#
# -p 1 because these packages are not independent: they share one deployed Qdrant and one
# collection, and one of them writes. NFR-4 ingests the whole corpus under its own throwaway
# tenant while internal/retrieval reads points back out of the same collection, and with packages
# running in parallel the reader saw the writer's points mid-run. That went red accusing the search
# of leaking across tenants — the most serious invariant in the system — while the search was
# correct throughout. A red pointing at the wrong place costs more than a slow suite.
#
# The assertion that made it that specific kind of wrong was also fixed (it identified points by
# uid, which is not unique across tenants). This flag is the other half: it removes the overlap
# instead of relying on every future read-back to be written defensively. It costs about a minute
# on a suite that already takes nine, because NFR-4 dominates either way. D-30.
test-integration:
	go test -tags integration -timeout 60m -p 1 ./...

# D-24's test_server.py. Like test-integration above it needs equipment the CI runner does not
# have — `server.py` imports torch and FlagEmbedding at module scope, so importing it at all needs
# the embedder virtualenv — and for the same reason it is a separate target that `test` does not
# call, rather than something that turns CI red on a plain ubuntu runner. No test here loads the
# model or touches the GPU; the venv is needed only to satisfy those two module-scope imports.
# Trigger: the private runner, and any change to `to_sparse` or to the response format.
#
# `cd` because the test imports `server` as a sibling module, and unittest only puts the directory
# it is invoked from on sys.path.
test-embedder:
	cd scripts/embedder-service && $(KNOWRAG_PYTHON) -m unittest -v test_server

lint:
	golangci-lint run ./...

build:
	go build -o bin/mcp-server ./cmd/mcp-server
	go build -o bin/cli ./cmd/cli
