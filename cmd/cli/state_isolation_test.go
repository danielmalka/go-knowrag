package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain points every place the vault-root state could land (KNOWRAG_STATE_DIR,
// XDG_STATE_HOME and the HOME fallback) at a throwaway directory, so no test in this package can
// write into the real state directory of whoever runs the suite.
func TestMain(m *testing.M) {
	sandbox, err := os.MkdirTemp("", "knowrag-cli-test-state-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "creating the test state sandbox:", err)
		os.Exit(1)
	}
	_ = os.Setenv("KNOWRAG_STATE_DIR", filepath.Join(sandbox, "state"))
	_ = os.Setenv("XDG_STATE_HOME", filepath.Join(sandbox, "xdg"))
	_ = os.Setenv("HOME", filepath.Join(sandbox, "home"))
	code := m.Run()
	_ = os.RemoveAll(sandbox)
	os.Exit(code)
}

// TestStateIsolation_ResolvedPathIsInsideTheTempDir fails if the state path a test would use
// resolves outside os.TempDir(), whichever of the three environment sources supplies it.
func TestStateIsolation_ResolvedPathIsInsideTheTempDir(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	inTemp := func(p string) bool {
		abs, err := filepath.Abs(p)
		return err == nil && (abs == tmp || strings.HasPrefix(abs, tmp+string(filepath.Separator)) ||
			strings.HasPrefix(abs, os.TempDir()+string(filepath.Separator)))
	}
	cases := map[string]func(t *testing.T){
		"as configured":    func(t *testing.T) {},
		"no state dir":     func(t *testing.T) { t.Setenv("KNOWRAG_STATE_DIR", "") },
		"no state, no XDG": func(t *testing.T) { t.Setenv("KNOWRAG_STATE_DIR", ""); t.Setenv("XDG_STATE_HOME", "") },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			setup(t)
			p, err := vaultStateFile()
			if err != nil {
				t.Fatal(err)
			}
			if !inTemp(p) {
				t.Fatalf("state file resolves to %s, outside %s: tests would write to the real state directory", p, os.TempDir())
			}
		})
	}
}
