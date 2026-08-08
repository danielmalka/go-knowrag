//go:build stackpins

// This file exists only to keep the Qdrant client (PRD-contrato §2.3b) in go.mod until S05 imports
// it for real: `go mod tidy` reads files behind build tags, so the pin survives, while the tag —
// which nothing ever sets — keeps the import out of every binary.
//
// It lives in internal/store rather than a shared pins package because S09's architecture test
// asserts that nothing outside internal/store imports the Qdrant client, and that test parses
// sources with go/parser, which does not honour build tags. A pin anywhere else would register as
// a real violation of an invariant it never actually breaks.
//
// Delete this file when S05 lands the first real Qdrant import in this package.
package store

import _ "github.com/qdrant/go-client/qdrant"
