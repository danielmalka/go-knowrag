package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/danielmalka/go-knowrag/internal/clicmd"
	"github.com/danielmalka/go-knowrag/internal/config"
	"github.com/danielmalka/go-knowrag/internal/eval"
	"github.com/danielmalka/go-knowrag/internal/vault"
)

// goldenAuthorEnv carries the name written into every entry's `author`.
//
// It arrives by environment variable, and not as a flag with a default or a value in a config file,
// for the reason CLAUDE.md gives for the vault paths: this repository is public, and a person's name
// must never reach a tracked file. The golden set itself is not tracked — it lives under docs/, which
// is gitignored — so the name is safe once it is in there, and only the route matters.
const goldenAuthorEnv = "KNOWRAG_GOLDEN_AUTHOR"

// excerptLines and excerptRunes bound the body shown for one note, and both numbers are decided
// here: nothing else in this repository reads them.
//
// They are a compromise with one failure mode on each side. Too little and the note is not
// recognisable, so the author cannot write a question about it; too much and the author reads the
// note instead of remembering it, and writes a question in the note's own vocabulary — which turns
// the eval into a string match and is the thing the reminder beside the prompt exists to prevent.
// Six lines of about 400 runes is roughly a screenful of a paragraph: enough to place a note, short
// enough that reading it is not how you spend the session.
const (
	excerptLines = 6
	excerptRunes = 400
)

// promptRule and promptCriterion are printed above every prompt, not once at the start of the
// session. Guidance that lives anywhere else — a file, a header, a README — is guidance nobody is
// reading at question forty, and the moment of typing is the only channel that reaches the author.
//
// promptRule is the whole instruction compressed into one sentence, and it is phrased as a situation
// rather than as a rule because a situation is reproducible: "at 11pm, without remembering the file
// name" is a thing the author can actually put himself in, where "be original" is not. It is what
// produces the property promptCriterion names — that the value of a question is the distance between
// how he asks and how the note is written, because that distance is what semantic retrieval exists
// to cross. A question that reuses the note's vocabulary is answered by matching strings and scores
// perfectly without proving anything.
//
// Three lines total, deliberately. A paragraph here is a paragraph he skips, and a skipped reminder
// is the same as no reminder.
const (
	promptRule      = "Ask it the way you would at 11pm, without remembering the file name."
	promptCriterion = "The further your wording is from the note's own, the more this measures " +
		"retrieval instead of string matching."
)

// goldenPrompt is the prompt itself. Its exact text is part of the output contract asserted in
// golden_test.go: nothing this command prints may fall outside the shapes listed there, and that is
// what stops a search result from ever appearing on this stream.
const goldenPrompt = "question (empty to skip, q to stop): "

// promptNudges is the third line: one suggested kind of question per turn, rotated.
//
// One at a time rather than a menu of four, and that is a decision with a cost worth stating. Listing
// every kind on every prompt reads as a reference table and gets skipped; suggesting one biases the
// author toward it, which is the mechanism, not a side effect. The bias to compare it against is the
// one that exists already: left alone, an author writes precise-fact questions in a row, because that
// is the kind that comes to mind while looking at a note — and it is the least informative kind here,
// since a precise fact usually shares its rare term with the note and retrieves on that term alone.
// A nudge he can ignore beats a default he does not notice.
//
// The cycle is what realizes the target mix rather than a second place the proportion is written
// down: six turns hold three natural kinds, two "hard", and one associative, which is the
// half / a third / the rest that progressMix states. TestPromptNudges_RealiseTheTargetMix checks the
// two against each other, so changing one without the other fails rather than drifts.
//
// "Hard" is not a kind, it is a difficulty, and it earns two of the six on its own: a golden set of
// questions the search already answers saturates. At 95% recall there is no room left to get worse,
// so no future change can show up as a regression and the set certifies that all is well, forever.
// The questions that fail today are the informative ones — each is a retrieval defect with an
// address.
//
// The examples are deliberately generic. This file is tracked and this repository is public
// (CLAUDE.md), so no example may be drawn from the vault the tool actually runs against.
var promptNudges = []struct{ kind, hint string }{
	{"a decision", `why something was ruled out — "why didn't we go with X for that?"`},
	{"a hard one", "one you suspect the search will get wrong"},
	{"a procedure", `how you do something — "how do I redeploy after changing the proxy?"`},
	{"an associative one", `no words in common with the note — "that study about chunk size I read once"`},
	{"a precise fact", `one small answer — "which port does that service listen on?"`},
	{"a hard one", "one you suspect the search will get wrong"},
}

