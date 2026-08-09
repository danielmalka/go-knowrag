package security

import "strings"

// tagEscaper rewrites the angle brackets of a marker occurrence so the result is still legible as
// what it was — marking, not silent removal (PRD §2.6) — while no longer being the marker.
var tagEscaper = strings.NewReplacer("<", "&lt;", ">", "&gt;")

// Sanitize neutralizes every occurrence of the envelope tags inside s.
//
// Contract: the output never contains UntrustedContentOpenTag or UntrustedContentCloseTag as a
// substring, no matter how the input nests or repeats them. Nothing else about s is altered — a
// note that says "ignore previous instructions" still says so after sanitizing, because the point
// is to mark hostile content, not to hide it.
//
// The loop is not decoration. A single replacement pass is how strip-once sanitizers get defeated:
// deleting the inner tag of `<untrusted<untrusted_content>_content>` re-forms a real one. The
// escaped form here cannot re-form a tag (it contains no `<`), and each pass strictly reduces the
// number of occurrences, so the loop terminates — it is the cheap proof rather than the argument.
func Sanitize(s string) string {
	for _, tag := range [...]string{UntrustedContentOpenTag, UntrustedContentCloseTag} {
		escaped := tagEscaper.Replace(tag)
		for strings.Contains(s, tag) {
			s = strings.ReplaceAll(s, tag, escaped)
		}
	}
	return s
}

// Frame wraps already-sanitized text in the untrusted-content envelope.
//
// It does not sanitize for you: the caller sanitizes each document-controlled field on its way into
// the block, because a block is assembled from several fields (text, breadcrumb, path) and framing
// the assembled string would be one sanitization pass too late to tell them apart.
func Frame(sanitizedText string) string {
	return UntrustedContentOpenTag + "\n" +
		UntrustedContentWarning + "\n" +
		sanitizedText + "\n" +
		UntrustedContentCloseTag
}
