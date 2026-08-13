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
// hand-reordered file is refused with nothing written.
//
// What that does and does not promise, stated exactly, because the difference is narrow and an
// earlier version of this comment claimed the wider one:
//
//   - It promises this function never *emits* bytes it has not already parsed, and never builds on a
//     file it could not read. Every refusal happens before the file is opened for writing.
//   - It does not promise atomicity. There is no fsync and no temp-file rename, so a process killed
//     inside the write syscall leaves a partial final entry on disk. The next run fails to parse the
//     file and names it; the repair is deleting the truncated lines by hand.
//
// The temp-and-rename that would close that window is deliberately not here, and not for cost: a
// rename publishes a whole file, so two sessions appending at once would end with one of them
// silently overwriting the other's entry. O_APPEND of only the new bytes is what makes concurrent
// appends safe by construction rather than by luck — every writer adds its own bytes to whatever the
// file holds at that moment and rewrites none of them. Trading a guarantee against concurrent loss
// for a guarantee against a torn tail is the wrong trade on a file that is the owner's hand-typed
// work, and it is also why there is no lock here: there is nothing for one to protect.
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
	set, err := decodeGoldenSet(current, path)
	if err != nil {
		return err
	}
	// Here, and not only where a session opens the file. This is the function that writes, so this is
	// where a file that cannot govern an authoring session has to be refused: a zero-byte file decodes
	// into a valid empty GoldenSet, and appending to it produces entries in areas no table constrains
	// and a total no table bounds. Nothing downstream would notice — ValidateCoverage warns at run
	// time and does not gate.
	//
	// What used to stop it was an accident of the caller: drawNote walks CoverageStatus, which returns
	// nothing for a table with no groups, so the session ended before writing. That is the call site
	// protecting the function, which is the failure mode this repository catalogues (CLAUDE.md), and
	// it protects no other caller — a bulk import, a harness, a script.
	if verr := set.Coverage.Validate(); verr != nil {
		return fmt.Errorf("eval: refusing to append to %s, whose coverage table would govern "+
			"nothing: %w", path, verr)
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
// It scans text rather than the parsed document because the parsed document has already thrown away
// the two things it needs: where the bytes sit, and how they are indented. The cost of scanning text
// is that anything which merely *looks* like a top-level key has to be excluded by hand, and both
// exclusions below are bugs this function shipped with:
//
//   - A comment at column zero — `# nota pra mim`, typed between sessions — was read as the next
//     top-level key, and every later append was refused with "move `questions:` to the end" about a
//     file where it already was. That breaks the incremental hand-edited workflow this whole command
//     exists to serve.
//   - A carriage return. In a CRLF file every blank line arrives here as "\r", which is not empty and
//     is not indented, so it read as a top-level key too — same wrong refusal, from a file that had
//     nothing wrong with it. This is not hypothetical: the vaults come off a Windows mount.
//
// The appended block is written with LF line endings even into a CRLF file. YAML accepts either, and
// mixing them changes no byte that was already there, which is the property that matters here.
func questionsLayout(data []byte) (found, last bool, indent string) {
	indent = defaultQuestionIndent
	haveIndent := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSuffix(raw, "\r")
		if strings.HasPrefix(line, questionsKey) {
			found, last, haveIndent = true, true, false
			continue
		}
		body := strings.TrimLeft(line, " \t")
		// Blank lines and comments are not structure. A comment cannot be mistaken for content
		// either: a `#` at the start of a line inside a value only occurs in a block scalar, and a
		// block scalar's lines are indented past their key, so they never reach column zero.
		if body == "" || strings.HasPrefix(body, "#") {
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