// progressMix is the one line of the progress report that is about difficulty rather than counts.
//
// It states the target because nothing stores it: GoldenQuestion has no difficulty field, so no
// report can say how many hard questions the set holds, and this sentence is the only thing standing
// between the author and sixty easy ones. It says so out loud rather than implying a tracking that
// does not exist — the report claiming a number it never measured is the failure mode this
// repository has paid for before.
const progressMix = "  mix: aim for about half natural, a third hard (ones you expect to fail), " +
	"the rest associative - nothing records this, so this line is the only reminder"

// newGoldenCmd builds `golden`.
//
// The name is flat rather than `golden add`, matching the rest of this tree (schema, ingest, search,
// stats, eval): a parent command with one child is scaffolding for a second verb that does not
// exist. When one does, this becomes `golden add` and the parent gains a purpose in the same change.
//
// There is no --json. The command is a conversation with a person — it refuses to run without a
// terminal — and an envelope on a stream nobody is parsing is a flag that only ever gets tested.
func newGoldenCmd(cfg *config.Config) *cobra.Command {
	var path string

	cmd := &cobra.Command{
		Use:   "golden",
		Short: "Author golden-set questions, one note at a time",
		Long: "golden draws a note from the vaults, shows you enough of it to remember what it is\n" +
			"about, and asks you for one question that note should answer. It appends what you\n" +
			"write to the golden set and draws the next one, so a session can stop anywhere and\n" +
			"resume later.\n\n" +
			"It draws from the area the coverage table is shortest on, never uniformly, and never\n" +
			"draws a note that already has a question.\n\n" +
			"It never runs a search and never shows a search result, and that is the point of the\n" +
			"instrument rather than a limitation of it: a question written after seeing what the\n" +
			"index returns is a question tuned until it passes, and a golden set of those measures\n" +
			"itself. It reads the vaults, not the index, so it needs no Qdrant and no embedder.\n\n" +
			"Every prompt carries the rule that decides whether a question is worth anything, one\n" +
			"suggested kind of question, and the target mix. The long version — the anti-patterns,\n" +
			"and what to do when a question turns out to be ambiguous — is in docs/eval/, which is\n" +
			"not tracked: every useful example of a good question quotes the vault.\n\n" +
			"The `author` field comes from " + goldenAuthorEnv + " (falling back to USER) and the\n" +
			"`date` from today. Both are documentation: what proves a question was written before\n" +
			"the result it grades is the git history of the golden set, not a field in it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true

			// os.Stdin rather than cmd.InOrStdin(), for the reason `ingest` gives at the same call:
			// whether a human is on the other end is a property of the file descriptor, not of the
			// io.Reader cobra hands over (cmd/cli/ingest_modes.go, isTerminal).
			return runGolden(os.Stdin, cmd.OutOrStdout(), cmd.InOrStdin(), cfg, path)
		},
	}

	cmd.Flags().StringVar(&path, "file", DefaultGoldenSetPath,
		"golden set to append to; it must already exist and declare its `coverage:` table")
	return cmd
}

