package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/config"
)

// setInstanceEnv sets this build's instance variables and lets t.Setenv unset them afterwards.
func setInstanceEnv(t *testing.T) {
	t.Helper()
	t.Setenv(envCollection, "interno")
	t.Setenv(envTenantID, "tenant-a")
	t.Setenv(envQdrantEndpoint, "qdrant.internal:6334")
	t.Setenv(envQdrantAPIKey, "runtime-read-key")
	t.Setenv(envEmbedderEndpoint, "http://embedder.internal:8080")
	t.Setenv(envAreas, "infra,research")
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
	if cfg.TenantID != "tenant-a" {
		t.Errorf("TenantID = %q, want %q", cfg.TenantID, "tenant-a")
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
	t.Setenv(config.AdminQdrantAPIKeyEnv, "admin-key")
	t.Setenv("QDRANT_API_KEY", "admin-key")

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
	for _, env := range []string{envCollection, envTenantID, envQdrantEndpoint, envQdrantAPIKey, envEmbedderEndpoint, envAreas} {
		t.Setenv(env, "")
	}

	_, err := LoadConfig()
	if err == nil {
		t.Fatal("LoadConfig() = nil error with nothing set, want an error")
	}
	for _, env := range []string{envCollection, envTenantID, envQdrantEndpoint, envQdrantAPIKey, envEmbedderEndpoint, envAreas} {
		if !strings.Contains(err.Error(), env) {
			t.Errorf("error %q does not name %s — an operator would fix one variable per restart", err, env)
		}
	}
}

// TestLoadConfig_MissingAreas_NamedAlongsideOtherMissing pins the report shape the requirement asks
// for explicitly: MCP_AREAS is one more entry in the same missing-variable report, not a separate
// error a second restart is needed to discover.
func TestLoadConfig_MissingAreas_NamedAlongsideOtherMissing(t *testing.T) {
	setInstanceEnv(t)
	t.Setenv(envAreas, "")
	t.Setenv(envTenantID, "")

	_, err := LoadConfig()
	if err == nil {
		t.Fatal("LoadConfig() = nil error with MCP_AREAS unset, want an error")
	}
	for _, env := range []string{envAreas, envTenantID} {
		if !strings.Contains(err.Error(), env) {
			t.Errorf("error %q does not name %s", err, env)
		}
	}
}

// TestLoadConfig_AreasBlankAfterTrim_TreatedAsMissing covers the edge the requirement calls out by
// name: a list that is only commas and spaces has no areas in it, and must fail the same way an
// unset variable does rather than starting a server that advertises nothing.
func TestLoadConfig_AreasBlankAfterTrim_TreatedAsMissing(t *testing.T) {
	setInstanceEnv(t)
	t.Setenv(envAreas, " , , ")

	_, err := LoadConfig()
	if err == nil {
		t.Fatal("LoadConfig() with a blank-after-trim MCP_AREAS = nil error, want one")
	}
	if !strings.Contains(err.Error(), envAreas) {
		t.Errorf("error %q does not name %s", err, envAreas)
	}
}

// TestLoadConfig_AreaNotASlug_NamesTheOffendingValue is the validation half: MCP_AREAS goes through
// the same rule KNOWRAG_VAULT_*_AREAS does (config.ValidateSlug), so the server and the ingestor can
// never disagree about what an area name is.
func TestLoadConfig_AreaNotASlug_NamesTheOffendingValue(t *testing.T) {
	for name, areas := range map[string]string{
		"uppercase": "Research",
		"space":     "00 inbox",
		"accent":    "não-aplica",
	} {
		t.Run(name, func(t *testing.T) {
			setInstanceEnv(t)
			t.Setenv(envAreas, "research,"+areas)

			_, err := LoadConfig()
			if err == nil {
				t.Fatalf("LoadConfig() with area %q = nil error, want one", areas)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("%q", areas)) {
				t.Errorf("error %q does not name the offending value %q", err, areas)
			}
		})
	}
}

// TestLoadConfig_Areas_SortedAndDeduplicated pins the property canonicalAreas relies on: it just
// returns cfg.Areas, so the sorting and deduplication have to have already happened here.
func TestLoadConfig_Areas_SortedAndDeduplicated(t *testing.T) {
	setInstanceEnv(t)
	t.Setenv(envAreas, "research, infra, research")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig(): %v", err)
	}
	want := []string{"infra", "research"}
	if len(cfg.Areas) != len(want) || cfg.Areas[0] != want[0] || cfg.Areas[1] != want[1] {
		t.Errorf("Areas = %v, want %v", cfg.Areas, want)
	}
}

// TestConfig_LogValue_MasksAPIKey covers the startup log line in main.go, which is the one place
// the whole config is handed to a logger.
func TestConfig_LogValue_MasksAPIKey(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	cfg := Config{Collection: "interno", TenantID: "tenant-a", QdrantAPIKey: "super-secret-key"}
	logger.Info("starting", "config", cfg)

	if strings.Contains(buf.String(), "super-secret-key") {
		t.Errorf("the API key reached the log output:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "interno") {
		t.Errorf("masking swallowed the whole config, leaving nothing diagnosable:\n%s", buf.String())
	}
}

// TestConfig_RedactsTheKeyOnBothRoutes covers the route this type was missing: it implemented
// LogValue and not String, so fmt printed the credential in clear while the slog test above stayed
// green. internal/store.Config and internal/config.Config both carry the pair; this one did not.
//
// Both value and pointer are exercised because the value receiver is the load-bearing part — a
// pointer receiver would leave a Config formatted by copy unredacted, which is the shape this
// project shipped once (internal/store/client.go).
func TestConfig_RedactsTheKeyOnBothRoutes(t *testing.T) {
	const secret = "super-secret-key"
	cfg := Config{
		Collection: "interno", TenantID: "tenant-a", QdrantEndpoint: "host:6334",
		QdrantAPIKey: secret, EmbedderEndpoint: "http://127.0.0.1:7999", Areas: []string{"a"},
	}

	for _, tc := range []struct {
		name string
		out  string
	}{
		{"value %+v", fmt.Sprintf("%+v", cfg)},
		{"pointer %+v", fmt.Sprintf("%+v", &cfg)},
		// %s is not listed: for a Stringer it is the same route as %v, and staticcheck rejects
		// spelling it out.
		{"value %v", fmt.Sprintf("%v", cfg)},
	} {
		if strings.Contains(tc.out, secret) {
			t.Errorf("%s leaked the API key: %s", tc.name, tc.out)
		}
		if !strings.Contains(tc.out, "[REDACTED]") {
			t.Errorf("%s does not mark the key as redacted: %s", tc.name, tc.out)
		}
	}

	// An unset key must stay distinguishable from a hidden one.
	empty := Config{Collection: "interno"}
	if strings.Contains(fmt.Sprintf("%+v", empty), "[REDACTED]") {
		t.Error("an unset key was rendered as redacted, which hides that it is missing")
	}
}
