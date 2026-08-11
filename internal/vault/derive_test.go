package vault

import (
	"errors"
	"testing"
)

func TestDeriveArea_TableDriven(t *testing.T) {
	tests := []struct {
		areas    []string
		relPath  string
		wantArea string
		wantSub  string
	}{
		// The "pessoal" fixture's four areas.
		{lifeAreas(), "00-inbox/captura.md", "00-inbox", ""},
		{lifeAreas(), "mocs/central.md", "mocs", ""},
		{lifeAreas(), "personal/saude/treino.md", "personal", "saude"},
		{lifeAreas(), "research/golang/concurrency.md", "research", "golang"},
		// The "trabalho" fixture's six.
		{wayAreas(), "00-inbox/captura.md", "00-inbox", ""},
		{wayAreas(), "alfa/marketing/post.md", "alfa", "marketing"},
		{wayAreas(), "beta/curriculo.md", "beta", ""},
		{wayAreas(), "gama/produtos/analise.md", "gama", "produtos"},
		{wayAreas(), "infra/deploy/runbook.md", "infra", "deploy"},
		{wayAreas(), "delta/knowrag/plano.md", "delta", "knowrag"},
		// `sub` is the second segment only; deeper nesting does not extend it.
		{lifeAreas(), "research/golang/pprof/heap.md", "research", "golang"},
	}

	for _, tc := range tests {
		t.Run(tc.relPath, func(t *testing.T) {
			area, sub, err := deriveArea(tc.areas, tc.relPath)
			if err != nil {
				t.Fatalf("deriveArea: %v", err)
			}
			if area != tc.wantArea {
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
		areas      []string
		relPath    string
		wantFolder string
	}{
		{"unknown folder", lifeAreas(), "unknown-folder/note.md", "unknown-folder"},
		// PowerAI leaves ingestion through the exclusion list, not through the area set — so if it
		// is ever *not* excluded, it is an error, not a silently accepted area.
		{"excluded-but-not-excluded PowerAI", lifeAreas(), "PowerAI/spec.md", "PowerAI"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			area, sub, err := deriveArea(tc.areas, tc.relPath)

			var areaErr *UnknownAreaError
			if !errors.As(err, &areaErr) {
				t.Fatalf("got %v (%T), want *UnknownAreaError", err, err)
			}
			if areaErr.Path != tc.relPath || areaErr.Folder != tc.wantFolder {
				t.Errorf("error = %+v, want Path=%q Folder=%q", areaErr, tc.relPath, tc.wantFolder)
			}
			if area != "" || sub != "" {
				t.Errorf("got area=%q sub=%q on error, want both empty", area, sub)
			}
		})
	}
}

// TestDeriveArea_AreaSetIsScopedPerVault: `area` is valid per vault, not globally (PRD-contrato
// §2.4b). Each vault's set is passed to its own ScanVault call, and a folder one vault declares is
// still unknown in the other.
//
// This is the property that disappears the moment a caller merges the configured sets into one flat
// slice — every note lands in a valid area, every run reports success, and notes get filed under
// areas their vault never declared. Nothing else in the suite would see it: with one vault the two
// behaviours are identical.
func TestDeriveArea_AreaSetIsScopedPerVault(t *testing.T) {
	// Each row: a folder, the set that must accept it, and the set that must reject it.
	tests := []struct {
		relPath  string
		accepted []string
		rejected []string
	}{
		{"research/note.md", lifeAreas(), wayAreas()},
		{"personal/note.md", lifeAreas(), wayAreas()},
		{"alfa/note.md", wayAreas(), lifeAreas()},
		{"infra/note.md", wayAreas(), lifeAreas()},
	}

	for _, tc := range tests {
		t.Run(tc.relPath, func(t *testing.T) {
			if _, _, err := deriveArea(tc.accepted, tc.relPath); err != nil {
				t.Errorf("the vault that declares this folder rejected it: %v", err)
			}

			_, _, err := deriveArea(tc.rejected, tc.relPath)
			var areaErr *UnknownAreaError
			if !errors.As(err, &areaErr) {
				t.Fatalf("the other vault accepted it (err = %v); `area` must stay scoped per vault", err)
			}
		})
	}
}

// TestDeriveArea_EmptyAreaSet_RejectsEveryNote: an empty set means "this vault declares no area",
// and every note in it is unfilable — not "no restriction configured, accept anything".
//
// The inversion this guards reads like a sensible guard (`if len(areas) == 0 { return ok }`) and
// fails silently: a vault whose AREAS went missing would ingest its entire corpus under whatever
// folder names happen to be on disk, and the run would report success. Both nil and the empty slice
// are covered, because they arrive from different places — a zero-value config field and a
// splitList over an empty string.
func TestDeriveArea_EmptyAreaSet_RejectsEveryNote(t *testing.T) {
	// Paths that are valid under a real area set, so nothing but the empty set can reject them.
	paths := []string{"research/golang/note.md", "mocs/central.md", "00-inbox/captura.md"}

	for _, areas := range [][]string{nil, {}} {
		for _, relPath := range paths {
			area, sub, err := deriveArea(areas, relPath)

			var areaErr *UnknownAreaError
			if !errors.As(err, &areaErr) {
				t.Errorf("deriveArea(%#v, %q) = (%q, %q, %v), want an *UnknownAreaError",
					areas, relPath, area, sub, err)
				continue
			}
			if area != "" || sub != "" {
				t.Errorf("deriveArea(%#v, %q) returned area=%q sub=%q alongside the error",
					areas, relPath, area, sub)
			}
		}
	}
}

// TestDeriveArea_RootLevelNote_ReturnsExplicitError: a note at the vault root has no first-level
// folder to derive from. Defaulting it to a sentinel area would misfile it silently — owner
// decision, 2026-08-08. Root files that are *meant* to be there are excluded by name during the
// walk and never reach here.
func TestDeriveArea_RootLevelNote_ReturnsExplicitError(t *testing.T) {
	_, _, err := deriveArea(lifeAreas(), "readme.md")

	var areaErr *UnknownAreaError
	if !errors.As(err, &areaErr) {
		t.Fatalf("got %v (%T), want *UnknownAreaError", err, err)
	}
	if areaErr.Path != "readme.md" || areaErr.Folder != "" {
		t.Errorf("error = %+v, want Path=%q Folder=%q", areaErr, "readme.md", "")
	}
}

// TestDeriveArea_CaseInsensitiveFolderName: the real vault folders are mixed case on a
// case-insensitive filesystem; the configured area names are lowercase slugs. The lowercase form is
// also what becomes `area`, which is why this behaviour survived D-26 untouched — see deriveArea.
func TestDeriveArea_CaseInsensitiveFolderName(t *testing.T) {
	for _, folder := range []string{"Research", "RESEARCH", "ReSeArCh"} {
		area, _, err := deriveArea(lifeAreas(), folder+"/golang/note.md")
		if err != nil {
			t.Fatalf("deriveArea(%q): %v", folder, err)
		}
		if area != "research" {
			t.Errorf("deriveArea(%q) area = %q, want %q", folder, area, "research")
		}
	}
}
