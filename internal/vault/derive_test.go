package vault

import (
	"errors"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/schema"
)

func TestDeriveArea_TableDriven(t *testing.T) {
	tests := []struct {
		vault    schema.Vault
		relPath  string
		wantArea string
		wantSub  string
	}{
		// MalkaLife: the four areas of PRD-contrato §2.4b.
		{schema.VaultMalkaLife(), "00-inbox/captura.md", "00-inbox", ""},
		{schema.VaultMalkaLife(), "mocs/central.md", "mocs", ""},
		{schema.VaultMalkaLife(), "personal/saude/treino.md", "personal", "saude"},
		{schema.VaultMalkaLife(), "research/golang/concurrency.md", "research", "golang"},
		// MalkaWay: the six areas of PRD-contrato §2.4b.
		{schema.VaultMalkaWay(), "00-inbox/captura.md", "00-inbox", ""},
		{schema.VaultMalkaWay(), "arcanto/marketing/post.md", "arcanto", "marketing"},
		{schema.VaultMalkaWay(), "carreira/curriculo.md", "carreira", ""},
		{schema.VaultMalkaWay(), "cooperativa/produtos/analise.md", "cooperativa", "produtos"},
		{schema.VaultMalkaWay(), "infra/deploy/runbook.md", "infra", "deploy"},
		{schema.VaultMalkaWay(), "projetos-pessoais/knowrag/plano.md", "projetos-pessoais", "knowrag"},
		// `sub` is the second segment only; deeper nesting does not extend it.
		{schema.VaultMalkaLife(), "research/golang/pprof/heap.md", "research", "golang"},
	}

	for _, tc := range tests {
		t.Run(tc.vault.String()+"/"+tc.relPath, func(t *testing.T) {
			area, sub, err := deriveArea(tc.vault, tc.relPath)
			if err != nil {
				t.Fatalf("deriveArea: %v", err)
			}
			if area.String() != tc.wantArea {
				t.Errorf("area = %q, want %q", area, tc.wantArea)
			}
			if sub != tc.wantSub {
				t.Errorf("sub = %q, want %q", sub, tc.wantSub)
			}
		})
	}
}

func TestDeriveArea_UnknownFolder_ReturnsExplicitError(t *testing.T) {
	tests := []struct {
		name       string
		vault      schema.Vault
		relPath    string
		wantFolder string
	}{
		{"unknown folder", schema.VaultMalkaLife(), "unknown-folder/note.md", "unknown-folder"},
		// `area` is scoped per vault: a folder valid in one vault is undefined in the other.
		{"other vault's area", schema.VaultMalkaLife(), "arcanto/note.md", "arcanto"},
		{"other vault's area, reversed", schema.VaultMalkaWay(), "research/note.md", "research"},
		// PowerAI leaves ingestion through the exclusion list, not through the area map — so if it
		// is ever *not* excluded, it is an error, not a silently accepted area.
		{"excluded-but-not-excluded PowerAI", schema.VaultMalkaLife(), "PowerAI/spec.md", "PowerAI"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			area, sub, err := deriveArea(tc.vault, tc.relPath)

			var areaErr *UnknownAreaError
			if !errors.As(err, &areaErr) {
				t.Fatalf("got %v (%T), want *UnknownAreaError", err, err)
			}
			if areaErr.Path != tc.relPath || areaErr.Folder != tc.wantFolder {
				t.Errorf("error = %+v, want Path=%q Folder=%q", areaErr, tc.relPath, tc.wantFolder)
			}
			if area.String() != "" || sub != "" {
				t.Errorf("got area=%q sub=%q on error, want both empty", area, sub)
			}
		})
	}
}

// TestDeriveArea_RootLevelNote_ReturnsExplicitError: a note at the vault root has no first-level
// folder to derive from. Defaulting it to a sentinel area would misfile it silently — owner
// decision, 2026-08-08. Root files that are *meant* to be there are excluded by name during the
// walk and never reach here.
func TestDeriveArea_RootLevelNote_ReturnsExplicitError(t *testing.T) {
	_, _, err := deriveArea(schema.VaultMalkaLife(), "readme.md")

	var areaErr *UnknownAreaError
	if !errors.As(err, &areaErr) {
		t.Fatalf("got %v (%T), want *UnknownAreaError", err, err)
	}
	if areaErr.Path != "readme.md" || areaErr.Folder != "" {
		t.Errorf("error = %+v, want Path=%q Folder=%q", areaErr, "readme.md", "")
	}
}

// TestDeriveArea_CaseInsensitiveFolderName: the real vault folders are mixed case on a
// case-insensitive filesystem; the contract's map is lowercase.
func TestDeriveArea_CaseInsensitiveFolderName(t *testing.T) {
	for _, folder := range []string{"Research", "RESEARCH", "ReSeArCh"} {
		area, _, err := deriveArea(schema.VaultMalkaLife(), folder+"/golang/note.md")
		if err != nil {
			t.Fatalf("deriveArea(%q): %v", folder, err)
		}
		if area.String() != "research" {
			t.Errorf("deriveArea(%q) area = %q, want %q", folder, area, "research")
		}
	}
}
