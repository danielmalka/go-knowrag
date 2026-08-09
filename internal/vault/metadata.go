package vault

import (
	"time"

	"github.com/danielmalka/go-knowrag/internal/schema"
)

// NoteMetadataFields returns the note-level metadata component of S06a's point_hash, exactly the
// eleven fields PRD-contrato §2.4 lists: vault, path, type, status, visibility, tags, title, lang,
// created, area, sub.
//
// It computes no hash. point_hash needs chunk text and breadcrumb (S03), point identity, pipeline
// config, and the handshake-confirmed model revision (S04); S06a is the one place that has all of
// them, and it is the only caller that runs schema.SerializeFields over this map.
//
// `updated` is excluded on purpose (owner decision, 2026-08-08; PRD-contrato §2.4, ADR-004 §5.1) —
// eleven fields, not twelve. It is the single §2.4 payload field kept out of point_hash, because
// it is derived from git/mtime and therefore moves without the content moving: a git checkout, a
// rebase, a restored backup or an editor that rewrites on save would each re-embed a note without
// a letter having changed. Identity of content cannot depend on a clock that walks on its own.
// The same exclusion applies to S06a's condition-4 field comparison, or mtime noise returns
// through the back door.
//
// Anything not in the list — `description`, and S03's per-chunk `oversize`/`headings` — is not this
// function's business. Over-inclusion here silently changes every point_hash in the index.
func NoteMetadataFields(n Note) map[string]string {
	return map[string]string{
		"vault":      n.Vault.String(),
		"path":       n.Path,
		"type":       n.Type.String(),
		"status":     n.Status.String(),
		"visibility": n.Visibility.String(),
		"tags":       schema.SerializeList(n.Tags),
		"title":      n.Title,
		"lang":       n.Lang,
		"created":    n.Created.UTC().Format(time.RFC3339),
		"area":       n.Area.String(),
		"sub":        n.Sub,
	}
}
