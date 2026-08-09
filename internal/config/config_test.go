package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/schema"
)

// allEnvVars is every variable Load consults, required or not. Tests clear all of them so an
// operator's ambient environment cannot make a case pass or fail by accident. It is derived from
// the same `fields` slice Load reads rather than retyped, so a setting added there cannot be
// forgotten here and start leaking the host's environment into a test.
var allEnvVars = func() []string {
	out := []string{configFileEnv}
	for _, f := range fields {
		out = append(out, f.env)
	}
	return out
}()

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

// allNeeds is every Need bit, so a test can ask for "everything a command could require".
const allNeeds = NeedQdrant | NeedCollection | NeedEmbedder | NeedVaultMalkaLife | NeedVaultMalkaWay

// setRequiredEnv sets every variable some command requires to a recognizable value.
func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("QDRANT_ENDPOINT", "qdrant.example:6334")
	t.Setenv("QDRANT_API_KEY", "env-key")
	t.Setenv("EMBEDDER_ENDPOINT", "http://embedder.example:8080")
	t.Setenv("DEFAULT_COLLECTION", "knowrag_interno")
	t.Setenv("KNOWRAG_VAULT_MALKALIFE_PATH", "/vaults/MalkaLife")
	t.Setenv("KNOWRAG_VAULT_MALKAWAY_PATH", "/vaults/MalkaWay")
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
vault_malkalife:
  path: /vaults/MalkaLife
  exclude_folders: PowerAI, resources
  exclude_root_files: AGENTS.md
vault_malkaway:
  path: /vaults/MalkaWay
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
		VaultMalkaLife: VaultSettings{
			Path:             "/vaults/MalkaLife",
			ExcludeFolders:   "PowerAI, resources",
			ExcludeRootFiles: "AGENTS.md",
		},
		VaultMalkaWay: VaultSettings{Path: "/vaults/MalkaWay"},
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
	if err := cfg.Require(allNeeds); err != nil {
		t.Fatalf("Require(allNeeds) with every required var set: %v", err)
	}
	want := Config{
		QdrantEndpoint:    "qdrant.example:6334",
		QdrantAPIKey:      "env-key",
		EmbedderEndpoint:  "http://embedder.example:8080",
		DefaultCollection: "knowrag_interno",
		LogLevel:          "info",
		VaultMalkaLife:    VaultSettings{Path: "/vaults/MalkaLife"},
		VaultMalkaWay:     VaultSettings{Path: "/vaults/MalkaWay"},
	}
	if *cfg != want {
		t.Fatalf("Load() = %+v, want %+v", *cfg, want)
	}
}

// TestRequire_MissingVar_ReturnsError asserts the check that used to live inside Load: every
// setting some command requires, absent, is reported by name. It asks for every Need at once, which
// is the "all of them" case the old all-or-nothing Load covered.
func TestRequire_MissingVar_ReturnsError(t *testing.T) {
	missing := []string{
		"QDRANT_ENDPOINT", "QDRANT_API_KEY", "EMBEDDER_ENDPOINT", "DEFAULT_COLLECTION",
		"KNOWRAG_VAULT_MALKALIFE_PATH", "KNOWRAG_VAULT_MALKAWAY_PATH",
	}

	for _, name := range missing {
		t.Run(name, func(t *testing.T) {
			clearEnv(t)
			setRequiredEnv(t)
			if err := os.Unsetenv(name); err != nil {
				t.Fatalf("unsetting %s: %v", name, err)
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() with %s unset returned an error; presence is Require's job: %v", name, err)
			}
			err = cfg.Require(allNeeds)
			if err == nil {
				t.Fatalf("Require(allNeeds) with %s unset = nil, want an error", name)
			}
			if !strings.Contains(err.Error(), name) {
				t.Fatalf("Require error %q does not name the missing variable %s", err, name)
			}
		})
	}
}

