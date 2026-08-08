package schema

import (
	"slices"
	"strings"
)

// Canonical separators for the hash input (PRD-contrato §2.4). They are control bytes precisely
// because no field value in the contract is expected to contain one, so the joined form stays
// unambiguously decomposable into its parts.
const (
	fieldSeparator = "\x00"
	listSeparator  = "\x1f"
)

// SerializeFields canonically serializes scalar fields for hashing (PRD-contrato §2.4): keys sorted
// lexicographically, "key=value" pairs joined by \x00.
//
// It only orders and joins. Callers pre-format every value: list values through SerializeList,
// dates as RFC 3339 UTC, booleans as "true"/"false". Keeping the formatting at the call site is
// what lets a second implementation in another language reproduce the same bytes from the same
// contract text, which is the whole reason this function exists as a shared primitive rather than
// as private code inside the hasher.
func SerializeFields(fields map[string]string) string {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	pairs := make([]string, len(keys))
	for i, k := range keys {
		pairs[i] = k + "=" + fields[k]
	}
	return strings.Join(pairs, fieldSeparator)
}

// SerializeList canonically serializes a list-typed field value: a sorted copy joined by \x1f.
//
// The copy is deliberate — sorting the caller's slice in place would reorder a Note's Tags as a
// side effect of hashing it, which is the kind of action-at-a-distance that shows up much later as
// an unexplained payload diff.
func SerializeList(items []string) string {
	sorted := slices.Clone(items)
	slices.Sort(sorted)
	return strings.Join(sorted, listSeparator)
}
