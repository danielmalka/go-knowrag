package vault

import (
	"strings"

	"github.com/danielmalka/go-knowrag/internal/schema"
)

// deriveArea derives `area` from the first-level folder and `sub` from the second, per
// PRD-contrato §2.4b. `area` never comes from the frontmatter — the vault's linter strips any
// hand-written one, so there is no override path to implement.
//
// The lookup goes through schema.ParseArea rather than a local map, which is what keeps the area
// map single-sourced: `area` is scoped per vault (the same folder name can be meaningful in one
// vault and undefined in the other), and that scoping already lives in internal/schema.
//
// deriveArea holds no exclusion logic. It only ever sees paths walkVault's exclusion let through,
// and that separation is the point: exclusion narrows what reaches this function, it does not
// soften what it does then. An unknown first-level folder that is not excluded is still an error —
// a silent empty `area` would misfile the note where nobody finds it.
func deriveArea(v schema.Vault, relPath string) (schema.Area, string, error) {
	segments := strings.Split(relPath, "/")
	if len(segments) < 2 {
		// No first-level folder at all. Defaulting to a sentinel area would buy a second,
		// ambiguous home for notes at the price of an `area` meaning "nowhere" (S02 T3, owner
		// decision 2026-08-08).
		return schema.Area{}, "", &UnknownAreaError{Path: relPath, Folder: ""}
	}

	folder := segments[0]
	area, ok := schema.ParseArea(v, strings.ToLower(folder))
	if !ok {
		return schema.Area{}, "", &UnknownAreaError{Path: relPath, Folder: folder}
	}

	sub := ""
	if len(segments) > 2 {
		sub = segments[1]
	}
	return area, sub, nil
}
