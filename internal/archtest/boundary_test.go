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

// indexPackages are the two packages through which anything in this module reaches the index:
// internal/retrieval builds and runs the query, internal/store holds the client.
var indexPackages = []string{
	"github.com/danielmalka/go-knowrag/internal/retrieval",
	"github.com/danielmalka/go-knowrag/internal/store",
}

// authoringPackage writes the golden set that `eval --golden` measures against.
const authoringPackage = "internal/goldenauthor"

// TestArch_GoldenAuthoringCannotReachTheIndex is invariant 2, and it is the whole of what keeps the
// golden set worth measuring.
//
// A question authored after seeing what the index returns is a question adjusted — unconsciously —
// until it passes, and a golden set of those measures the tool that produced it rather than the
// retrieval. So the authoring session must never show a search result, which means it must have no
// way to obtain one.
//
// This invariant is here, as an import edge, because the four attempts to hold it any other way all
// failed. The authoring code used to live in cmd/cli, where every route to Qdrant is a sibling
// declaration in package main and no import is needed to call one, so the rule was defended by tests
// that read the source: a substring scan over identifiers, an allow-list of symbols, a taint pass
// over the package's reference graph, a check on methods of the types the command holds. Five reviews
// found five ways past them — a new package with an innocuous function name and a method declared in
// another file of the package needed no watched name at all — and each fix was narrower than the hole
// it closed. Moving the code into its own package made the defect unrepresentable, which is what
// CLAUDE.md says to do when a plant will not go red, and left this: one walk, no list of symbols.
//
// The residual is written down rather than papered over, and it is demonstrated rather than
// suspected: internal/goldenauthor imports internal/eval for the golden-set file schema, and
// internal/eval also holds the gate, so eval.LoadCorpus + eval.NewCorpusSearcher + eval.RunGolden
// compile from there and return real hits over a local corpus file. This test does not catch that,
// by construction — internal/eval is a legitimate import and an exception list is what this
// invariant exists to avoid having.
//
// What it is not is a route to the deployment's index: no Qdrant, no embedder, and no corpus path
// that command ever holds. Closing it means splitting the file schema out of internal/eval, which is
// its own change — see the package doc in internal/goldenauthor/author.go for the cost.
func TestArch_GoldenAuthoringCannotReachTheIndex(t *testing.T) {
	dir := filepath.Join(moduleRoot(t), authoringPackage)

	for _, pkg := range indexPackages {
		violations, err := FindImporters(dir, pkg, nil)
		if err != nil {
			t.Fatalf("walking %s for %s: %v", dir, pkg, err)
		}
		for _, v := range violations {
			t.Errorf("%s/%s imports %q. The authoring session must have no route to the index: it "+
				"asks for a question about a note, and a question written after seeing what a search "+
				"returns is a question tuned until it passes", authoringPackage, v, pkg)
		}
	}

	// Non-vacuity, and it has to be about this walk rather than about FindImporters in general: a
	// wrong directory, a renamed package or a moved file makes the loop above pass over nothing and go
	// on passing forever. internal/vault is what the session reads notes from, so it is imported here
	// by construction — if the walk cannot see that, it is seeing nothing.
	const known = "github.com/danielmalka/go-knowrag/internal/vault"
	seen, err := FindImporters(dir, known, nil)
	if err != nil {
		t.Fatalf("walking %s for %s: %v", dir, known, err)
	}
	if len(seen) == 0 {
		t.Fatalf("the walk of %s found no file importing %q, which the session needs to read notes "+
			"at all — so the check above is looking at nothing", dir, known)
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
