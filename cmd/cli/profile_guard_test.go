package main

import (
	"strings"
	"testing"
)

// The guards that keep the PR 6 cutover true after the next edit. schema.DefaultProfile (BGE-M3) stays
// as the library fallback until the cleanup PR deletes that profile, so an entry point that read it, or
// built an embedder, a manifest or a search config without a profile, would still compile and would
// quietly serve the old model. The entry points are scanned, not the library: the fallback is
// legitimate there and in tests.
//
// They work on OBJECTS, not on names (profile_guard_scan_test.go, over go/types): a package is the one
// at an import PATH whatever alias the file uses, so `modelSchema ".../internal/schema"` is still the
// schema package, and a config variable is the declaration its identifier is bound to, so an inner
// `cfg` that shadows an outer one is a different variable. A dot-import of a watched package is
// refused outright in an entry point, because after it a call has no qualifier left to resolve.

const modulePath = "github.com/danielmalka/go-knowrag"

// libraryDefaults are the calls that read the library's fallback profile, by package import path.
var libraryDefaults = map[string][]string{
	modulePath + "/internal/schema": {"DefaultProfile", "Manifest"},
	modulePath + "/internal/embed":  {"Expected", "DefaultDenseDim", "NewServiceEmbedder"},
}

const retrievalPath = modulePath + "/internal/retrieval"

// entryPointRoots are the directories whose non-test Go files are entry points.
var entryPointRoots = []string{"..", "../../internal/clicmd", "../../internal/eval"}

func TestNoEntryPointReadsTheLibraryDefaultProfile(t *testing.T) {
	var hits []string
	for _, root := range entryPointRoots {
		for _, file := range goFiles(t, root) {
			hits = append(hits, libraryDefaultLeaks(checkFile(t, file, nil))...)
		}
	}
	if len(hits) > 0 {
		t.Errorf("entry points must resolve their profile (schema.ResolveProfile / cfg.Profile) and pass it "+
			"explicitly; these read the library fallback:\n  %s", strings.Join(hits, "\n  "))
	}
}

func TestEveryEntryPointSearchConfigNamesItsProfile(t *testing.T) {
	var hits []string
	sites := 0
	for _, root := range entryPointRoots {
		for _, file := range goFiles(t, root) {
			n, leaks := searchConfigLeaks(checkFile(t, file, nil))
			sites += n
			hits = append(hits, leaks...)
		}
	}
	if len(hits) > 0 {
		t.Errorf("every search config an entry point builds must have its Profile set:\n  %s", strings.Join(hits, "\n  "))
	}
	// Non-vacuity: the scan has to actually find the searchers this repository builds (cmd/cli search,
	// the MCP server, measure-search). A guard that matched nothing would stay green after a rename.
	if sites < 3 {
		t.Errorf("the search-config guard found %d site(s), expected at least 3: it is no longer looking at the code", sites)
	}
}

// --- the guards, over parsed files so a test can feed them any source ------------------------------

func TestGuards_ResolveByImportPathNotByIdentifier(t *testing.T) {
	cases := []struct {
		name, src string
		want      int // leaks libraryDefaultLeaks must report
	}{
		{"plain selector", `package p
import "github.com/danielmalka/go-knowrag/internal/schema"
var _ = schema.DefaultProfile`, 1},
		{"aliased import", `package p
import modelSchema "github.com/danielmalka/go-knowrag/internal/schema"
var _ = modelSchema.DefaultProfile`, 1},
		{"dot import is refused", `package p
import . "github.com/danielmalka/go-knowrag/internal/schema"
var _ = DefaultProfile`, 1},
		{"aliased embed", `package p
import e "github.com/danielmalka/go-knowrag/internal/embed"
var _ = e.NewServiceEmbedder`, 1},
		{"dot import of embed", `package p
import . "github.com/danielmalka/go-knowrag/internal/embed"
var _ = Expected`, 1},
		{"profile-aware calls are fine", `package p
import m "github.com/danielmalka/go-knowrag/internal/schema"
import "github.com/danielmalka/go-knowrag/internal/embed"
var _ = m.ResolveProfile
var _ = m.ManifestFor
var _ = embed.NewServiceEmbedderFor`, 0},
		{"a local named schema that is not the package is not flagged", `package p
type s struct{ DefaultProfile int }
var schema s
var _ = schema.DefaultProfile`, 0},
		{"a different package aliased as schema is not flagged", `package p
import schema "example.com/other/schema"
var _ = schema.DefaultProfile`, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := libraryDefaultLeaks(checkFile(t, "x.go", []byte(c.src))); len(got) != c.want {
				t.Errorf("libraryDefaultLeaks = %v, want %d leak(s)", got, c.want)
			}
		})
	}
}

