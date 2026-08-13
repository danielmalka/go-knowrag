package archtest

import (
	"os"
	"path/filepath"
	"strings"
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

// modulePath is this module's import prefix. A wrong value here does not weaken the check quietly:
// ModuleImports would record no internal edge at all, and the non-vacuity assertion below — which
// requires internal/eval to be caught — fails on an empty graph.
const modulePath = "github.com/danielmalka/go-knowrag"

// searchRoots are the two packages that *are* the index: internal/retrieval builds and runs the
// query, internal/store holds the Qdrant client. Nothing else in this module can produce a search
// result without going through one of them.
//
// This list is two entries and not three, and the missing third is the point. An earlier version of
// this test also named internal/eval, because internal/eval can search — it holds LoadCorpus,
// NewCorpusSearcher and RunGolden (internal/eval/corpus.go, internal/eval/runner.go), which answer
// with real hits over a local corpus file and never touch Qdrant. Naming it worked and was wrong in
// a way this project keeps paying for: it made the guard a hand-written roster, and deleting one line
// from a roster disables it with nothing going red. internal/eval is now caught because it *reaches*
// internal/retrieval, which is a fact about the import graph rather than an entry someone remembered
// to add — and any future package that gains a way to search is caught the same day, by the same fact.
//
// What keeps these two honest is that they are irreducible: they are where the query and the client
// live, so a deletion here is not a narrowed list, it is a claim that the index no longer searches.
// The non-vacuity assertion below turns emptying this list into a failure.
var searchRoots = []string{
	modulePath + "/internal/retrieval",
	modulePath + "/internal/store",
}

// authoringPackage is the session, and it is the only package this test has to name. Everything it
// reaches — internal/goldenset, internal/config, internal/schema, internal/vault today — is derived
// from the import graph, so no directory can drop out of the check by being forgotten in a list.
const authoringPackage = modulePath + "/internal/goldenauthor"

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
// CLAUDE.md says to do when a plant will not go red, and left this: an import walk, no list of
// symbols.
//
// internal/eval is caught by this test, and it is the reason the golden-set file schema was split out
// of it. The session has to read and append to the golden-set file, so it has to import the schema;
// while the schema and the gate were one package, importing the first brought eval.LoadCorpus,
// eval.NewCorpusSearcher and eval.RunGolden, which compile without Qdrant and return real hits over a
// local corpus file. That was the last of the five bypasses and the only one left open on record.
// internal/goldenset now holds the schema and reaches no searcher.
//
// The check is over the transitive closure and not over a list of directories, and that is the second
// thing this test learned the hard way. A version of it walked a hand-written map of packages: two
// separate reviews found that deleting one line from that map turned the whole invariant off with the
// entire suite still green, and that the closure it did not walk had two live holes in it —
// internal/config or internal/vault gaining an import of internal/retrieval both compiled and both
// passed. Deriving the reachable set from the source on every run is what makes those unwritable
// rather than merely tested for: there is no list to shorten and no directory to forget.
//
// One residual, stated at the severity it has. internal/schema cannot import internal/retrieval or
// internal/store at all — both already depend on internal/schema, so the edge is an import cycle and
// the compiler refuses it. That is topology, not this test, and it is worth knowing precisely because
// it would otherwise look like a case this test proved. This test would catch it too; it just never
// gets the chance.
//
// The ceiling, so nobody re-derives it: this is red, not impossible. Go has no intra-module import
// restriction — `internal/` gates by module, not by importer, and a build tag cannot select on who is
// importing. A separate module would enforce it, and is not worth it here: it would drag internal/config
// and internal/vault along, which nearly everything imports, and buy a second go.mod and a second test
// invocation. The internal/schema case above is that enforcement happening for free, by an accident of
// layout rather than by design.
func TestArch_GoldenAuthoringCannotReachTheIndex(t *testing.T) {
	graph, err := ModuleImports(moduleRoot(t), modulePath)
	if err != nil {
		t.Fatalf("building the module import graph: %v", err)
	}

	for _, root := range searchRoots {
		if chain := PathTo(graph, authoringPackage, root); chain != nil {
			t.Errorf("the authoring session reaches %s:\n\t%s\nIt asks for a question about a note, "+
				"and a question written after seeing what a search returns is a question tuned until "+
				"it passes — so it must have no way to obtain one. Remove the first edge of that chain.",
				root, strings.Join(chain, "\n\t  -> "))
		}
	}

	// Non-vacuity, and it is about this predicate rather than about the graph builder in general. A
	// wrong modulePath, a broken walk or an emptied searchRoots all make the loop above pass over
	// nothing and go on passing forever. internal/eval is the package that does search — it is the
	// gate — so the same question asked about it has to come back yes. If it does not, the check above
	// proved nothing about anything.
	//
	// It is deliberately not anchored on another hand-written list: the fixture is a real package whose
	// job is searching, so it cannot stop reaching a searcher without ceasing to be the gate.
	const gate = modulePath + "/internal/eval"
	caught := false
	for _, root := range searchRoots {
		if PathTo(graph, gate, root) != nil {
			caught = true
		}
	}
	if !caught {
		t.Fatalf("%s reaches none of %v, but it is the package that runs the searches — so the walk "+
			"above is looking at nothing (graph has %d package(s))", gate, searchRoots, len(graph))
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
