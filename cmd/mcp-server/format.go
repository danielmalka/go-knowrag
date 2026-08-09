package main

import (
	"fmt"
	"strings"

	"github.com/danielmalka/go-knowrag/internal/retrieval"
	"github.com/danielmalka/go-knowrag/internal/security"
)

// noResults is what an empty search returns. It is not framed as untrusted content because no
// document contributed a character of it.
const noResults = "No matching chunks were found."

// formatResults renders search results as the text the MCP client receives.
//
// Every field it prints came out of a note, so every field is document-controlled and passes
// through security.Sanitize before it is written — text, breadcrumb, path, uid alike. That is what
// makes the envelope worth having: the delimiters are public constants, so a note that contains
// them verbatim would otherwise close the envelope early and have the rest of the response read as
// system text (PRD-contrato §2.6, S08 T4).
//
// Each result gets its own envelope. Sharing one across results would make a single warning
// ambiguously cover chunks it was not written for.
func formatResults(results []retrieval.Result) (string, error) {
	if len(results) == 0 {
		return noResults, nil
	}

	blocks := make([]string, 0, len(results))
	for i, r := range results {
		// Path validation happens before any assembly: a traversal or absolute path fails the whole
		// call rather than riding along in a best-effort response. The vault root is empty because
		// this process never opens a vault file — see security.ValidateRelativePath.
		if err := security.ValidateRelativePath("", r.Path); err != nil {
			return "", fmt.Errorf("result %d (uid %q): %w", i, r.UID, err)
		}

		var b strings.Builder
		fmt.Fprintf(&b, "path: %s\n", security.Sanitize(r.Path))
		fmt.Fprintf(&b, "breadcrumb: %s\n", security.Sanitize(r.Breadcrumb))
		fmt.Fprintf(&b, "uid: %s | chunk_index: %v | score: %.4f\n\n",
			security.Sanitize(r.UID), r.ChunkIndex, r.Score)
		b.WriteString(security.Sanitize(r.Text))

		blocks = append(blocks, security.Frame(b.String()))
	}
	return strings.Join(blocks, "\n\n"), nil
}