func TestGuards_SearchConfigMustSetItsProfile(t *testing.T) {
	const head = `package p
import r "github.com/danielmalka/go-knowrag/internal/retrieval"
`
	cases := []struct {
		name, src string
		wantSites int
		wantLeaks int
	}{
		{"assigned then Profile set", head + `func f() { cfg := r.DefaultConfig(); cfg.Profile = nil; _ = cfg }`, 1, 0},
		{"Profile set through a different field is a leak", head + `func f() { cfg := r.DefaultConfig(); cfg.MaxChunksPerUID = 2; _ = cfg }`, 1, 1},
		{"the substring in a comment does not count", head + `func f() { cfg := r.DefaultConfig(); /* cfg.Profile = x */ _ = cfg }`, 1, 1},
		{"Profile set on another variable is a leak", head + `func f() { cfg, other := r.DefaultConfig(), r.DefaultConfig(); other.Profile = nil; _, _ = cfg, other }`, 2, 1},
		{"used inline", head + `func f() { _ = g(r.DefaultConfig()) }; func g(any) int { return 0 }`, 1, 1},
		{"returned directly", head + `func f() any { return r.DefaultConfig() }`, 1, 1},
		{"composite literal without Profile", head + `func f() any { return r.Config{PrefetchMultiplier: 1} }`, 1, 1},
		{"empty composite literal", head + `func f() any { return r.Config{} }`, 1, 1},
		{"composite literal with Profile", head + `func f() any { return r.Config{Profile: nil} }`, 1, 0},
		{"zero value variable left alone", head + `func f() any { var cfg r.Config; return cfg }`, 1, 1},
		{"zero value variable with Profile", head + `func f() any { var cfg r.Config; cfg.Profile = nil; return cfg }`, 1, 0},
		{"package-level default", head + `var cfg = r.DefaultConfig()`, 1, 1},
		{"dot import is refused", `package p
import . "github.com/danielmalka/go-knowrag/internal/retrieval"
func f() any { return DefaultConfig() }`, 0, 1},
		{"unrelated package with the same alias", `package p
import r "example.com/other/retrieval"
func f() any { return r.DefaultConfig() }`, 0, 0},
		// Shadowing: the same NAME is a different VARIABLE in an inner block, and only objects tell.
		{"inner shadow sets Profile, outer never does", head + `func f() any {
	cfg := r.DefaultConfig()
	{
		cfg := r.DefaultConfig()
		cfg.Profile = nil
		_ = cfg
	}
	return cfg
}`, 2, 1},
		{"outer sets Profile, inner shadow never does", head + `func f() any {
	cfg := r.DefaultConfig()
	cfg.Profile = nil
	{
		cfg := r.DefaultConfig()
		_ = cfg
	}
	return cfg
}`, 2, 1},
		{"both set their own Profile", head + `func f() any {
	cfg := r.DefaultConfig()
	cfg.Profile = nil
	{
		cfg := r.DefaultConfig()
		cfg.Profile = nil
		_ = cfg
	}
	return cfg
}`, 2, 0},
		{"a parameter that shadows the config variable does not set it", head + `func f() any {
	cfg := r.DefaultConfig()
	func(cfg r.Config) { cfg.Profile = nil }(r.Config{Profile: nil})
	return cfg
}`, 2, 1},
		{"a closure assigning the captured outer variable does set it", head + `func f() any {
	cfg := r.DefaultConfig()
	func() { cfg.Profile = nil }()
	return cfg
}`, 1, 0},
		{"plain assignment to a declared config variable", head + `func f() any {
	var cfg r.Config
	cfg = r.DefaultConfig()
	cfg.Profile = nil
	return cfg
}`, 2, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sites, leaks := searchConfigLeaks(checkFile(t, "x.go", []byte(c.src)))
			if sites != c.wantSites || len(leaks) != c.wantLeaks {
				t.Errorf("sites=%d leaks=%v, want sites=%d and %d leak(s)", sites, leaks, c.wantSites, c.wantLeaks)
			}
		})
	}
}
