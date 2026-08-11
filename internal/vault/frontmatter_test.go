package vault

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/schema"
)

const validNote = `---
uid: 0198a7f2-4b31-7c42-9e15-3d8a92c47b6a
type: concept
status: draft
created: 2026-08-07
tags: [golang, architecture]
title: Go concurrency
lang: en
---

# Go concurrency

Body text.
`

// withoutLine returns validNote with the frontmatter line starting with prefix removed.
func withoutLine(prefix string) []byte {
	var kept []string
	for _, line := range strings.Split(validNote, "\n") {
		if strings.HasPrefix(line, prefix) {
			continue
		}
		kept = append(kept, line)
	}
	return []byte(strings.Join(kept, "\n"))
}

func TestParseFrontmatter_Valid(t *testing.T) {
	n, err := parseFrontmatter("research/golang/concurrency.md", []byte(validNote))
	if err != nil {
		t.Fatalf("parseFrontmatter: %v", err)
	}

	if n.Path != "research/golang/concurrency.md" {
		t.Errorf("Path = %q", n.Path)
	}
	if got := n.UID.String(); got != "0198a7f2-4b31-7c42-9e15-3d8a92c47b6a" {
		t.Errorf("UID = %q", got)
	}
	if n.Type != schema.NoteTypeConcept() || n.Status != schema.StatusDraft() || n.Lang != "en" {
		t.Errorf("Type/Status/Lang = %s/%s/%q", n.Type, n.Status, n.Lang)
	}
	if n.Created.Format("2006-01-02") != "2026-08-07" {
		t.Errorf("Created = %v", n.Created)
	}
	if len(n.Tags) != 2 || n.Tags[0] != "golang" || n.Tags[1] != "architecture" {
		t.Errorf("Tags = %v", n.Tags)
	}
	if n.Title != "Go concurrency" {
		t.Errorf("Title = %q", n.Title)
	}
	// The body is the raw text after the closing `---`, unmodified apart from LF normalization.
	if want := "\n# Go concurrency\n\nBody text.\n"; n.Body != want {
		t.Errorf("Body = %q, want %q", n.Body, want)
	}
}

// TestParseFrontmatter_MissingRequiredField_ReturnsErrorWithPathAndField covers the five required
// fields independently: the operator must learn which file and which field, or "invalid
// frontmatter" sends them reading 700 notes.
func TestParseFrontmatter_MissingRequiredField_ReturnsErrorWithPathAndField(t *testing.T) {
	const path = "research/golang/concurrency.md"
	for _, field := range []string{"uid", "type", "status", "created", "tags"} {
		t.Run(field, func(t *testing.T) {
			_, err := parseFrontmatter(path, withoutLine(field+":"))

			var fmErr *FrontmatterError
			if !errors.As(err, &fmErr) {
				t.Fatalf("got %v (%T), want *FrontmatterError", err, err)
			}
			if fmErr.Path != path {
				t.Errorf("Path = %q, want %q", fmErr.Path, path)
			}
			if fmErr.Field != field {
				t.Errorf("Field = %q, want %q", fmErr.Field, field)
			}
		})
	}
}

func TestParseFrontmatter_VisibilityDefaultsToInternal(t *testing.T) {
	n, err := parseFrontmatter("a.md", []byte(validNote))
	if err != nil {
		t.Fatalf("parseFrontmatter: %v", err)
	}
	if n.Visibility != schema.VisibilityInternal() {
		t.Errorf("absent visibility = %s, want %s", n.Visibility, schema.VisibilityInternal())
	}

	explicit := strings.Replace(validNote, "lang: en", "lang: en\nvisibility: private", 1)
	n, err = parseFrontmatter("a.md", []byte(explicit))
	if err != nil {
		t.Fatalf("parseFrontmatter: %v", err)
	}
	if n.Visibility != schema.VisibilityPrivate() {
		t.Errorf("explicit visibility = %s, want %s", n.Visibility, schema.VisibilityPrivate())
	}
}

