package security_test

import (
	"testing"

	"github.com/danielmalka/go-knowrag/internal/security"
)

// TestValidateRelativePath is S08 T4 step 5. `path` reaches the response from the index payload,
// so it is document-controlled and gets checked like any other untrusted string.
func TestValidateRelativePath(t *testing.T) {
	const root = "/home/user/vault"

	for name, tc := range map[string]struct {
		root    string
		path    string
		wantErr bool
	}{
		"plain relative":              {root, "01-inbox/note.md", false},
		"single segment":              {root, "note.md", false},
		"empty root falls back to .":  {"", "01-inbox/note.md", false},
		"absolute":                    {root, "/etc/passwd", true},
		"absolute inside vault":       {root, root + "/note.md", true},
		"traversal":                   {root, "../../etc/passwd", true},
		"traversal mid-path":          {root, "01-inbox/../../etc/passwd", true},
		"traversal that cleans clean": {root, "../secrets.md", true},
		"single dot-dot":              {root, "..", true},
		"unclean dot prefix":          {root, "./note.md", true},
		"unclean double slash":        {root, "01-inbox//note.md", true},
		"unclean trailing slash":      {root, "01-inbox/", true},
		"empty":                       {root, "", true},
	} {
		err := security.ValidateRelativePath(tc.root, tc.path)
		if tc.wantErr && err == nil {
			t.Errorf("%s: ValidateRelativePath(%q, %q) = nil, want an error", name, tc.root, tc.path)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("%s: ValidateRelativePath(%q, %q) = %v, want nil", name, tc.root, tc.path, err)
		}
	}
}
