package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// setInstanceEnv sets this build's instance variables and lets t.Setenv unset them afterwards.
func setInstanceEnv(t *testing.T) {
	t.Helper()
	t.Setenv(envCollection, "interno")
	t.Setenv(envTenantID, "malka")
	t.Setenv(envQdrantEndpoint, "qdrant.internal:6334")
	t.Setenv(envQdrantAPIKey, "runtime-read-key")
	t.Setenv(envEmbedderEndpoint, "http://embedder.internal:8080")
}

// TestLoadConfig_FixedCollectionAndTenant is S08 T1: the scope this instance serves comes from the
// environment the operator wrote, not from anything the client can reach.
func TestLoadConfig_FixedCollectionAndTenant(t *testing.T) {
	setInstanceEnv(t)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() = %v, want nil", err)
	}
	if cfg.Collection != "interno" {
		t.Errorf("Collection = %q, want %q", cfg.Collection, "interno")
	}
	if cfg.TenantID != "malka" {
		t.Errorf("TenantID = %q, want %q", cfg.TenantID, "malka")
	}
}

func TestLoadConfig_MissingTenantID_ReturnsClearError(t *testing.T) {
	setInstanceEnv(t)
	t.Setenv(envTenantID, "")

	_, err := LoadConfig()
	if err == nil {
		t.Fatal("LoadConfig() = nil error with no tenant set, want an error")
	}
	if !strings.Contains(err.Error(), envTenantID) {
		t.Errorf("error %q does not name %s — the operator cannot act on it", err, envTenantID)
	}
}

// TestLoadConfig_QdrantAPIKeyEnvVar_IsDistinctFromAdminKey proves the negative the PRD scope line
// asks for: the administrative credential is not a fallback this process can reach. Setting only
// the admin-style variables still fails, so the key that ingestion uses never lands here by
// accident of a shared name.
func TestLoadConfig_QdrantAPIKeyEnvVar_IsDistinctFromAdminKey(t *testing.T) {
	setInstanceEnv(t)
	t.Setenv(envQdrantAPIKey, "")
	t.Setenv("QDRANT_API_KEY", "admin-key")
	t.Setenv("QDRANT_ADMIN_API_KEY", "admin-key")

	cfg, err := LoadConfig()
	if err == nil {
		t.Fatalf("LoadConfig() succeeded with only an admin key set, yielding %+v — "+
			"the MCP process must never read the administrative credential", cfg)
	}
	if !strings.Contains(err.Error(), envQdrantAPIKey) {
		t.Errorf("error %q does not name %s", err, envQdrantAPIKey)
	}
}

func TestLoadConfig_MissingEverything_NamesEveryVariable(t *testing.T) {
	for _, env := range []string{envCollection, envTenantID, envQdrantEndpoint, envQdrantAPIKey, envEmbedderEndpoint} {
		t.Setenv(env, "")
	}

	_, err := LoadConfig()
	if err == nil {
		t.Fatal("LoadConfig() = nil error with nothing set, want an error")
	}
	for _, env := range []string{envCollection, envTenantID, envQdrantEndpoint, envQdrantAPIKey, envEmbedderEndpoint} {
		if !strings.Contains(err.Error(), env) {
			t.Errorf("error %q does not name %s — an operator would fix one variable per restart", err, env)
		}
	}
}

// TestConfig_LogValue_MasksAPIKey covers the startup log line in main.go, which is the one place
// the whole config is handed to a logger.
func TestConfig_LogValue_MasksAPIKey(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	cfg := Config{Collection: "interno", TenantID: "malka", QdrantAPIKey: "super-secret-key"}
	logger.Info("starting", "config", cfg)

	if strings.Contains(buf.String(), "super-secret-key") {
		t.Errorf("the API key reached the log output:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "interno") {
		t.Errorf("masking swallowed the whole config, leaving nothing diagnosable:\n%s", buf.String())
	}
}
