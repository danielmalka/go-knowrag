package security

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ValidateRelativePath reports whether path is a plain, clean, vault-contained relative path.
//
// The path in a search result is document-controlled: it came from the vault walk and rode through
// the index as payload. Before it goes back out in a response it has to be what it claims to be —
// a location inside the vault — and not `../../etc/passwd` or `/etc/passwd`.
//
// vaultRoot may be empty, and for the MCP server it is: that process never opens a vault file, so
// it has no root to check against. An empty root is treated as ".", which loses nothing — a clean
// relative path with no leading `..` is contained in *every* root, so the structural checks below
// already carry the containment guarantee. A caller that does have a root passes it and gets the
// prefix check on top.
//
// Symlinks are out of scope by construction: this function never touches the filesystem. It answers
// "is this string a safe relative path", which is the question the response assembler has.
func ValidateRelativePath(vaultRoot, path string) error {
	if path == "" {
		return fmt.Errorf("security: empty path")
	}
	if filepath.IsAbs(path) {
		return fmt.Errorf("security: path %q is absolute, expected vault-relative", path)
	}
	if clean := filepath.Clean(path); clean != path {
		return fmt.Errorf("security: path %q is not clean (cleans to %q)", path, clean)
	}
	// Clean leaves leading `..` in place — that is the traversal case, and the only one Clean
	// cannot fold away.
	if path == ".." || strings.HasPrefix(path, ".."+string(filepath.Separator)) {
		return fmt.Errorf("security: path %q escapes the vault", path)
	}

	if vaultRoot == "" {
		vaultRoot = "."
	}
	rel, err := filepath.Rel(vaultRoot, filepath.Join(vaultRoot, path))
	if err != nil {
		return fmt.Errorf("security: path %q is not resolvable against vault root %q: %w", path, vaultRoot, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("security: path %q escapes vault root %q", path, vaultRoot)
	}
	return nil
}