// runGolden is the whole command: refuse if there is nobody to ask, read what exists, scan the
// vaults, and hand the loop what it needs.
//
// The terminal check is here rather than at the call site because this is the function that blocks
// on a read. A guard a caller can forget is a guard that a cron job discovers by hanging until
// somebody notices, which is worse than either answer — the same reading confirmPrune writes down
// for --prune (cmd/cli/ingest_modes.go).
func runGolden(stdin *os.File, out io.Writer, in io.Reader, cfg *config.Config, path string) error {
	if !isTerminal(stdin) {
		return fmt.Errorf("%w: `golden` asks you a question per note and reads the answer from "+
			"stdin, and stdin is not a terminal — there is nobody to answer, and waiting for one "+
			"would hang. Run it from a terminal", errUsage)
	}

	set, err := openGoldenSetForAuthoring(path)
	if err != nil {
		return err
	}

	author, err := goldenAuthor()
	if err != nil {
		return err
	}

	names, err := selectVaults(cfg, bothVaults)
	if err != nil {
		return err
	}
	if err := cfg.RequireVaults(names...); err != nil {
		return err
	}
	scans, err := scanVaults(cfg, names)
	if err != nil {
		return err
	}

	return authorGolden(out, in, path, noteCards(scans), set, author,
		time.Now().Format(time.DateOnly), rand.IntN)
}

// openGoldenSetForAuthoring reads the set a session will append to.
//
// Both failures are usage failures rather than backend ones, which is the reading `eval --golden`
// already gives the same file (cmd/cli/eval.go): the operator named a path, and nothing on the far
// side is going to change, so a caller that retried the identical command line would retry it
// forever.
//
// The two are told apart because the answers are different. A file that is not there is not an error
// to debug, it is the first step nobody has taken — and the coverage table is the part this command
// cannot invent: which areas exist and how many questions each needs is the owner's, and writing a
// guess into it would aim every session at the wrong thing.
//
// It is a function rather than six lines inside runGolden so a test can reach it. runGolden refuses a
// non-terminal stdin before it gets here, and a terminal cannot be faked without a pty, so inline
// this branch would be unreachable from any test — which is how a refusal quietly stops refusing.
func openGoldenSetForAuthoring(path string) (eval.GoldenSet, error) {
	set, err := eval.ReadGoldenSet(path)
	if err == nil {
		return set, nil
	}
	if errors.Is(err, eval.ErrGoldenSetMissing) {
		return eval.GoldenSet{}, clicmd.Usage("no golden set at %s. Create it with a `coverage:` "+
			"table first — the table is what says how many questions each area needs, and this "+
			"command has nothing to aim at without it (internal/eval/coverage.go, CoverageTable)", path)
	}
	return eval.GoldenSet{}, clicmd.Usage("%v", err)
}

// goldenAuthor resolves the name every entry is attributed to. USER is the fallback because a
// terminal session almost always has one and being asked to export a variable before writing the
// first question is friction over a field that is documentation.
func goldenAuthor() (string, error) {
	for _, name := range []string{goldenAuthorEnv, "USER"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value, nil
		}
	}
	return "", clicmd.Usage("no author to attribute these questions to: set %s", goldenAuthorEnv)
}

// noteCard is everything this command will ever show about one note, resolved once at scan time.
//
// It is a separate type from vault.Note, and it holds strings rather than a *vault.Note, because it
// is the boundary of what can be printed: whatever is not in here cannot reach the terminal. The
// note's body is cut to an excerpt at this line and not at the print, so there is no path on which
// the whole note — or anything else the scan carried — is printable.
type noteCard struct {
	UID     string
	Area    string
	Title   string
	Path    string
	Excerpt []string
}

// noteCards flattens the scans into the deck to draw from. Sorted by vault path so the candidate
// list a draw indexes into is the same on every run.
func noteCards(scans []vault.ScanResult) []noteCard {
	var cards []noteCard
	for _, scan := range scans {
		for _, note := range scan.Notes {
			cards = append(cards, noteCard{
				UID:     note.UID.String(),
				Area:    note.Area,
				Title:   noteTitle(note),
				Path:    note.Vault + "/" + note.Path,
				Excerpt: excerpt(note.Body),
			})
		}
	}
	slices.SortFunc(cards, func(a, b noteCard) int { return strings.Compare(a.Path, b.Path) })
	return cards
}