// TestParseFrontmatter_EnumFieldsAreValidated closes a punt whose two halves pointed at each other:
// Note documented a frontmatter validator as the owner of `type`/`status`/`visibility`, S00 T7 left
// the decision to S02, and neither side implemented one — so the three fields were validated
// nowhere. The status values below are not invented: they are in the real corpus today, and each of
// them reached NoteMetadataFields, the point_hash and the payload of a note that the query-time
// default filter `status != archived` cannot classify.
//
// The message must list what is accepted. "invalid status" tells the owner of a 731-note vault
// nothing they can act on, and the list is generated from the registry so it cannot go stale.
func TestParseFrontmatter_EnumFieldsAreValidated(t *testing.T) {
	tests := []struct {
		name, field, line string
		accepted          []string
	}{
		{"type", "type", "type: conceito", stringsOf(schema.AllNoteTypes())},
		// A real path in the corpus, one vault.
		{"status maduro", "status", "status: maduro", stringsOf(schema.AllStatuses())},
		// Same vault, a tag written into the status field
		{"status tag", "status", `status: "#em-progresso"`, stringsOf(schema.AllStatuses())},
		// The other vault
		{"status todo", "status", "status: todo", stringsOf(schema.AllStatuses())},
		{"visibility", "visibility", "visibility: publico", stringsOf(schema.AllVisibilities())},
		// Case matters: the contract fixes the spelling, and a value that only matches when
		// lowercased is a different value.
		{"case", "status", "status: Draft", stringsOf(schema.AllStatuses())},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := strings.Replace(string(withoutLine(tc.field+":")), "lang: en",
				"lang: en\n"+tc.line, 1)

			var fmErr *FrontmatterError
			if _, err := parseFrontmatter("a.md", []byte(raw)); !errors.As(err, &fmErr) {
				t.Fatalf("got %v, want *FrontmatterError", err)
			} else if fmErr.Field != tc.field {
				t.Errorf("Field = %q, want %q", fmErr.Field, tc.field)
			} else {
				for _, want := range tc.accepted {
					if !strings.Contains(fmErr.Error(), want) {
						t.Errorf("error does not offer the accepted value %q:\n%s", want, fmErr)
					}
				}
			}
		})
	}
}

func stringsOf[T fmt.Stringer](values []T) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = v.String()
	}
	return out
}

// TestParseFrontmatter_ValidEnumValuesRoundTrip: every registered value must parse. A validator
// that rejects a canonical value is worse than none, and this is the half of the check a fixture
// with four notes in it cannot cover.
func TestParseFrontmatter_ValidEnumValuesRoundTrip(t *testing.T) {
	for _, ty := range schema.AllNoteTypes() {
		raw := strings.Replace(validNote, "type: concept", "type: "+ty.String(), 1)
		n, err := parseFrontmatter("a.md", []byte(raw))
		if err != nil {
			t.Errorf("type %s rejected: %v", ty, err)
		} else if n.Type != ty {
			t.Errorf("type %s parsed as %s", ty, n.Type)
		}
	}
	for _, st := range schema.AllStatuses() {
		raw := strings.Replace(validNote, "status: draft", "status: "+st.String(), 1)
		n, err := parseFrontmatter("a.md", []byte(raw))
		if err != nil {
			t.Errorf("status %s rejected: %v", st, err)
		} else if n.Status != st {
			t.Errorf("status %s parsed as %s", st, n.Status)
		}
	}
	for _, vis := range schema.AllVisibilities() {
		raw := strings.Replace(validNote, "lang: en", "lang: en\nvisibility: "+vis.String(), 1)
		n, err := parseFrontmatter("a.md", []byte(raw))
		if err != nil {
			t.Errorf("visibility %s rejected: %v", vis, err)
		} else if n.Visibility != vis {
			t.Errorf("visibility %s parsed as %s", vis, n.Visibility)
		}
	}
}

