package isolation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/danielmalka/go-knowrag/internal/archtest"
)

// The invariant, spelled here because this case is what enforces it at release time rather than at
// `go test` time. Both values are the ones internal/archtest's own test uses; they are literals in
// two places because archtest keeps its copies unexported, and
// TestArchitectureCase_AgreesWithArchtestsOwnInvariant holds the two equal.
const (
	qdrantClientImport = "github.com/qdrant/go-client/qdrant"
	storeDir           = "internal/store"
)

// ArchitectureBoundaryCase fails the isolation suite if anything outside internal/store talks to
// the Qdrant client directly.
//
// It is an isolation case and not only a style rule: every tenant condition this suite checks is
// built in internal/retrieval and transcribed in internal/store, and a package that opened its own
// client would be a second way to reach the database — one no filter of ours passes through. The
// suite would keep proving things about a path that was no longer the only one.
//
// It calls archtest.FindImporters in-process rather than shelling out to `go test`, so
// `cli eval --isolation` enforces it wherever it runs, with no dependency on a separate CI step
// having been invoked. root is the module root to walk; cmd/cli passes the working directory.
func ArchitectureBoundaryCase(root string) Case {
	return Case{Name: "architecture: only internal/store reaches the Qdrant client", Run: func(_ context.Context) string {
		violations, err := archtest.FindImporters(root, qdrantClientImport, []string{storeDir})
		if err != nil {
			// Not reported as a pass. A scan that could not run proved nothing, and the one thing
			// this suite may never do is report an unrun check as a held invariant.
			return fmt.Sprintf("the import scan over %q could not run, so the boundary is unproven: %v", root, err)
		}
		if len(violations) == 0 {
			return ""
		}
		located := make([]string, 0, len(violations))
		for _, v := range violations {
			located = append(located, v.String())
		}
		return fmt.Sprintf("%d file(s) outside %s import %q: %s",
			len(violations), storeDir, qdrantClientImport, strings.Join(located, ", "))
	}}
}

// ModuleRoot walks up from the working directory to the directory holding go.mod.
//
// It searches rather than assuming, because the architecture case scans a tree: run from a
// subdirectory and a guessed root would scan a subtree, find no violation, and report the boundary
// as held having looked at a fraction of the module. Not finding a root is an error and not a
// fallback — see ArchitectureBoundaryCase, which turns it into a failing case rather than a skipped
// one.
//
// The consequence to know about: `cli eval --isolation` checks a build-time invariant, so it has to
// run where the source is. On a deployed host with no source tree it fails, loudly, rather than
// reporting an invariant nobody verified.
func ModuleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolving the working directory: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod at or above the working directory, so the source tree " +
				"the architecture boundary is checked against is not here")
		}
		dir = parent
	}
}
