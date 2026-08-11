module github.com/danielmalka/go-knowrag

go 1.26.0

require (
	github.com/google/uuid v1.6.0
	github.com/modelcontextprotocol/go-sdk v1.7.0
	// Qdrant server minimum: v1.16 (introduces ACORN, the filtered-search strategy that stops a
	// selective filter from silently degrading vector-search recall — critical here because every
	// query in this system carries a mandatory tenant_id filter, so the filtered path is this
	// system's normal path, not an edge case; PRD-contrato §2.3b). Deployed server tracks v1.19.x,
	// the current stable release — this is a fresh install with no legacy instance to migrate, so
	// there is no reason to deploy anything older. Migration context: v1.17 replaced RocksDB with
	// Gridstore and breaks direct upgrade from versions older than v1.15; v1.18 removed the legacy
	// search/recommend/discover endpoints outright, so this codebase only ever speaks the Query API
	// (PrefetchQuery + RRF fusion) — already the fixed plan (PRD-contrato §2.3b), now doubly so.
	//
	// Client and server are re-pinned AS A PAIR: bumping this module's minor without moving the
	// deployed server (or the reverse) lets the two drift. See the dependency-update policy in
	// docs/tasks/S01-go-skeleton-and-ci.md T2.
	github.com/qdrant/go-client v1.19.0
	github.com/spf13/cobra v1.10.2
	gopkg.in/yaml.v3 v3.0.1
)

require (
	golang.org/x/sys v0.45.0
	google.golang.org/grpc v1.82.1
)

require (
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/oauth2 v0.36.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260427160629-7cedc36a6bc4 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)