// TestParseFrontmatter_WronglyTypedValueNamesItsField: `tags: golang` is a scalar where §2.4 asks
// for a list — the shape 18 notes already use for `related`. Decoded in one call the key survives
// only inside yaml.v3's message text, and FrontmatterError.Field falls back to the generic "---",
// which is the failure its own doc comment calls out: a field without a path sends the operator
// reading every note in the vault.
func TestParseFrontmatter_WronglyTypedValueNamesItsField(t *testing.T) {
	for _, tc := range []struct{ field, line string }{
		{"tags", "tags: golang"},
		{"title", "title: [a, b]"},
		{"uid", "uid: {a: b}"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			raw := strings.Replace(string(withoutLine(tc.field+":")), "lang: en",
				"lang: en\n"+tc.line, 1)

			var fmErr *FrontmatterError
			if _, err := parseFrontmatter("a.md", []byte(raw)); !errors.As(err, &fmErr) {
				t.Fatalf("got %v, want *FrontmatterError", err)
			} else if fmErr.Field != tc.field {
				t.Errorf("Field = %q, want %q (message was: %v)", fmErr.Field, tc.field, fmErr)
			}
		})
	}
}

// TestParseFrontmatter_UTF8BOMIsStripped: a BOM is three invisible bytes ahead of the `---`, so the
// prefix check fails and the operator is told the file has no frontmatter block while looking at
// one that plainly starts with `---`. Same origin as the CRLF problem (a git repository on a
// Windows disk, an editor that rewrites the file without changing a letter), same treatment: remove
// it up front so nothing downstream has to know it existed.
func TestParseFrontmatter_UTF8BOMIsStripped(t *testing.T) {
	plain, err := parseFrontmatter("a.md", []byte(validNote))
	if err != nil {
		t.Fatalf("parsing the plain note: %v", err)
	}

	// The doubled case is not padding. A single TrimPrefix passes the first row and fails the
	// second, and its failure is the *original* misleading error: "file does not start with a
	// `---` frontmatter block", about a file that does. A tool that prepends a BOM without
	// checking for one produces it — concatenating two BOM-saved files is enough.
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"single", append([]byte("\xef\xbb\xbf"), validNote...)},
		{"doubled", append([]byte("\xef\xbb\xbf\xef\xbb\xbf"), validNote...)},
		{"with CRLF", append([]byte("\xef\xbb\xbf"),
			[]byte(strings.ReplaceAll(validNote, "\n", "\r\n"))...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseFrontmatter("a.md", tc.raw)
			if err != nil {
				t.Fatalf("parsing a BOM-prefixed note: %v", err)
			}
			if got.Body != plain.Body {
				t.Errorf("Body differs:\n  plain = %q\n  BOM   = %q", plain.Body, got.Body)
			}
			if a, b := schema.SerializeFields(NoteMetadataFields(plain)),
				schema.SerializeFields(NoteMetadataFields(got)); a != b {
				t.Errorf("serialized metadata differs:\n  plain = %q\n  BOM   = %q", a, b)
			}
		})
	}
}

func TestParseFrontmatter_UIDMustBeCanonicalUUIDv7(t *testing.T) {
	// Since S01, schema.PointID takes a uuid.UUID, so this is where uid format is enforced.
	tests := map[string]string{
		"not a uuid":   "nope",
		"unhyphenated": "0198a7f24b317c429e153d8a92c47b6a",
		"upper case":   "0198A7F2-4B31-7C42-9E15-3D8A92C47B6A",
		"urn prefixed": "urn:uuid:0198a7f2-4b31-7c42-9e15-3d8a92c47b6a",
		// Quoted, because unquoted braces are a YAML flow mapping and would be rejected one layer
		// earlier by the decoder rather than by the canonical-form check under test here.
		"brace wrapped":    `"{0198a7f2-4b31-7c42-9e15-3d8a92c47b6a}"`,
		"version 4, not 7": "9f1b5c2e-6d3a-4f8b-9c1d-2e3f4a5b6c7d",
	}
	for name, uid := range tests {
		t.Run(name, func(t *testing.T) {
			raw := strings.Replace(validNote,
				"uid: 0198a7f2-4b31-7c42-9e15-3d8a92c47b6a", "uid: "+uid, 1)

			var fmErr *FrontmatterError
			if _, err := parseFrontmatter("a.md", []byte(raw)); !errors.As(err, &fmErr) {
				t.Fatalf("got %v, want *FrontmatterError", err)
			} else if fmErr.Field != "uid" {
				t.Errorf("Field = %q, want %q", fmErr.Field, "uid")
			}
		})
	}
}

