package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// allEnvVars is every variable Load consults, required or not. Tests clear all of them so an
// operator's ambient environment cannot make a case pass or fail by accident.
var allEnvVars = []string{
	"KNOWRAG_CONFIG_FILE",
	"QDRANT_ENDPOINT",
	"QDRANT_API_KEY",
	"EMBEDDER_ENDPOINT",
	"DEFAULT_COLLECTION",
	"LOG_LEVEL",
}

// clearEnv unsets every variable Load reads, restoring the originals when the test ends.
// t.Setenv registers the restore; the immediate Unsetenv is what actually clears the value.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, name := range allEnvVars {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unsetting %s: %v", name, err)
		}
	}
}

// setRequiredEnv sets every required variable to a recognizable value.
func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("QDRANT_ENDPOINT", "qdrant.example:6334")
	t.Setenv("QDRANT_API_KEY", "env-key")
	t.Setenv("EMBEDDER_ENDPOINT", "http://embedder.example:8080")
	t.Setenv("DEFAULT_COLLECTION", "knowrag_interno")
}

func writeConfigFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "knowrag.yml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing config file: %v", err)
	}
	return path
}

func TestLoad_FromFileOnly_ReturnsConfig(t *testing.T) {
	clearEnv(t)
	path := writeConfigFile(t, `qdrant_endpoint: file.example:6334
qdrant_api_key: file-key
embedder_endpoint: http://file-embedder:8080
default_collection: knowrag_file
log_level: debug
`)
	t.Setenv("KNOWRAG_CONFIG_FILE", path)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	want := Config{
		QdrantEndpoint:    "file.example:6334",
		QdrantAPIKey:      "file-key",
		EmbedderEndpoint:  "http://file-embedder:8080",
		DefaultCollection: "knowrag_file",
		LogLevel:          "debug",
	}
	if *cfg != want {
		t.Fatalf("Load() = %+v, want %+v", *cfg, want)
	}
}

func TestLoad_EnvOverridesFile(t *testing.T) {
	clearEnv(t)
	path := writeConfigFile(t, `qdrant_endpoint: file-value
qdrant_api_key: file-key
embedder_endpoint: http://file-embedder:8080
default_collection: knowrag_file
`)
	t.Setenv("KNOWRAG_CONFIG_FILE", path)
	t.Setenv("QDRANT_ENDPOINT", "env-value")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if cfg.QdrantEndpoint != "env-value" {
		t.Fatalf("QdrantEndpoint = %q, want the env value %q", cfg.QdrantEndpoint, "env-value")
	}
	if cfg.QdrantAPIKey != "file-key" {
		t.Fatalf("QdrantAPIKey = %q, want the file value %q — a field set only in the file must survive", cfg.QdrantAPIKey, "file-key")
	}
}

func TestLoad_AllRequiredPresent_ReturnsConfig(t *testing.T) {
	clearEnv(t)
	setRequiredEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error with no config file and all required vars set: %v", err)
	}
	want := Config{
		QdrantEndpoint:    "qdrant.example:6334",
		QdrantAPIKey:      "env-key",
		EmbedderEndpoint:  "http://embedder.example:8080",
		DefaultCollection: "knowrag_interno",
		LogLevel:          "info",
	}
	if *cfg != want {
		t.Fatalf("Load() = %+v, want %+v", *cfg, want)
	}
}

func TestLoad_MissingRequiredVar_ReturnsError(t *testing.T) {
	missing := []string{"QDRANT_ENDPOINT", "QDRANT_API_KEY", "EMBEDDER_ENDPOINT", "DEFAULT_COLLECTION"}

	for _, name := range missing {
		t.Run(name, func(t *testing.T) {
			clearEnv(t)
			setRequiredEnv(t)
			if err := os.Unsetenv(name); err != nil {
				t.Fatalf("unsetting %s: %v", name, err)
			}

			cfg, err := Load()
			if err == nil {
				t.Fatalf("Load() with %s unset = %+v, want an error", name, cfg)
			}
			if !strings.Contains(err.Error(), name) {
				t.Fatalf("Load() error %q does not name the missing variable %s", err, name)
			}
		})
	}
}

