.PHONY: check-leaks test lint build test-integration test-embedder cover verify-deploy

# Interpreter that runs the embedder service's tests. They need only pytest: the service's own
# modules import torch lazily, so no GPU and no model are involved. An installed machine may point it
# at the service's venv (the variable knowrag-embedder.env already defines), but any Python with
# pytest works.
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
# ingestion is a 30-minute contract that measured 6m50s, so the default kills the run at 10 min and
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

# The embedder service's pytest suite (scripts/embedder-service/test_*.py), run against a fake model.
# Like test-integration above it is a separate target that `test` does not call, so CI is not made
# to depend on a Python toolchain it does not install. The real-GPU tests in the same directory skip
# themselves unless KNOWRAG_GPU_TESTS=1.
# `cd` because the tests import the service's modules as siblings.
test-embedder:
	cd scripts/embedder-service && $(KNOWRAG_PYTHON) -m pytest -q

lint:
	golangci-lint run ./...

# Checks deploy/docker-compose.yml — image pin, bind addresses, mandatory API key — by reading the
# file, so it needs no Docker and no deployed machine. CI runs it beside the linter for that reason:
# the properties it guards are the ones a hurried edit removes without anything else noticing.
verify-deploy:
	./scripts/verify-deploy.sh

# Refuses private material in tracked files. The shape half needs nothing and runs in CI; the names
# half needs .leakterms, which is gitignored and therefore absent there — the script says so out loud
# instead of passing quietly. See its header for why the list cannot live in this repository.
check-leaks:
	./scripts/check-leaks.sh

build:
	go build -o bin/mcp-server ./cmd/mcp-server
	go build -o bin/cli ./cmd/cli
