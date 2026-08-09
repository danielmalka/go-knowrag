package vault

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/danielmalka/go-knowrag/internal/schema"
)

// createdLayout is the `created` format PRD-contrato §2.4 fixes: a bare date, no clock, no zone.
const createdLayout = "2006-01-02"

// utf8BOM is the byte-order mark an editor prepends when it saves a file as "UTF-8 with BOM". It is
// stripped for the same reason CRLF is normalized, and against the same installation: the vaults are
// git repositories on a Windows disk read from WSL, and an editor can add one without a letter of
// the content changing. Left in place it is invisible and makes the file lie — the `---` prefix
// check below fails three bytes early, so the operator is told "file does not start with a `---`
// frontmatter block" about a file whose first visible characters are `---`. No note in the corpus
// carries one today; neither did any note carry a symlink.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// stripBOM removes every leading BOM, not just the first. A single TrimPrefix left a doubled BOM
// producing the exact misleading error this strip exists to prevent, which is worse than not
// stripping at all: the operator is told the file has no frontmatter, and the one visible clue —
// the leading `---` — says it does. Doubling happens when a tool prepends a BOM without checking
// for one already there, which is what concatenating two BOM-saved files does. The loop costs one
// comparison on the overwhelmingly common no-BOM input.
func stripBOM(raw []byte) []byte {
	for bytes.HasPrefix(raw, utf8BOM) {
		raw = raw[len(utf8BOM):]
	}
	return raw
}

var (
	errFieldMissing      = errors.New("required by PRD-contrato §2.4, missing or empty")
	errNoFrontmatter     = errors.New("file does not start with a `---` frontmatter block")
	errUnterminatedBlock = errors.New("frontmatter block is never closed by a `---` line")
)

// frontmatter mirrors PRD-contrato §2.4's YAML block. It carries no yaml struct tags: decoding goes
// through decodeFrontmatter, which pairs each field with its key there, and a second copy of the key
// names here would be a place for the two to drift.
//
// There is no `area` key and there never will be: `area` does not come from the frontmatter
// (PRD-contrato §2.4b), and the vault's own linter strips any hand-written one. Adding an override
// path here would resurrect a field the write side deletes.
type frontmatter struct {
	UID     string
	Type    string
	Status  string
	Created string
	Tags    []string

	Title      string
	Lang       string
	Visibility string
}

// decodeFrontmatter decodes the YAML block into a map of raw nodes and then decodes each contract
// field from its own node, rather than unmarshalling the block straight into the struct.
//
// The reason is the field name. Decoded in one call, a value of the wrong shape — `tags: golang`,
// a scalar where §2.4 asks for a list, which the corpus writes for `related` today — comes back as
// a yaml.v3 message with the key buried in its text and nothing to put in FrontmatterError.Field
// except the generic "---". That is precisely what that type's doc comment says must not happen.
// Node by node, the failing key is known before the decode is attempted.
//
// Decoding stays non-strict, which is a requirement and not an accident of the rewrite: real notes
// carry domain-pipeline keys the contract deliberately leaves in the vault and out of the index
// (`titulo_exibicao`, `cron_job_id`, …). Unknown keys are simply keys the loop never asks for.
//
// field is the key to blame, "" when err is nil and "---" when the block itself is not a mapping.
func decodeFrontmatter(yamlBlock []byte) (fm frontmatter, field string, err error) {
	var nodes map[string]yaml.Node
	if err := yaml.Unmarshal(yamlBlock, &nodes); err != nil {
		return frontmatter{}, "---", err
	}

	for _, f := range []struct {
		key  string
		into any
	}{
		{"uid", &fm.UID},
		{"type", &fm.Type},
		{"status", &fm.Status},
		{"created", &fm.Created},
		{"tags", &fm.Tags},
		{"title", &fm.Title},
		{"lang", &fm.Lang},
		{"visibility", &fm.Visibility},
	} {
		node, present := nodes[f.key]
		if !present {
			continue
		}
		if err := node.Decode(f.into); err != nil {
			return frontmatter{}, f.key, err
		}
	}
	return fm, "", nil
}

// parseEnumField resolves one frontmatter enum value through internal/schema's registry, the single
// declaration site for every enum in the contract.
//
// The accepted values in the message come from that same registry (All*()) instead of a list typed
// here. Typing them would make this package a second declaration site — the one thing S00 T8's
// architecture test exists to catch — and would leave the operator reading a stale list the day an
// enum grows. Listing them at all is the point of the error: "invalid status" tells the owner of a
// 731-note vault nothing they can act on.
func parseEnumField[T fmt.Stringer](raw string, parse func(string) (T, bool), all []T) (T, error) {
	v, ok := parse(raw)
	if ok {
		return v, nil
	}
	accepted := make([]string, len(all))
	for i, a := range all {
		accepted[i] = a.String()
	}
	return v, fmt.Errorf("%q is not one of %s", raw, strings.Join(accepted, "|"))
}

