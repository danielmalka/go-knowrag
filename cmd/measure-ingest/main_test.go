package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/danielmalka/go-knowrag/internal/ingest"
	"github.com/danielmalka/go-knowrag/internal/measure"
)

// TestRun_NoBackendConfigured_FailsCleanly proves this tool fails with a named-settings message,
// not a panic or a hang, when nothing is configured — the same refusal cmd/cli/ingest.go's runIngest
// gives before it opens a socket.
func TestRun_NoBackendConfigured_FailsCleanly(t *testing.T) {
	for _, v := range []string{
		"QDRANT_ENDPOINT", "KNOWRAG_ADMIN_QDRANT_API_KEY", "EMBEDDER_ENDPOINT",
		"DEFAULT_COLLECTION", "KNOWRAG_VAULTS", "KNOWRAG_CONFIG_FILE",
	} {
		t.Setenv(v, "")
		_ = os.Unsetenv(v)
	}
	var stdout, stderr bytes.Buffer
	code := run(nil, &stdout, &stderr)
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

	phases := measure.IngestPhases{LockAcquire: 10 * time.Millisecond, VaultScan: 2 * time.Second, Orchestrate: 3 * time.Second}
	verdict := measure.EvaluateIngest(phases.Accounted()+50*time.Millisecond, phases, 60*time.Second)
	report := ingest.Report{Mode: "incremental"}

	if err := writeJSONReport(path, verdict, report); err != nil {
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
	if !decoded.Pass {
		t.Errorf("Pass = false, want true for a fast run")
	}
	if decoded.RequirementGate == "" {
		t.Error("RequirementGate is empty — the operator would not see which NFR this checks")
	}
	if decoded.GateSeconds != 60 {
		t.Errorf("GateSeconds = %v, want 60", decoded.GateSeconds)
	}
}

// TestWriteJSONReport_FailingRunIsVisibleInTheFile proves a run with failed notes still writes a
// report that says so — an operator reading only the file must not mistake a partial run for a
// clean pass.
func TestWriteJSONReport_FailingRunIsVisibleInTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	phases := measure.IngestPhases{LockAcquire: time.Millisecond, VaultScan: time.Millisecond, Orchestrate: time.Millisecond}
	verdict := measure.EvaluateIngest(phases.Accounted(), phases, 60*time.Second)
	report := ingest.Report{
		Mode:    "incremental",
		Results: []ingest.NoteResult{{State: ingest.StateFailed, Err: errFake{}}},
	}

	if err := writeJSONReport(path, verdict, report); err != nil {
		t.Fatalf("writeJSONReport: %v", err)
	}
	// #nosec G304 -- path is built from t.TempDir() by this test, not external input.
	data, _ := os.ReadFile(path)
	var decoded reportJSON
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("report is not valid JSON: %v", err)
	}
	if !decoded.NotesFailed {
		t.Error("NotesFailed = false, want true — the report must surface a failed note, not hide it behind a timing verdict")
	}
}

type errFake struct{}

func (errFake) Error() string { return "fake failure" }