func TestLoad_ConfigFile_UnknownKey_ReturnsError(t *testing.T) {
	clearEnv(t)
	path := writeConfigFile(t, `qdrant_endpont: typo.example:6334
qdrant_api_key: file-key
embedder_endpoint: http://file-embedder:8080
default_collection: knowrag_file
`)
	t.Setenv("KNOWRAG_CONFIG_FILE", path)

	cfg, err := Load()
	if err == nil {
		t.Fatalf("Load() with an unrecognized config key = %+v, want an error", cfg)
	}
	if !strings.Contains(err.Error(), "qdrant_endpont") {
		t.Fatalf("Load() error %q does not name the unrecognized key qdrant_endpont", err)
	}
}

func TestLoad_MissingConfigFile_ReturnsError(t *testing.T) {
	clearEnv(t)
	setRequiredEnv(t)
	t.Setenv("KNOWRAG_CONFIG_FILE", filepath.Join(t.TempDir(), "absent.yml"))

	if _, err := Load(); err == nil {
		t.Fatal("Load() with KNOWRAG_CONFIG_FILE pointing at a missing file returned no error")
	}
}

func TestLoad_LogLevelDefaultsToInfo(t *testing.T) {
	clearEnv(t)
	setRequiredEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if cfg.LogLevel != "info" {
		t.Fatalf("LogLevel = %q, want the default %q", cfg.LogLevel, "info")
	}
}

const secret = "super-secret-qdrant-key"

func secretConfig() Config {
	return Config{
		QdrantEndpoint:    "qdrant.example:6334",
		QdrantAPIKey:      secret,
		EmbedderEndpoint:  "http://embedder.example:8080",
		DefaultCollection: "knowrag_interno",
		LogLevel:          "info",
	}
}

// TestConfig_LogValue_RedactsAPIKey covers both ways a Config reaches a logger. The pointer case
// alone is not enough: LogValue used to have a pointer receiver, so logging a Config by value
// silently bypassed it and wrote the key in clear text.
func TestConfig_LogValue_RedactsAPIKey(t *testing.T) {
	cfg := secretConfig()
	cases := map[string]any{
		"value":   cfg,
		"pointer": &cfg,
	}

	for name, logged := range cases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			slog.New(slog.NewJSONHandler(&buf, nil)).Info("startup", "config", logged)

			if strings.Contains(buf.String(), secret) {
				t.Fatalf("log output leaked the API key: %s", buf.String())
			}
			if !strings.Contains(buf.String(), redacted) {
				t.Fatalf("log output %s does not contain the redaction marker %q", buf.String(), redacted)
			}
			// The rest of the config must still be logged — redaction that swallows everything is useless.
			if !strings.Contains(buf.String(), "qdrant.example:6334") {
				t.Fatalf("log output %s dropped the non-secret fields", buf.String())
			}
			var line map[string]any
			if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
				t.Fatalf("log line is not valid JSON: %v", err)
			}
		})
	}
}

// TestConfig_LogValue_NilPointer_DoesNotCrash pins the documented nil behaviour: a value receiver
// cannot branch on nil, so slog.Value.Resolve recovers the dereference panic. The process must
// survive and the line must still be valid JSON.
func TestConfig_LogValue_NilPointer_DoesNotCrash(t *testing.T) {
	var cfg *Config

	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("startup", "config", cfg)

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("logging a nil *Config produced a line that is not valid JSON: %v", err)
	}
}

// TestConfig_String_RedactsAPIKey closes the route that bypasses slog entirely.
func TestConfig_String_RedactsAPIKey(t *testing.T) {
	cfg := secretConfig()
	cases := map[string]any{
		"value":   cfg,
		"pointer": &cfg,
	}

	for name, formatted := range cases {
		t.Run(name, func(t *testing.T) {
			out := fmt.Sprintf("%+v", formatted)
			if strings.Contains(out, secret) {
				t.Fatalf("%%+v leaked the API key: %s", out)
			}
			if !strings.Contains(out, redacted) {
				t.Fatalf("%%+v output %s does not contain the redaction marker %q", out, redacted)
			}
			if !strings.Contains(out, "qdrant.example:6334") {
				t.Fatalf("%%+v output %s dropped the non-secret fields", out)
			}
		})
	}
}

// TestConfig_Redaction_EmptyKeyStaysEmpty keeps "not set" distinguishable from "set but hidden";
// marking an absent key as [REDACTED] would hide a misconfiguration.
func TestConfig_Redaction_EmptyKeyStaysEmpty(t *testing.T) {
	cfg := secretConfig()
	cfg.QdrantAPIKey = ""

	if out := fmt.Sprintf("%v", cfg); strings.Contains(out, redacted) {
		t.Fatalf("an unset API key was reported as redacted: %s", out)
	}
}