// noteTitle falls back to the file name, because `title` is one of the optional frontmatter fields
// (internal/vault/note.go) and a card with no title at all is a card nobody recognises.
func noteTitle(note vault.Note) string {
	if note.Title != "" {
		return note.Title
	}
	name := note.Path
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return strings.TrimSuffix(name, ".md")
}

// excerpt cuts the body down to what the card shows, by the two bounds above. Blank lines are
// dropped rather than counted, so a note written with a blank line between every paragraph does not
// spend half its allowance on nothing.
func excerpt(body string) []string {
	var out []string
	budget := excerptRunes
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, " \t\r")
		if line == "" {
			continue
		}
		runes := []rune(line)
		if len(runes) > budget {
			out = append(out, string(runes[:budget])+"...")
			break
		}
		budget -= len(runes)
		out = append(out, line)
		if len(out) == excerptLines {
			break
		}
	}
	return out
}

// authorGolden is the session: progress, then a card and a question until there is nothing left to
// ask or the author stops, then progress again.
//
// It takes the deck, the set and the draw as parameters so a test drives a whole session with no
// vault, no file system beyond the golden set, and a draw that is not random.
func authorGolden(
	out io.Writer,
	in io.Reader,
	path string,
	cards []noteCard,
	set eval.GoldenSet,
	author, date string,
	draw func(int) int,
) error {
	// Seeded from what is already in the file, which is the half of "never draw the same note twice"
	// that survives quitting: a note answered in a previous session is one of these on the next.
	asked := make(map[string]bool, len(set.Questions))
	for _, q := range set.Questions {
		asked[q.UID] = true
	}

	if err := printProgress(out, path, set); err != nil {
		return err
	}

	reader := bufio.NewReader(in)
	// turn counts the prompts shown, and it is what rotates promptNudges. It counts cards offered
	// rather than questions saved, so skipping a note still advances the suggestion — otherwise a run
	// of skips would offer the same kind over and over.
	turn := 0
	// The loop condition is the table's ceiling: a MaxTotal of zero is "no maximum" (coverage.go,
	// CoverageGroup), and nothing downstream would refuse the extra entries — ValidateCoverage only
	// warns at run time — so this is the one place that number stops anything.
	for set.Coverage.MaxTotal == 0 || len(set.Questions) < set.Coverage.MaxTotal {
		card, ok := drawNote(cards, eval.CoverageStatus(set.Questions, set.Coverage), asked, draw)
		if !ok {
			break
		}
		// Marked before the answer, not after: a skipped note that stayed unmarked would be drawn
		// again on the next turn, and a deck of one would loop forever.
		asked[card.UID] = true

		if err := printCard(out, card); err != nil {
			return err
		}
		text, stop, err := ask(out, reader, turn)
		turn++
		if err != nil {
			return err
		}
		if stop {
			break
		}
		if text == "" {
			if _, err := fmt.Fprintln(out, "skipped, nothing written"); err != nil {
				return fmt.Errorf("writing the session: %w", err)
			}
			continue
		}

		q := eval.GoldenQuestion{
			Question: text,
			UID:      card.UID,
			Area:     card.Area,
			Author:   author,
			Date:     date,
		}
		if err := eval.AppendQuestion(path, q); err != nil {
			return err
		}
		set.Questions = append(set.Questions, q)
		if _, err := fmt.Fprintf(out, "saved: %d question(s)\n", len(set.Questions)); err != nil {
			return fmt.Errorf("writing the session: %w", err)
		}
	}

	return printProgress(out, path, set)
}

