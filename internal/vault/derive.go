package vault

import (
	"slices"
	"strings"
)

// deriveArea derives `area` from the first-level folder and `sub` from the second, per
// PRD-contrato §2.4b. `area` never comes from the frontmatter — the vault's linter strips any
// hand-written one, so there is no override path to implement.
//
// areas is the valid set of the one vault being scanned, and it comes from that vault's
// configuration (D-26): which areas exist is installation configuration, not contract. Passing it
// per call is what keeps `area` scoped per vault — the same folder name can be meaningful in one
// vault and undefined in another — and it is why the caller must never merge two vaults' sets into
// one flat slice: that silently accepts, in each vault, every folder the other one declares.
//
// An empty set rejects every note. It is not a "nothing configured, so anything goes" shortcut and
// must not become one: config requires AREAS on every rostered vault, so an empty set here means
// the configuration is broken, and the accepting version files a whole corpus under areas nobody
// declared while reporting a clean run.
//
// deriveArea holds no exclusion logic. It only ever sees paths walkVault's exclusion let through,
// and that separation is the point: exclusion narrows what reaches this function, it does not
// soften what it does then. An unknown first-level folder that is not excluded is still an error —
// a silent empty `area` would misfile the note where nobody finds it.
func deriveArea(areas []string, relPath string) (string, string, error) {
	segments := strings.Split(relPath, "/")
	if len(segments) < 2 {
		// No first-level folder at all. Defaulting to a sentinel area would buy a second,
		// ambiguous home for notes at the price of an `area` meaning "nowhere" (S02 T3, owner
		// decision 2026-08-08).
		return "", "", &UnknownAreaError{Path: relPath, Folder: ""}
	}

	folder := segments[0]
	// The folder name is lowercased before the lookup, and the lowercase form is the `area` that
	// reaches the payload and point_hash. That has been true since S02 — a folder named `Research`
	// on disk has always produced area `research` — and it stays true here on purpose: making the
	// match case-sensitive, or storing the folder's on-disk casing, would move the point_hash of
	// every note under a mixed-case folder. That is a reindex, not a refactor.
	area := strings.ToLower(folder)
	if !slices.Contains(areas, area) {
		return "", "", &UnknownAreaError{Path: relPath, Folder: folder}
	}

	sub := ""
	if len(segments) > 2 {
		sub = segments[1]
	}
	return area, sub, nil
}
