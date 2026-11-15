package chunk

import (
	"errors"
	"strings"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/schema"
)

func TestConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"floor below ceiling", Config{FloorTokens: 256, CeilingTokens: 1024}, false},
		{"floor equal to ceiling", Config{FloorTokens: 512, CeilingTokens: 512}, false},
		{"floor above ceiling", Config{FloorTokens: 1024, CeilingTokens: 256}, true},
		{"zero floor", Config{FloorTokens: 0, CeilingTokens: 1024}, true},
		{"negative floor", Config{FloorTokens: -1, CeilingTokens: 1024}, true},
		{"zero ceiling", Config{FloorTokens: 256, CeilingTokens: 0}, true},
		{"negative ceiling", Config{FloorTokens: 256, CeilingTokens: -1}, true},
		// The hard 8192-token window is the model's, not a preference: a ceiling above it would
		// authorize chunks the embedder cannot accept, so the config is rejected rather than
		// silently clamped at emit time.
		{"ceiling above the model window", Config{FloorTokens: 256, CeilingTokens: hardLimit + 1}, true},
		{"ceiling exactly at the model window", Config{FloorTokens: 256, CeilingTokens: hardLimit}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("Validate() = nil, want an error for %+v", tc.cfg)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate() = %v, want nil for %+v", err, tc.cfg)
			}
		})
	}
}

// hardLimit is the window a Config with no WindowTokens resolves to: the default profile's, which is
// what a caller that never named a profile gets. It is a variable only because it is computed.
var hardLimit = schema.DefaultProfile().WindowTokens()

// TestHardLimit_IsTheProfileWindow pins the default against the model card's 8,192-token window minus
// the prompt reserve, rather than against itself: a chunk above it cannot be embedded at all.
func TestHardLimit_IsTheProfileWindow(t *testing.T) {
	if hardLimit != 8064 {
		t.Errorf("the default window = %d, want 8064 (8192 minus the 128-token prompt reserve)", hardLimit)
	}
}

// TestVersion_IsTheSchemaChunkerVersion is the whole point of chunk.Version existing as an alias:
// S06a hashes the chunker version into point_hash through internal/schema, so a second, drifting
// literal here would let the two disagree about which pipeline produced a point.
func TestVersion_IsTheSchemaChunkerVersion(t *testing.T) {
	if Version != schema.ChunkerVersion {
		t.Errorf("Version = %q, want schema.ChunkerVersion (%q)", Version, schema.ChunkerVersion)
	}
}

// The window a chunk must fit in comes from the model profile. Zero resolves to the default profile's,
// so a Config built without ConfigFor is as strict as one built with it.
func TestConfig_WindowTokens(t *testing.T) {
	p := schema.DefaultProfile()
	cfg := ConfigFor(p, 256, 1024)
	if cfg.WindowTokens != p.WindowTokens() || cfg.WindowTokens != 8064 {
		t.Fatalf("ConfigFor window = %d, want 8064 (8192 minus the 128-token prompt reserve)", cfg.WindowTokens)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a normal config for the profile was refused: %v", err)
	}
	if err := ConfigFor(p, 256, 8064).Validate(); err != nil {
		t.Errorf("a ceiling exactly at the profile window was refused: %v", err)
	}
	if err := ConfigFor(p, 256, 8065).Validate(); err == nil {
		t.Error("a ceiling above the profile window was accepted: the prompt prefix would push it past the model window")
	}
	if err := (Config{FloorTokens: 1, CeilingTokens: 8064}).Validate(); err != nil {
		t.Errorf("a zero WindowTokens must resolve to the profile window: %v", err)
	}
	if err := (Config{FloorTokens: 1, CeilingTokens: 8065}).Validate(); err == nil {
		t.Error("a zero WindowTokens resolved to the full 8192-token model window instead of the profile's")
	}
	if err := (Config{FloorTokens: 1, CeilingTokens: 10, WindowTokens: -1}).Validate(); err == nil {
		t.Error("a negative window was accepted")
	}
}

func TestClassifyOversize_UsesTheConfigWindow(t *testing.T) {
	cfg := Config{FloorTokens: 1, CeilingTokens: 100, WindowTokens: 500}
	if _, err := classifyOversize("n.md", nil, 500, cfg); err != nil {
		t.Errorf("a chunk exactly at the window was refused: %v", err)
	}
	_, err := classifyOversize("n.md", []string{"A"}, 501, cfg)
	var hard *HardLimitError
	if !errors.As(err, &hard) || hard.Window != 500 || !strings.Contains(err.Error(), "500-token window") {
		t.Fatalf("a chunk over the config window = %v, want a HardLimitError naming the 500-token window", err)
	}
	// and a zero window still means the profile's one
	if _, err := classifyOversize("n.md", nil, hardLimit+1, Config{FloorTokens: 1, CeilingTokens: 10}); err == nil {
		t.Error("the default window no longer applies")
	}
}
