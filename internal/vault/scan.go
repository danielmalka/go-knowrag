package vault

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

// ScanVault turns a vault directory into validated Notes: discovery with exclusion, LF
// normalization and frontmatter parsing, area/sub derivation, one batched `updated` derivation,
// and duplicate-uid detection.
//
// name and areas are this vault's identity and its valid `area` set, both installation
// configuration rather than contract (D-26). They are separate parameters, and areas is not folded
// into Exclusions, because areas is not an exclusion: it decides what is accepted, not what is
// skipped. See deriveArea for why an empty areas rejects every note.
//
// It holds no default exclusion list — ex comes from the per-vault config, so re-including an
// excluded folder is a config edit rather than a code change. It takes no pipeline config and
// computes no point_hash: S02 delivers Notes and leaves NoteMetadataFields for S06a to call once
// chunk text exists.
//
// On failure it returns the zero ScanResult, never a partial one. Every per-note failure is
// collected and reported together in a *ScanErrors — see that type for why collecting beats
// failing on the first bad note here.
func ScanVault(root, name string, areas []string, ex Exclusions) (ScanResult, error) {
	paths, violations, err := walkVault(root, ex)
	if err != nil {
		return ScanResult{}, fmt.Errorf("walking vault %s: %w", root, err)
	}

	gitUpdated, err := gitUpdatedMap(root)
	if err != nil {
		return ScanResult{}, fmt.Errorf("deriving `updated` for vault %s: %w", root, err)
	}

	result := ScanResult{Vault: name, Notes: make([]Note, 0, len(paths))}
	seenUID := make(map[uuid.UUID]string, len(paths))
	// Walk-level violations seed the same list the per-note failures land in, so one run reports
	// every symlink together with every bad frontmatter instead of making the operator fix one
	// category, re-run, and discover the next.
	failures := violations

	for _, rel := range paths {
		// #nosec G304 G703 -- rel comes from walking the operator-configured vault root, not from a
		// request, and walkVault refuses every symlink it encounters rather than returning it, so
		// this join cannot resolve to a file outside that root. See SymlinkError for why the walk
		// has to refuse them explicitly instead of relying on WalkDir not following links.
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			failures = append(failures, fmt.Errorf("reading %s: %w", rel, err))
			continue
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			result.Skipped = append(result.Skipped, SkippedNote{Path: rel, Reason: reasonEmptyFile})
			continue
		}

		note, err := parseFrontmatter(rel, raw)
		if err != nil {
			failures = append(failures, err)
			continue
		}

		note.Vault = name
		if note.Area, note.Sub, err = deriveArea(areas, rel); err != nil {
			failures = append(failures, err)
			continue
		}
		note.Updated = resolveUpdated(root, rel, gitUpdated)

		if first, dup := seenUID[note.UID]; dup {
			failures = append(failures,
				&DuplicateUIDError{UID: note.UID, PathA: first, PathB: rel})
			continue
		}
		seenUID[note.UID] = rel

		result.Notes = append(result.Notes, note)
	}

	if len(failures) > 0 {
		return ScanResult{}, &ScanErrors{Path: root, Errs: failures}
	}
	return result, nil
}

// CheckCrossVaultDuplicateUIDs reports a uid that appears in both scans.
//
// It is separate from ScanVault, and takes two finished ScanResults rather than two roots, because
// one ScanVault call sees one vault. The check has to exist at all because the point ID is
// uuid5(tenant_id + uid + chunk_index) and does not include `vault`: a uid repeated across two
// vaults collides in Qdrant exactly like a uid repeated inside one of them, and the second upsert
// overwrites the first in silence.
//
// S06a is the one caller that holds both results, and must call this before acting on either.
func CheckCrossVaultDuplicateUIDs(a, b ScanResult) error {
	byUID := make(map[uuid.UUID]string, len(a.Notes))
	for _, n := range a.Notes {
		byUID[n.UID] = n.Path
	}
	for _, n := range b.Notes {
		if path, dup := byUID[n.UID]; dup {
			return &DuplicateUIDError{UID: n.UID, PathA: path, PathB: n.Path}
		}
	}
	return nil
}
