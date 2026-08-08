package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/qdrant/go-client/qdrant"

	"github.com/danielmalka/go-knowrag/internal/schema"
)

// cleanQdrant is a Qdrant with nothing in it. The command's own contract — exit code, report
// output, error propagation — is all that is under test here; what Apply does with the manifest is
// proven in internal/schema.
type cleanQdrant struct{}

func (cleanQdrant) CollectionExists(context.Context, string) (bool, error) { return false, nil }

func (cleanQdrant) CreateCollection(context.Context, *qdrant.CreateCollection) error { return nil }

func (cleanQdrant) GetCollectionInfo(_ context.Context, name string) (*qdrant.CollectionInfo, error) {
	return nil, errors.New("collection does not exist: " + name)
}

func (cleanQdrant) CreateFieldIndex(context.Context, *qdrant.CreateFieldIndexCollection) (*qdrant.UpdateResult, error) {
	return &qdrant.UpdateResult{}, nil
}

// memState keeps the applied-state record in memory so the command test never writes to the repo.
type memState struct{ state schema.AppliedState }

func (m *memState) Load() (schema.AppliedState, error) { return m.state, nil }

func (m *memState) Save(s schema.AppliedState) error { m.state = s; return nil }

func TestSchemaApplyCmd_Success_ExitsZero(t *testing.T) {
	var out bytes.Buffer

	code, err := runSchemaApply(t.Context(), &out, cleanQdrant{}, &memState{})
	if err != nil {
		t.Fatalf("runSchemaApply: %v", err)
	}
	if code != exitOK {
		t.Errorf("exit code = %d, want %d", code, exitOK)
	}

	// One line per collection, naming every collection the manifest manages — a report that only
	// mentioned the populated one would hide a failure to provision the other two.
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	manifest := schema.Manifest()
	if len(lines) != len(manifest) {
		t.Fatalf("report has %d line(s), want %d:\n%s", len(lines), len(manifest), out.String())
	}
	for i, c := range manifest {
		if !strings.Contains(lines[i], c.Name) {
			t.Errorf("report line %d = %q, want it to name %q", i, lines[i], c.Name)
		}
	}
}

func TestSchemaApplyCmd_DriftError_ExitsNonZero(t *testing.T) {
	drifted := schema.Manifest()[0]
	state := &memState{state: schema.AppliedState{Collections: []schema.AppliedCollection{{
		ManagedCollection: drifted.Name,
		EmbeddingModel:    "bge-m3@a-previous-revision",
	}}}}

	var out bytes.Buffer
	code, err := runSchemaApply(t.Context(), &out, cleanQdrant{}, state)

	if code == exitOK {
		t.Errorf("exit code = %d on drift, want non-zero", code)
	}
	if !errors.Is(err, schema.ErrSchemaDrift) {
		t.Fatalf("error = %v, want it to match schema.ErrSchemaDrift", err)
	}
	// The command is an adapter: it must hand the operator the explicit message internal/schema
	// built, not a summary of its own.
	if !strings.Contains(err.Error(), "bge-m3@a-previous-revision") {
		t.Errorf("error %q lost the detail from the drift error", err)
	}
}

func TestNewSchemaCmd_HasApplySubcommand(t *testing.T) {
	cmd := newSchemaCmd(nil)
	if cmd.Use != "schema" {
		t.Errorf("parent command Use = %q, want %q", cmd.Use, "schema")
	}
	for _, sub := range cmd.Commands() {
		if sub.Name() == "apply" {
			if sub.RunE == nil {
				t.Error("schema apply has no RunE")
			}
			return
		}
	}
	t.Errorf("schema has no apply subcommand, only %v", cmd.Commands())
}
