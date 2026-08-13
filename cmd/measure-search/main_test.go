package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/danielmalka/go-knowrag/internal/measure"
	"github.com/danielmalka/go-knowrag/internal/retrieval"
)

// TestRun_MissingTenant_RefusedBeforeConfigOrNetwork is the same shape as internal/clicmd/search.go's
// own refusal: a missing --tenant must be caught before this tool touches config.Load or dials
// anything, which is why the test needs no env vars set to pass.
func TestRun_MissingTenant_RefusedBeforeConfigOrNetwork(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--query", "renewal terms"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if stderr.Len() == 0 {
		t.Fatal("expected a message on stderr naming the missing flag")
	}
}

func TestRun_MissingQuery_Refused(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--tenant", "malka"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

// TestRun_NoBackendConfigured_FailsCleanly proves that with the required flags present but no
// QDRANT_ENDPOINT/EMBEDDER_ENDPOINT/DEFAULT_COLLECTION in the environment, this tool fails with a
// named-settings message rather than a panic or a hang trying to dial nothing.
func TestRun_NoBackendConfigured_FailsCleanly(t *testing.T) {
	for _, v := range []string{"QDRANT_ENDPOINT", "KNOWRAG_ADMIN_QDRANT_API_KEY", "EMBEDDER_ENDPOINT", "DEFAULT_COLLECTION", "KNOWRAG_CONFIG_FILE"} {
		t.Setenv(v, "")
		_ = os.Unsetenv(v)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"--tenant", "malka", "--query", "renewal terms"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if stderr.Len() == 0 {
		t.Fatal("expected a message on stderr naming the missing settings")
	}
}

func TestWriteJSONReport_CreatesDirAndValidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "report.json")

	report := measure.EvaluateSearch(fastLegsForTest(30), 3*time.Second, 5*time.Second)
	if err := writeJSONReport(path, report); err != nil {
		t.Fatalf("writeJSONReport: %v", err)
	}

	// #nosec G304 -- path is built from t.TempDir() by this test, not external input.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading report: %v", err)
	}
	var decoded reportJSON
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("report is not valid JSON: %v", err)
	}
	if decoded.N != 30 {
		t.Errorf("N = %d, want 30", decoded.N)
	}
	if !decoded.Pass {
		t.Errorf("Pass = false, want true for a fast run")
	}
	if decoded.RequirementGate == "" {
		t.Error("RequirementGate is empty — the operator would not see which NFR this checks")
	}
}

func fastLegsForTest(n int) []retrieval.Timing {
	legs := make([]retrieval.Timing, n)
	for i := range legs {
		legs[i] = retrieval.Timing{Embed: 20 * time.Millisecond, Qdrant: 15 * time.Millisecond, Overhead: 5 * time.Millisecond, Total: 40 * time.Millisecond}
	}
	return legs
}
