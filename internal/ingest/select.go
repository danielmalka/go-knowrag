package ingest

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/danielmalka/go-knowrag/internal/vault"
)

// ErrBadPattern is a --only glob this build cannot honour. It is a sentinel so cmd/cli can map it to
// a usage exit code: nothing is broken, the pattern is.
var ErrBadPattern = errors.New("ingest: invalid --only pattern")

// subtreeSuffix is the only form of `**` this build accepts, and it has to be the end of the
// pattern: `vault/areas/**` means that folder and everything under it.
const subtreeSuffix = "/**"

// FilterByGlob restricts the note set to what matches pattern, matched against `vault/path`.
//
// The vault is part of the target, not just the path, because the roster holds more than one and
// their trees look alike: `areas/**` with two vaults configured would silently take both, and an
// operator who wrote it to mean one of them gets a run twice the size they asked for.
//
// **`**` is supported only as a trailing `/**`, and rejected anywhere else.** path.Match has no
// recursive wildcard at all — it reads `**` as `*`, which does not cross a `/` — so a pattern like
// `pessoal/**/notas.md` quietly becomes `pessoal/*/notas.md` and matches one level instead of every
// level. That is the failure this refuses: not an error the operator can see, but a smaller run that
// looks like the one they asked for. The trailing form is handled here as a prefix test rather than
// handed to path.Match, which is why it is the one form that works.
//
// An unusable pattern is an error and never an empty result. `--only` exists to narrow a run, and a
// run narrowed to nothing is indistinguishable from a corpus that has nothing to do — the operator
// would read "0 notes" as success.
func FilterByGlob(notes []vault.Note, pattern string) ([]vault.Note, error) {
	if pattern == "" {
		return notes, nil
	}

	prefix, subtree := strings.CutSuffix(pattern, subtreeSuffix)
	if strings.Contains(prefix, "**") {
		return nil, fmt.Errorf("%w: %q uses ** somewhere other than at the end. This build matches "+
			"with path.Match, which has no recursive wildcard — it would read ** as * and match a "+
			"single level while looking like it matched every one. Write the subtree you mean as a "+
			"trailing %q, or a single * for one level", ErrBadPattern, pattern, subtreeSuffix)
	}
	// Validated once, up front, rather than per note: a malformed pattern over an empty note set
	// would otherwise return no error and no notes, which is the empty result this must never be.
	if !subtree {
		if _, err := path.Match(pattern, ""); err != nil {
			return nil, fmt.Errorf("%w: %q is not a valid glob: %w", ErrBadPattern, pattern, err)
		}
	}

	var out []vault.Note
	for _, n := range notes {
		target := n.Vault + "/" + n.Path
		var ok bool
		if subtree {
			ok = target == prefix || strings.HasPrefix(target, prefix+"/")
		} else {
			// The error is already ruled out above; path.Match returns one only for a bad pattern,
			// never for a name it dislikes.
			ok, _ = path.Match(pattern, target)
		}
		if ok {
			out = append(out, n)
		}
	}
	return out, nil
}