// drawNote picks one note to ask about: from the neediest area that still has an unasked note, at
// random within it.
//
// The area order is CoverageStatus's, not a second derivation of it — walking that list and taking
// the first area with a candidate is what makes "the area the progress report says is shortest" and
// "the area the next card comes from" the same fact rather than two that can disagree. Areas the
// table declares full are already last there and are skipped outright.
func drawNote(cards []noteCard, status []eval.AreaStatus, asked map[string]bool, draw func(int) int) (noteCard, bool) {
	for _, area := range status {
		if area.Full() {
			continue
		}
		var candidates []noteCard
		for _, card := range cards {
			if card.Area == area.Area && !asked[card.UID] {
				candidates = append(candidates, card)
			}
		}
		if len(candidates) == 0 {
			continue
		}
		return candidates[draw(len(candidates))], true
	}
	return noteCard{}, false
}

// printProgress writes where the set stands, in CoverageStatus's order — neediest first, so what to
// do next is the first line rather than something to find in a table of twenty areas.
func printProgress(out io.Writer, path string, set eval.GoldenSet) error {
	if _, err := fmt.Fprintf(out, "golden set %s: %d question(s), target %d-%d\n",
		path, len(set.Questions), set.Coverage.MinTotal, set.Coverage.MaxTotal); err != nil {
		return fmt.Errorf("writing the progress report: %w", err)
	}
	for _, area := range eval.CoverageStatus(set.Questions, set.Coverage) {
		standing := "ok"
		switch {
		case area.Needs() > 0:
			standing = fmt.Sprintf("%d more needed", area.Needs())
		case area.Full():
			standing = "full"
		}
		if _, err := fmt.Fprintf(out, "  area %s: %d question(s), min %d, max %d - %s\n",
			area.Area, area.Have, area.Min, area.Max, standing); err != nil {
			return fmt.Errorf("writing the progress report: %w", err)
		}
	}
	if _, err := fmt.Fprintln(out, progressMix); err != nil {
		return fmt.Errorf("writing the progress report: %w", err)
	}
	return nil
}

// printCard shows the note. Every line is prefixed with the field it carries, so the shape of this
// command's output is decidable line by line — which is what the output contract in golden_test.go
// rests on, and through it the guarantee that no search result is ever printed here.
func printCard(out io.Writer, card noteCard) error {
	lines := []string{
		"",
		"area:  " + card.Area,
		"title: " + card.Title,
		"path:  " + card.Path,
	}
	for _, line := range card.Excerpt {
		lines = append(lines, "body:  "+line)
	}
	for _, line := range lines {
		if _, err := fmt.Fprintln(out, line); err != nil {
			return fmt.Errorf("writing the note: %w", err)
		}
	}
	return nil
}

// ask prints the guidance and the prompt and reads one line. turn selects the rotated suggestion.
//
// EOF on an empty read is a stop rather than a skip: a closed stdin is not somebody declining this
// note, it is the end of the session, and treating it as a skip would burn through the whole deck
// marking every note asked.
func ask(out io.Writer, in *bufio.Reader, turn int) (text string, stop bool, err error) {
	nudge := promptNudges[turn%len(promptNudges)]
	for _, line := range []string{
		promptRule,
		promptCriterion,
		fmt.Sprintf("try %s: %s", nudge.kind, nudge.hint),
	} {
		if _, err := fmt.Fprintln(out, line); err != nil {
			return "", false, fmt.Errorf("writing the prompt: %w", err)
		}
	}
	if _, err := fmt.Fprint(out, goldenPrompt); err != nil {
		return "", false, fmt.Errorf("writing the prompt: %w", err)
	}

	line, rerr := in.ReadString('\n')
	text = strings.TrimSpace(line)
	if rerr != nil && !errors.Is(rerr, io.EOF) {
		return "", false, fmt.Errorf("reading the question: %w", rerr)
	}
	if errors.Is(rerr, io.EOF) && text == "" {
		return "", true, nil
	}
	if text == "q" {
		return "", true, nil
	}
	return text, false, nil
}
