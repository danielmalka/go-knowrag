package vault

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// FrontmatterError reports a note whose frontmatter does not satisfy PRD-contrato §2.4. It always
// names both the file and the offending field: "invalid frontmatter" without a field sends the
// operator reading 700 files, and a field without a path sends them reading all of them.
type FrontmatterError struct {
	Path  string
	Field string
	Err   error
}

func (e *FrontmatterError) Error() string {
	return fmt.Sprintf("%s: frontmatter field %q: %v", e.Path, e.Field, e.Err)
}

func (e *FrontmatterError) Unwrap() error { return e.Err }

// UnknownAreaError reports a note whose first-level folder is not in the vault's area map
// (PRD-contrato §2.4b). Folder is empty when the note sits directly at the vault root and there is
// no first-level folder to derive from — a distinct enough case that it gets its own message.
type UnknownAreaError struct {
	Path   string
	Folder string
}

func (e *UnknownAreaError) Error() string {
	if e.Folder == "" {
		return fmt.Sprintf(
			"%s: note sits at the vault root, with no first-level folder to derive `area` from; "+
				"move it under an area folder, or add it to the vault's excluded root files", e.Path)
	}
	return fmt.Sprintf(
		"%s: first-level folder %q is not a known `area` for this vault, and is not excluded",
		e.Path, e.Folder)
}

// SymlinkError reports a symbolic link inside the vault tree. Both kinds are refused, for two
// different reasons that one rule closes together:
//
//   - A link to a file escapes the vault root, which is this package's only trust boundary.
//     filepath.WalkDir does not *follow* links, but it does report a link to a file as an ordinary
//     non-directory entry, so a `note.md -> /home/user/.ssh/config` passes the extension check and
//     the subsequent os.ReadFile resolves it. Anything with parseable frontmatter then gets
//     embedded and becomes searchable over MCP. Anyone able to write into the vault folder can
//     plant one, and on `/mnt/c` that folder is shared with Windows.
//   - A link to a directory is the mirror image: WalkDir will not descend it, so every note under
//     it is missing from the index with no signal at all — the silent-omission failure the
//     contract rejects everywhere else.
//
// Refusing follows PRD-contrato §2.4b's rule rather than inventing a new one: excluded is a
// declared decision, unknown is an error. A symlink is not a declared part of the vault contract,
// so it is named and refused instead of being silently skipped (which would reintroduce the second
// failure above) or silently followed (the first). Neither real vault contained one when this was
// written, so the strict reading costs nothing today; the day it does, the answer is a contract
// change, not a silent read outside the root.
type SymlinkError struct {
	Path   string
	Target string // link target as recorded on disk; "" when it could not be read
}

func (e *SymlinkError) Error() string {
	if e.Target == "" {
		return fmt.Sprintf(
			"%s: symbolic links are not part of the vault contract; replace it with the real file "+
				"or exclude its folder", e.Path)
	}
	return fmt.Sprintf(
		"%s: symbolic link to %q — links are not part of the vault contract; replace it with the "+
			"real file or exclude its folder", e.Path, e.Target)
}

// DuplicateUIDError reports two notes sharing one uid. This is fatal rather than a warning because
// the point ID is uuid5(tenant_id + uid + chunk_index): two notes with one uid produce one set of
// point IDs, and the second upsert silently overwrites the first note's chunks.
type DuplicateUIDError struct {
	UID   uuid.UUID
	PathA string
	PathB string
}

func (e *DuplicateUIDError) Error() string {
	return fmt.Sprintf("duplicate uid %s in %q and %q", e.UID, e.PathA, e.PathB)
}

// ScanErrors aggregates every per-note failure of one scan.
//
// Collect-all rather than fail-fast, decided in S02 T8 (which left the choice open): a scan is a
// batch over ~731 files, and failing on the first bad note turns fixing a vault into one
// run-fix-run cycle per bad note. It is still fail-closed — ScanVault returns no Notes alongside
// this error, so no caller can act on a partial result.
//
// Unwrap returns the wrapped slice so errors.As/errors.Is reach the concrete *FrontmatterError,
// *UnknownAreaError and *DuplicateUIDError inside, and callers keep matching on those types.
type ScanErrors struct {
	Path string // the vault root that was scanned
	Errs []error
}

func (e *ScanErrors) Error() string {
	lines := make([]string, 0, len(e.Errs)+1)
	lines = append(lines, fmt.Sprintf("scanning vault %s: %d note(s) failed:", e.Path, len(e.Errs)))
	for _, err := range e.Errs {
		lines = append(lines, "  - "+err.Error())
	}
	return strings.Join(lines, "\n")
}

func (e *ScanErrors) Unwrap() []error { return e.Errs }
