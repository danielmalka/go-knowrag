.PHONY: test lint build test-integration cover

test:
	go test ./...

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
test-integration:
	go test -tags integration -timeout 60m ./...

lint:
	golangci-lint run ./...

build:
	go build -o bin/mcp-server ./cmd/mcp-server
	go build -o bin/cli ./cmd/cli
