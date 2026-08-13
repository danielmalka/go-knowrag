package eval

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// questionsKey is the top-level key the entries live under. AppendQuestion writes text rather than a
// re-marshalled document, so it has to know the key by name; it is spelled once, here, and the
// `yaml:"questions"` tag on GoldenSet (goldenset.go) is the other half of the same fact.
const questionsKey = "questions:"

// defaultQuestionIndent is what a `questions:` list with no entries yet gets indented by. Two spaces
// is a choice and not a contract — YAML accepts any consistent indentation — which is why
// questionsLayout below copies whatever a file already uses instead of imposing this number on a
// file that disagrees with it.
const defaultQuestionIndent = "  "

// AppendQuestion adds one entry to the golden set at path, as bytes appended to the end of the file.
//
// Appending rather than re-marshalling the whole GoldenSet is the property, not an implementation
// detail. Re-writing the document would discard every comment and every hand edit made between two
// authoring sessions, would re-emit the coverage table the author typed by hand, and would reorder
// the file the day a field is added to GoldenQuestion. Appending leaves every prior byte where it
// was, so a `git diff` after a session shows the entries that were added and nothing else — which is
// what makes the per-entry attribution in provenance.go legible: `git log -S` is searching for the
// question text, and a run that rewrapped the file would move every other entry's introducing commit
// to today.
//
// The cost of appending is that it only produces valid YAML while `questions:` is the last top-level
// key and its entries are the last thing in the file. Two things stand in front of that, and they
// catch different failures: the layout check below refuses a file whose `questions:` key is not last,
// and the read-back parses the exact bytes that would be written before any of them reach disk. A
// hand-reordered file is refused, never corrupted, and nothing is ever half-written.
func AppendQuestion(path string, q GoldenQuestion) error {
	// An entry with an empty area or a uid that is not a UUID is refused, and it is refused by the
	// read-back below rather than by a check up here. There used to be one, and a planted defect that
	// disabled it turned nothing red: decodeGoldenSet validates every entry in the document it
	// parses, so the candidate bytes carry the bad entry into the same validation either way. Two
	// guards for one condition is one guard nobody notices going missing, so this is now the single
	// place — and the entry never reaches disk in either case, which is the property that matters.
	//
	// #nosec G304 -- the path is the golden set the operator named, the same one LoadGoldenSet reads.
	current, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("eval: reading the golden set at %s: %w", path, err)
	}
	if _, err := decodeGoldenSet(current, path); err != nil {
		return err
	}

	found, last, indent := questionsLayout(current)
	// Refused before anything is rendered, and refused on the file's shape rather than on the result
	// of appending to it. There used to be a check after the fact — decode the candidate, compare it
	// against the entries that were there — and a planted defect that disabled it turned nothing red:
	// strict decoding rejects every document the bad append could produce before the comparison runs,
	// so the comparison was a guard for a state no input reaches. This is the reachable condition, so
	// this is where it is refused.
	//
	// A file with no `questions:` key at all is fine: withBlockAppended writes the key at the end,
	// which makes it last by construction.
	if found && !last {
		return fmt.Errorf("eval: `questions:` is not the last top-level key of %s, so an entry "+
			"appended to the end of the file would land under whatever follows it. Move `questions:` "+
			"to the end and run this again — nothing was written", path)
	}

	block, err := questionBlock(q, indent)
	if err != nil {
		return err
	}
	candidate := withBlockAppended(current, block, found)

	// The read-back. It is not the same check as the one above: that one is about where the key sits,
	// this one is about whether the bytes are appendable at all — `questions: []` at the end of the
	// file is the last key and still cannot take an item, and an entry list indented with tabs is not
	// matched by the indent detected above. It also runs the per-entry validation over q, which is why
	// there is no separate check of q on the way in.
	if _, err := decodeGoldenSet(candidate, path); err != nil {
		return fmt.Errorf("eval: appending an entry to the end of %s would not parse, so nothing "+
			"was written: %w", path, err)
	}

	// O_APPEND rather than a rewrite of `candidate`: the bytes already on disk are never rewritten,
	// so an interrupted write can truncate the entry being added and nothing else. The suffix is
	// taken from the candidate rather than rebuilt, so what lands on disk is byte-for-byte what was
	// just decoded and checked.
	// #nosec G304 -- same path as the read above.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("eval: opening the golden set at %s to append to it: %w", path, err)
	}
	if _, err := f.Write(candidate[len(current):]); err != nil {
		_ = f.Close()
		return fmt.Errorf("eval: appending an entry to %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("eval: closing %s after appending an entry: %w", path, err)
	}
	return nil
}

// questionsLayout reads the three facts AppendQuestion needs about a document's `questions:` key:
// whether it has one, whether it is the last top-level key, and the indentation its entries sit at.
//
// A key with no entries yet, or no key at all, answers with defaultQuestionIndent — any consistent
// indentation is valid YAML, and the read-back in AppendQuestion is what proves the one chosen fits.
func questionsLayout(data []byte) (found, last bool, indent string) {
	indent = defaultQuestionIndent
	haveIndent := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, questionsKey) {
			found, last, haveIndent = true, true, false
			continue
		}
		body := strings.TrimLeft(line, " \t")
		if body == "" {
			continue
		}
		// A non-empty line at column zero is a top-level key. One after `questions:` means the entry
		// list is over and something else ends the file, which is the case AppendQuestion refuses:
		// an entry appended to the end would land under that key instead.
		if len(line) == len(body) {
			last = false
			continue
		}
		if found && last && !haveIndent && (strings.HasPrefix(body, "- ") || body == "-") {
			indent, haveIndent = line[:len(line)-len(body)], true
		}
	}
	return found, found && last, indent
}

// questionBlock renders one entry as the lines that go under `questions:`, indented to match.
//
// The field order is GoldenQuestion's declaration order, because that is what yaml.Marshal emits for
// a struct: question, uid, chunk_index when present, area, author, date. It is the same on every
// run, so two authoring sessions produce diffs that read as additions rather than as rewrites.
func questionBlock(q GoldenQuestion, indent string) ([]byte, error) {
	raw, err := yaml.Marshal([]GoldenQuestion{q})
	if err != nil {
		return nil, fmt.Errorf("eval: rendering the entry as YAML: %w", err)
	}
	var b bytes.Buffer
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		b.WriteString(indent)
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.Bytes(), nil
}

// withBlockAppended builds the bytes the file would hold with block on the end, adding the
// `questions:` key when the file does not have one yet.
func withBlockAppended(current, block []byte, haveKey bool) []byte {
	var b bytes.Buffer
	b.Write(current)
	if len(current) > 0 && !bytes.HasSuffix(current, []byte("\n")) {
		b.WriteByte('\n')
	}
	if !haveKey {
		b.WriteString(questionsKey + "\n")
	}
	b.Write(block)
	return b.Bytes()
}
