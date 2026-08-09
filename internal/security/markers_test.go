package security_test

import (
	"strings"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/security"
)

// TestUntrustedContentMarkers_NonEmptyAndDistinct is S08 T4 step 1. It is a shape check, not a
// text check: an empty constant would make Frame produce an envelope with no envelope, and an open
// tag equal to the close tag would make the envelope unparseable by the reader it exists for.
func TestUntrustedContentMarkers_NonEmptyAndDistinct(t *testing.T) {
	for name, got := range map[string]string{
		"UntrustedContentOpenTag":  security.UntrustedContentOpenTag,
		"UntrustedContentCloseTag": security.UntrustedContentCloseTag,
		"UntrustedContentWarning":  security.UntrustedContentWarning,
	} {
		if strings.TrimSpace(got) == "" {
			t.Errorf("%s is empty", name)
		}
	}
	if security.UntrustedContentOpenTag == security.UntrustedContentCloseTag {
		t.Errorf("open and close tags are the same string %q — the envelope has no end",
			security.UntrustedContentOpenTag)
	}
}