// normalizeLineEndings converts CRLF and lone CR to LF, per PRD-contrato §2.4. Input with no CR is
// returned unchanged, byte-identical and without a copy.
//
// This is not a defensive hypothesis: 330 of the 731 indexable notes in the real corpus contain
// CR today. The vaults are git repositories on a Windows desktop read from WSL, so `core.autocrlf`
// leaves CRLF in the working tree while the blobs keep LF. Unnormalized, the same note read from
// two places yields two point_hash values and idempotency stops holding.
func normalizeLineEndings(raw []byte) []byte {
	if !bytes.ContainsRune(raw, '\r') {
		return raw
	}
	out := bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	return bytes.ReplaceAll(out, []byte("\r"), []byte("\n"))
}

// parseFrontmatter strips a UTF-8 BOM, normalizes raw to LF, and parses it into a validated Note.
//
// Both run before the `---` split and before any derivation, so the frontmatter YAML, the body and
// every content-derived field are already LF and BOM-free downstream — what S03 chunks, what gets
// embedded, what enters point_hash. This is the only call site in the codebase; nothing normalizes
// a second time.
//
// `type`, `status` and `visibility` are resolved against internal/schema's registry here. This is
// the layer that owns that check: nothing downstream re-reads the frontmatter, so a value not
// caught here is a value nothing will ever catch.
//
// The returned Note has Vault, Area, Sub and Updated unset — those are the caller's to derive.
func parseFrontmatter(path string, raw []byte) (Note, error) {
	raw = normalizeLineEndings(stripBOM(raw))

	yamlBlock, body, err := splitFrontmatter(raw)
	if err != nil {
		return Note{}, &FrontmatterError{Path: path, Field: "---", Err: err}
	}

	fm, badField, err := decodeFrontmatter(yamlBlock)
	if err != nil {
		return Note{}, &FrontmatterError{Path: path, Field: badField, Err: err}
	}

	n := Note{
		Path:  path,
		Tags:  fm.Tags,
		Title: fm.Title,
		Lang:  fm.Lang,
		Body:  string(body),
	}

	for _, required := range []struct {
		name  string
		empty bool
	}{
		{"uid", fm.UID == ""},
		{"type", fm.Type == ""},
		{"status", fm.Status == ""},
		{"created", fm.Created == ""},
		{"tags", len(fm.Tags) == 0},
	} {
		if required.empty {
			return Note{}, &FrontmatterError{Path: path, Field: required.name, Err: errFieldMissing}
		}
	}

	if n.UID, err = parseUID(fm.UID); err != nil {
		return Note{}, &FrontmatterError{Path: path, Field: "uid", Err: err}
	}
	if n.Created, err = time.Parse(createdLayout, fm.Created); err != nil {
		return Note{}, &FrontmatterError{Path: path, Field: "created", Err: err}
	}
	if n.Type, err = parseEnumField(fm.Type, schema.ParseNoteType, schema.AllNoteTypes()); err != nil {
		return Note{}, &FrontmatterError{Path: path, Field: "type", Err: err}
	}
	if n.Status, err = parseEnumField(fm.Status, schema.ParseStatus, schema.AllStatuses()); err != nil {
		return Note{}, &FrontmatterError{Path: path, Field: "status", Err: err}
	}
	// PRD-contrato §2.4's default for an absent `visibility`, resolved through the enum accessor
	// rather than through a string constant in this package: internal/schema stays the one place the
	// value is written down.
	n.Visibility = schema.VisibilityInternal()
	if fm.Visibility != "" {
		n.Visibility, err = parseEnumField(
			fm.Visibility, schema.ParseVisibility, schema.AllVisibilities())
		if err != nil {
			return Note{}, &FrontmatterError{Path: path, Field: "visibility", Err: err}
		}
	}
	return n, nil
}

// splitFrontmatter separates the leading `---` block from the body. It scans lines rather than
// splitting on a fixed delimiter string so that an empty block (`---` immediately followed by
// `---`) and a file with no trailing newline are handled by the same code as the normal case.
func splitFrontmatter(raw []byte) (yamlBlock, body []byte, err error) {
	const opener = "---\n"
	if !bytes.HasPrefix(raw, []byte(opener)) {
		return nil, nil, errNoFrontmatter
	}
	lines := bytes.Split(raw[len(opener):], []byte("\n"))
	for i, line := range lines {
		if !bytes.Equal(line, []byte("---")) {
			continue
		}
		return bytes.Join(lines[:i], []byte("\n")), bytes.Join(lines[i+1:], []byte("\n")), nil
	}
	return nil, nil, errUnterminatedBlock
}

// parseUID enforces PRD-contrato §2.4's "UUIDv7, canonical". This is the trust boundary for uid:
// since S01, schema.PointID takes a uuid.UUID, so the format check that used to live there now has
// to happen where the note enters the system — here.
//
// Canonical means the exact 36-character lowercase hyphenated form. uuid.Parse also accepts four
// other spellings of the same value (upper-case, unhyphenated, "urn:uuid:…", "{…}"), and letting
// them through would mean the same logical note keeps its identity only by luck of how it was
// typed.
func parseUID(s string) (uuid.UUID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, err
	}
	if u.String() != s {
		return uuid.Nil, fmt.Errorf(
			"%q is not the canonical 36-character lowercase form (%q)", s, u.String())
	}
	if u.Version() != 7 {
		return uuid.Nil, fmt.Errorf("%q is UUID version %d, want 7", s, u.Version())
	}
	return u, nil
}