// TestRequire_OnlyChecksWhatTheCommandNeeds is the regression this refactor exists for: `schema
// apply` talks to Qdrant and nothing else, and used to be refused for a missing EMBEDDER_ENDPOINT
// it never reads. Adding the vault paths to a single global list would have made it demand a vault
// too.
func TestRequire_OnlyChecksWhatTheCommandNeeds(t *testing.T) {
	clearEnv(t)
	t.Setenv("QDRANT_ENDPOINT", "qdrant.example:6334")
	t.Setenv("QDRANT_API_KEY", "env-key")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if err := cfg.Require(NeedQdrant); err != nil {
		t.Fatalf("Require(NeedQdrant) with both Qdrant settings present: %v", err)
	}
	if err := cfg.Require(allNeeds); err == nil {
		t.Fatal("Require(allNeeds) with only the Qdrant settings present = nil, want an error")
	}
}

// TestRequire_ReportsEveryMissingSettingAtOnce keeps the operator from discovering the list one
// restart at a time.
func TestRequire_ReportsEveryMissingSettingAtOnce(t *testing.T) {
	clearEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	err = cfg.Require(NeedEmbedder | NeedVaultMalkaWay)
	if err == nil {
		t.Fatal("Require with nothing set = nil, want an error")
	}
	for _, name := range []string{"EMBEDDER_ENDPOINT", "KNOWRAG_VAULT_MALKAWAY_PATH"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("Require error %q does not name %s", err, name)
		}
	}
	// The needs not asked for must not appear: a message that lists settings the command does not
	// read is what sent the operator to set EMBEDDER_ENDPOINT for a schema apply.
	if strings.Contains(err.Error(), "QDRANT_ENDPOINT") {
		t.Errorf("Require error %q names QDRANT_ENDPOINT, which NeedEmbedder|NeedVaultMalkaWay does not cover", err)
	}
}

// TestVaultOf_MapsEachVaultToItsSettings pins the accessor the CLI routes through. An unregistered
// vault must yield a zero Need, so Require is told there is nothing to check rather than silently
// requiring the wrong vault's path.
func TestVaultOf_MapsEachVaultToItsSettings(t *testing.T) {
	cfg := &Config{
		VaultMalkaLife: VaultSettings{Path: "/vaults/MalkaLife"},
		VaultMalkaWay:  VaultSettings{Path: "/vaults/MalkaWay"},
	}

	life, lifeNeed := cfg.VaultOf(schema.VaultMalkaLife())
	if life.Path != "/vaults/MalkaLife" || lifeNeed != NeedVaultMalkaLife {
		t.Errorf("VaultOf(MalkaLife) = %v, %d", life, lifeNeed)
	}
	way, wayNeed := cfg.VaultOf(schema.VaultMalkaWay())
	if way.Path != "/vaults/MalkaWay" || wayNeed != NeedVaultMalkaWay {
		t.Errorf("VaultOf(MalkaWay) = %v, %d", way, wayNeed)
	}
	if unknown, need := cfg.VaultOf(schema.Vault{}); unknown != (VaultSettings{}) || need != 0 {
		t.Errorf("VaultOf(unregistered) = %v, %d, want the zero settings and a zero Need", unknown, need)
	}
}

// TestVaultSettings_SplitLists pins the comma-separated encoding the env vars carry, including the
// entries that must be dropped: a trailing comma is a typo, and an exclusion of "" would match
// nothing while looking like it matched something.
func TestVaultSettings_SplitLists(t *testing.T) {
	v := VaultSettings{
		ExcludeFolders:   " PowerAI, resources ,,templates ",
		ExcludeRootFiles: "",
	}

	wantFolders := []string{"PowerAI", "resources", "templates"}
	if got := v.Folders(); !slices.Equal(got, wantFolders) {
		t.Errorf("Folders() = %q, want %q", got, wantFolders)
	}
	if got := v.RootFiles(); len(got) != 0 {
		t.Errorf("RootFiles() on an empty setting = %q, want nothing", got)
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