func TestParseFrontmatter_NoFrontmatterBlock(t *testing.T) {
	for name, raw := range map[string]string{
		"no opener":    "# Just a heading\n",
		"never closed": "---\nuid: x\ntype: concept\n\n# Heading\n",
	} {
		t.Run(name, func(t *testing.T) {
			var fmErr *FrontmatterError
			if _, err := parseFrontmatter("a.md", []byte(raw)); !errors.As(err, &fmErr) {
				t.Fatalf("got %v, want *FrontmatterError", err)
			}
		})
	}
}

func TestNormalizeLineEndings_CRLFAndLoneCR_BecomeLF(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"crlf", "a\r\nb\r\n", "a\nb\n"},
		{"lone cr", "a\rb\r", "a\nb\n"},
		{"mixed with an already-LF run", "a\r\nb\rc\nd\n", "a\nb\nc\nd\n"},
		{"cr at end of input", "a\r", "a\n"},
		{"lf only is untouched", "a\nb\n", "a\nb\n"},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(normalizeLineEndings([]byte(tc.in))); got != tc.want {
				t.Errorf("normalizeLineEndings(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestNormalizeLineEndings_NoCRReturnsSameBacking proves the "byte-identical" claim in the
// strongest available form: input without CR is not merely equal, it is the same allocation.
func TestNormalizeLineEndings_NoCRReturnsSameBacking(t *testing.T) {
	in := []byte("a\nb\n")
	if out := normalizeLineEndings(in); &out[0] != &in[0] {
		t.Error("normalizeLineEndings copied input that contained no CR")
	}
}

// TestParseFrontmatter_CRLFNoteEqualsLFNote_ByteIdentical is the acceptance criterion for
// PRD-contrato §2.4's normalization rule. The CRLF twin has CRLF *inside the frontmatter block
// too*, which is what proves normalization runs before the `---` split rather than after it: split
// first and the closing delimiter line would be "---\r", matching nothing.
func TestParseFrontmatter_CRLFNoteEqualsLFNote_ByteIdentical(t *testing.T) {
	lf := []byte(validNote)
	crlf := bytes.ReplaceAll(lf, []byte("\n"), []byte("\r\n"))

	fromLF, err := parseFrontmatter("a.md", lf)
	if err != nil {
		t.Fatalf("parsing the LF note: %v", err)
	}
	fromCRLF, err := parseFrontmatter("a.md", crlf)
	if err != nil {
		t.Fatalf("parsing the CRLF note: %v", err)
	}

	if fromLF.Body != fromCRLF.Body {
		t.Errorf("Body differs:\n  LF   = %q\n  CRLF = %q", fromLF.Body, fromCRLF.Body)
	}
	if fromLF.Title != fromCRLF.Title {
		t.Errorf("Title differs: %q vs %q", fromLF.Title, fromCRLF.Title)
	}
	if a, b := schema.SerializeFields(NoteMetadataFields(fromLF)),
		schema.SerializeFields(NoteMetadataFields(fromCRLF)); a != b {
		t.Errorf("serialized metadata differs:\n  LF   = %q\n  CRLF = %q", a, b)
	}
}

// TestParseFrontmatter_UnknownKeysAreTolerated: real notes carry domain-pipeline keys that
// PRD-contrato §2.4 deliberately keeps in the vault and out of the index. Strict decoding would
// reject notes that are valid under the contract.
func TestParseFrontmatter_UnknownKeysAreTolerated(t *testing.T) {
	raw := strings.Replace(validNote, "lang: en",
		"lang: en\ntitulo_exibicao: Concorrência em Go\ncron_job_id: 42", 1)
	if _, err := parseFrontmatter("a.md", []byte(raw)); err != nil {
		t.Errorf("unknown frontmatter keys rejected the note: %v", err)
	}
}
