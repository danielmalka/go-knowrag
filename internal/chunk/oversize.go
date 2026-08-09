package chunk

import (
	"fmt"
	"strings"
)

// HardLimitError reports a chunk that no configuration can rescue: it is larger than BGE-M3's
// 8192-token window, so it cannot be embedded at all.
//
// It names the file and the chunk's breadcrumb, for the same reason vault.FrontmatterError names
// both a path and a field: an operator holding this message has to know which of 731 notes to open
// and where in it to look. Failing here is the contract's explicit choice over silent truncation
// (PRD-stories-fundacao §3, S03) — a truncated chunk indexes text that ends mid-thought, and
// nothing downstream can tell it apart from a note that really ends there.
type HardLimitError struct {
	Path       string
	Breadcrumb []string
	Tokens     int
}

func (e *HardLimitError) Error() string {
	where := strings.Join(e.Breadcrumb, breadcrumbSeparator)
	if where == "" {
		where = "(no heading)"
	}
	return fmt.Sprintf(
		"%s: chunk %q is %d tokens, above the model's hard %d-token window; it cannot be embedded and "+
			"is not truncated — split the code fence or table it contains, or the section itself",
		e.Path, where, e.Tokens, HardTokenLimit)
}

// classifyOversize turns a measured token count into the `oversize` payload field (PRD-contrato
// §2.4), or refuses the chunk outright.
//
// One code path, no kind parameter, and that is the design rather than an omission: `oversize` is a
// pure function of the chunk's breadcrumb-inclusive token count and the ceiling. A fence, a table
// and a section of unsplittable prose are the same case here — whatever the pipeline could not cut
// legally and could not fit is flagged, and the flag says exactly that. Branching on kind would be
// two paths to keep in agreement about one rule.
//
// tokens is always the breadcrumb-prefixed count (measureTokens), never the bare body: the
// breadcrumb is embedded too and occupies the same window.
func classifyOversize(path string, breadcrumb []string, tokens int, cfg Config) (bool, error) {
	if tokens > HardTokenLimit {
		return false, &HardLimitError{Path: path, Breadcrumb: breadcrumb, Tokens: tokens}
	}
	return tokens > cfg.CeilingTokens, nil
}
