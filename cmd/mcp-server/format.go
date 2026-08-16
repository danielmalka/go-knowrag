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

// noNote is what get_note returns when the uid matches nothing visible. It is not framed as
// untrusted content because no document contributed a character of it.
const noNote = "No visible chunks for that uid. The note may be archived, private, or not indexed."

// deadAreaFilter replaces noResults when the empty answer was not about the subject at all: the
// search came back with nothing and a probe of the `area` it carried — that facet alone, the others
// cleared by explainEmptyArea — then showed the area matches nothing visible in the index, so every
// query under it is empty whatever it asks (D-28). Same reader as unavailableMessage, and the same
// job — an empty result and a dead filter are one more pair a model must never conflate, and only
// the server can tell them apart.
//
// It blames the area and nothing else because that is the only thing the probe asked about. A
// message that named `type` too would be guessing at which half of a compound filter was dead.
//
// It says "nothing visible" and not "not indexed" because that is all the probe proves: it carries
// the same `status != archived` and `visibility != private` guards the search does, so an area
// whose notes are all archived answers exactly like an area that was never ingested. Naming the
// wrong cause would send an operator looking for a missing ingestion that already ran.
const deadAreaFilter = `EMPTY BECAUSE OF THE AREA FILTER — this is not evidence the topic is absent.

Nothing visible in this knowledge base is under area %[1]q, so a search carrying that filter comes
back empty whatever it asks. The notes may exist and be archived or private, or the area may not be
indexed here at all; either way it is the filter that produced this answer, not the question.

Search again without the area filter before telling the user anything about what is or is not in
the knowledge base.

Operator: MCP_AREAS on this server advertises %[1]q, and nothing visible in the index answers for
it.`

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
		if r.Title != "" {
			fmt.Fprintf(&b, "title: %s\n", security.Sanitize(r.Title))
		}
		fmt.Fprintf(&b, "breadcrumb: %s\n", security.Sanitize(r.Breadcrumb))
		fmt.Fprintf(&b, "uid: %s | chunk_index: %v | score: %.4f\n\n",
			security.Sanitize(r.UID), r.ChunkIndex, r.Score)
		b.WriteString(security.Sanitize(r.Text))

		blocks = append(blocks, security.Frame(b.String()))
	}
	return strings.Join(blocks, "\n\n"), nil
}

// formatNoteResults is formatResults for a uid lookup: same envelope, a different empty sentence
// so a missing note is not read as "the knowledge base has nothing on this subject".
func formatNoteResults(results []retrieval.Result) (string, error) {
	if len(results) == 0 {
		return noNote, nil
	}
	return formatResults(results)
}
