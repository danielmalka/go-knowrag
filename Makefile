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
# ingestion is a 30-minute contract that measured 12min54s, so the default kills the run at 10 min
# and reports a panic instead of a verdict. 45 min leaves the gate room to fail as an assertion —
# "took 31m, want ≤30m" — which is the only form of that failure anyone can act on.
test-integration:
	go test -tags integration -timeout 45m ./...

lint:
	golangci-lint run ./...

build:
	go build -o bin/mcp-server ./cmd/mcp-server
	go build -o bin/cli ./cmd/cli
