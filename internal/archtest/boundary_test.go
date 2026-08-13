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

// searchingPackages are every package in this module from which a search result can be obtained.
//
// The first two are the index itself: internal/retrieval builds and runs the query, internal/store
// holds the Qdrant client. The third is internal/eval, and it is the one that has to be named
// explicitly because it reaches no deployment: it holds the golden gate and, with it, LoadCorpus,
// NewCorpusSearcher and RunGolden (internal/eval/corpus.go, internal/eval/runner.go), which answer
// with real hits over a local corpus file and no Qdrant at all. A result is a result — a question
// written after seeing one is tuned either way — so the list is "anything that searches", not
// "anything that dials the database".
var searchingPackages = []string{
	"github.com/danielmalka/go-knowrag/internal/retrieval",
	"github.com/danielmalka/go-knowrag/internal/store",
	"github.com/danielmalka/go-knowrag/internal/eval",
}

// authoringPackages are the packages the authoring session is made of, each mapped to an import it
// has by construction.
//
// Two entries, not one, because FindImporters reads the direct imports of the files under a single
// directory: a check on internal/goldenauthor alone proves only that the session imports no searcher,
// and internal/goldenset gaining one would hand the route straight back. internal/goldenset exists
// precisely because internal/eval was both schema and gate (see the package doc in
// internal/goldenset/goldenset.go); keeping it clean is what the split bought.
//
// The value is the non-vacuity anchor for that directory's walk, and every walk needs its own. A
// wrong root, a renamed package or a moved file makes a walk pass over nothing and go on passing
// forever, and the anchor is what turns that into a failure: the session reads notes through
// internal/vault, and the schema validates area slugs through internal/config, so neither import can
// go away without the package losing its job.
var authoringPackages = map[string]string{
	"internal/goldenauthor": "github.com/danielmalka/go-knowrag/internal/vault",
	"internal/goldenset":    "github.com/danielmalka/go-knowrag/internal/config",
}

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
// internal/eval is on the forbidden list, and it is the edge this test could not have until the
// golden-set file schema moved out of it. The authoring session has to read and append to that file,
// so it has to import the schema; while the schema and the gate were one package, importing the first
// brought eval.LoadCorpus, eval.NewCorpusSearcher and eval.RunGolden, which compile without Qdrant and
// return real hits over a local corpus file. That was the last of the five bypasses, and it was open
// on record rather than unnoticed. internal/goldenset now holds the schema and imports no searcher, so
// forbidding internal/eval here costs the session nothing — and the ability to forbid it is the proof
// the split did what it was for.
func TestArch_GoldenAuthoringCannotReachTheIndex(t *testing.T) {
	root := moduleRoot(t)

	for authoring, known := range authoringPackages {
		dir := filepath.Join(root, authoring)

		for _, pkg := range searchingPackages {
			violations, err := FindImporters(dir, pkg, nil)
			if err != nil {
				t.Fatalf("walking %s for %s: %v", dir, pkg, err)
			}
			for _, v := range violations {
				t.Errorf("%s/%s imports %q. The authoring session must have no route to a search "+
					"result: it asks for a question about a note, and a question written after seeing "+
					"what a search returns is a question tuned until it passes", authoring, v, pkg)
			}
		}

		// Non-vacuity for this walk, in the same loop so a package added above cannot arrive without
		// one. See authoringPackages for what the anchor is and why every directory needs its own.
		seen, err := FindImporters(dir, known, nil)
		if err != nil {
			t.Fatalf("walking %s for %s: %v", dir, known, err)
		}
		if len(seen) == 0 {
			t.Fatalf("the walk of %s found no file importing %q, which that package needs to do its "+
				"job at all — so the checks above are looking at nothing", dir, known)
		}
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
