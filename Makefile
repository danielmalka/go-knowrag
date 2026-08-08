.PHONY: test lint build test-integration

test:
	go test ./...

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
