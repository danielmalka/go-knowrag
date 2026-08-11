package archtest

import (
	"os"
	"path/filepath"
	"testing"
)

// qdrantClientImport is the package the invariant is about. Spelled once, used by both the real
// run and the fixture run, so the two can never drift apart and quietly stop testing the same thing.
const qdrantClientImport = "github.com/qdrant/go-client/qdrant"

// storeDir is the one place allowed to import it.
const storeDir = "internal/store"

// TestArch_QdrantClientConfinedToStore is invariant 1 of S09 T11: nothing outside internal/store
// talks to the Qdrant client.
//
// It is a boundary, not a style rule. Every Qdrant concern this project has — connection,
// credentials, request shapes, the wire enums — is meant to be reachable from exactly one package,
// so that a change to any of them has one place to happen and one place to test. A second package
// that imports the client is not a small convenience; it is a second definition of how this system
// talks to its database.
//
// The rule has already shaped one design and been paid for once: S06b's ingestion lock started as
// points in a Qdrant collection, which would have forced an exception here, and was respecified as
// a local flock (ADR-005) so that it would not. It is built — internal/ingest/lock reaches the
// kernel and never the database, which is why this test still passes with a lock in the tree.
// No exception exists, and none should be added.
func TestArch_QdrantClientConfinedToStore(t *testing.T) {
	root := moduleRoot(t)

	violations, err := FindImporters(root, qdrantClientImport, []string{storeDir})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	for _, v := range violations {
		t.Errorf(
			"%s imports %q, but only %s may talk to the Qdrant client — express what you need in "+
				"the caller's own types and put the translation behind a function in %s "+
				"(internal/schema.CollectionManifest is the worked example)",
			v, qdrantClientImport, storeDir, storeDir)
	}
}

// TestArch_QdrantCheckerIsNotVacuous proves the test above can fail. A walker with a wrong root, a
// misspelled import path or an over-broad skip list passes silently and forever, which is the
// failure mode a green architecture test is most likely to have.
//
// The fixture lives under testdata/ so the Go tool never builds it and the real walk never sees it;
// it is reached only by pointing the checker straight at it.
func TestArch_QdrantCheckerIsNotVacuous(t *testing.T) {
	fixture := filepath.Join("testdata", "violation")

	violations, err := FindImporters(fixture, qdrantClientImport, nil)
	if err != nil {
		t.Fatalf("walking %s: %v", fixture, err)
	}
	if len(violations) == 0 {
		t.Fatalf("the checker found no violation in %s, which is a file that imports %q — "+
			"TestArch_QdrantClientConfinedToStore is passing because it looks at nothing",
			fixture, qdrantClientImport)
	}
}

// moduleRoot returns the repository root and refuses to guess: a wrong root is exactly how this
// test would go vacuous, so the go.mod check is part of the assertion, not a convenience.
func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving the module root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("no go.mod at %s — the walk root is wrong, so this test proves nothing: %v", root, err)
	}
	return root
}
