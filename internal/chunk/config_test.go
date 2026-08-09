package chunk

import (
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
		{"ceiling above the model window", Config{FloorTokens: 256, CeilingTokens: HardTokenLimit + 1}, true},
		{"ceiling exactly at the model window", Config{FloorTokens: 256, CeilingTokens: HardTokenLimit}, false},
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

// TestHardTokenLimit_IsTheModelWindow pins the constant against PRD-contrato §2.3b rather than
// against itself: 8192 is the BGE-M3 window, and a chunk above it cannot be embedded at all.
func TestHardTokenLimit_IsTheModelWindow(t *testing.T) {
	if HardTokenLimit != 8192 {
		t.Errorf("HardTokenLimit = %d, want 8192 (PRD-contrato §2.3b, BGE-M3 window)", HardTokenLimit)
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
