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
# It exits 0 with zero tests today, which is correct: no story has written one yet. S05 lands the
# first (Qdrant schema against a live instance), S06a the full-corpus ingestion run.
test-integration:
	go test -tags integration ./...

lint:
	golangci-lint run ./...

build:
	go build -o bin/mcp-server ./cmd/mcp-server
	go build -o bin/cli ./cmd/cli
