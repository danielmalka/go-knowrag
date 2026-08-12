package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// fixedEnvVars is every variable Load consults whose name is known at compile time. The per-vault
// variables are not here on purpose: their names depend on the roster, so they are found by prefix
// in clearEnv instead. It is derived from the same `fields` slice Load reads rather than retyped, so
// a setting added there cannot be forgotten here and start leaking the host's environment into a
// test.
var fixedEnvVars = func() []string {
	out := []string{configFileEnv, vaultsEnv}
	for _, f := range fields {
		out = append(out, f.env)
	}
	return out
}()

// clearEnv unsets every variable Load reads, restoring the originals when the test ends.
// t.Setenv registers the restore; the immediate Unsetenv is what actually clears the value.
//
// The KNOWRAG_VAULT_* sweep is not decoration. Those names are no longer in `fields`, so a list
// derived from `fields` alone would silently stop clearing them: a vault variable set by one test
// would survive into the next, and the failure would land somewhere else entirely with nothing
// pointing back here.
func clearEnv(t *testing.T) {
	t.Helper()
	names := slices.Clone(fixedEnvVars)
	for _, entry := range os.Environ() {
		if name, _, _ := strings.Cut(entry, "="); strings.HasPrefix(name, vaultEnvPrefix) {
			names = append(names, name)
		}
	}
	for _, name := range names {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unsetting %s: %v", name, err)
		}
	}
}

// allNeeds is every Need bit a command can ask Require for. The vaults are not bits any more —
// their names are runtime configuration, so RequireVaults checks them by name.
const allNeeds = NeedQdrant | NeedCollection | NeedEmbedder

// setRequiredEnv sets every variable some command requires to a recognizable value.
func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("QDRANT_ENDPOINT", "qdrant.example:6334")
	t.Setenv("KNOWRAG_ADMIN_QDRANT_API_KEY", "env-key")
	t.Setenv("EMBEDDER_ENDPOINT", "http://embedder.example:8080")
	t.Setenv("DEFAULT_COLLECTION", "knowrag_interno")
	setRequiredVaultEnv(t)
}

// setRequiredVaultEnv rosters two vaults with everything a run needs from them.
func setRequiredVaultEnv(t *testing.T) {
	t.Helper()
	t.Setenv(vaultsEnv, "pessoal,trabalho")
	t.Setenv("KNOWRAG_VAULT_PESSOAL_PATH", "/vaults/Pessoal")
	t.Setenv("KNOWRAG_VAULT_PESSOAL_AREAS", "00-inbox,mocs,personal,research")
	t.Setenv("KNOWRAG_VAULT_TRABALHO_PATH", "/vaults/Trabalho")
	t.Setenv("KNOWRAG_VAULT_TRABALHO_AREAS", "00-inbox,alfa,beta")
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
vaults:
  pessoal:
    path: /vaults/Pessoal
    areas: 00-inbox, mocs
    exclude_folders: PowerAI, resources
    exclude_root_files: AGENTS.md
  trabalho:
    path: /vaults/Trabalho
    areas: 00-inbox
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
		Vaults: map[string]VaultSettings{
			"pessoal": {
				Path:             "/vaults/Pessoal",
				Areas:            "00-inbox, mocs",
				ExcludeFolders:   "PowerAI, resources",
				ExcludeRootFiles: "AGENTS.md",
			},
			"trabalho": {Path: "/vaults/Trabalho", Areas: "00-inbox"},
		},
	}
	if !reflect.DeepEqual(*cfg, want) {
		t.Fatalf("Load() = %+v, want %+v", *cfg, want)
	}
}

// TestLoad_ConfigFile_FixedVaultKey_ReturnsError covers the shape the file used to have. The two
// fixed keys are gone; strict decoding turns a config file still carrying them into a loud error
// instead of a vault that is silently never configured.
func TestLoad_ConfigFile_FixedVaultKey_ReturnsError(t *testing.T) {
	clearEnv(t)
	path := writeConfigFile(t, `qdrant_endpoint: file.example:6334
vault_pessoal:
  path: /vaults/Pessoal
`)
	t.Setenv("KNOWRAG_CONFIG_FILE", path)

	cfg, err := Load()
	if err == nil {
		t.Fatalf("Load() with the removed vault_pessoal key = %+v, want an error", cfg)
	}
	if !strings.Contains(err.Error(), "vault_pessoal") {
		t.Fatalf("Load() error %q does not name the removed key vault_pessoal", err)
	}
}

// TestLoad_VaultRoster_FromEnv pins the whole per-vault env binding, including the <NAME> spelling
// rule: a vault named `my-notes` reads KNOWRAG_VAULT_MY_NOTES_*, and nothing else is a legal
// spelling of it.
func TestLoad_VaultRoster_FromEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv(vaultsEnv, "my-notes, work")
	t.Setenv("KNOWRAG_VAULT_MY_NOTES_PATH", "/vaults/notes")
	t.Setenv("KNOWRAG_VAULT_MY_NOTES_AREAS", "00-inbox,research")
	t.Setenv("KNOWRAG_VAULT_MY_NOTES_EXCLUDE_FOLDERS", "resources")
	t.Setenv("KNOWRAG_VAULT_WORK_PATH", "/vaults/work")
	t.Setenv("KNOWRAG_VAULT_WORK_AREAS", "00-inbox")
	t.Setenv("KNOWRAG_VAULT_WORK_EXCLUDE_ROOT_FILES", "AGENTS.md")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	want := map[string]VaultSettings{
		"my-notes": {Path: "/vaults/notes", Areas: "00-inbox,research", ExcludeFolders: "resources"},
		"work":     {Path: "/vaults/work", Areas: "00-inbox", ExcludeRootFiles: "AGENTS.md"},
	}
	if !reflect.DeepEqual(cfg.Vaults, want) {
		t.Fatalf("Vaults = %+v, want %+v", cfg.Vaults, want)
	}
	if err := cfg.RequireVaults(cfg.VaultNames()...); err != nil {
		t.Fatalf("RequireVaults with both vaults fully configured: %v", err)
	}
	// Sorted, not map order: `--vault both` must scan in the same order on every run.
	if got := cfg.VaultNames(); !slices.Equal(got, []string{"my-notes", "work"}) {
		t.Fatalf("VaultNames() = %q, want them sorted", got)
	}
	if got := cfg.Vaults["my-notes"].AreaNames(); !slices.Equal(got, []string{"00-inbox", "research"}) {
		t.Fatalf("AreaNames() = %q, want the split list", got)
	}
}

// TestLoad_EnvRosterOverridesFile keeps the environment winning over the file for the roster the
// same way it wins for every other setting, while the file's settings for a rostered vault survive.
func TestLoad_EnvRosterOverridesFile(t *testing.T) {
	clearEnv(t)
	path := writeConfigFile(t, `vaults:
  pessoal:
    path: /from/file
    areas: 00-inbox
`)
	t.Setenv("KNOWRAG_CONFIG_FILE", path)
	t.Setenv(vaultsEnv, "pessoal")
	t.Setenv("KNOWRAG_VAULT_PESSOAL_AREAS", "00-inbox,mocs")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	want := VaultSettings{Path: "/from/file", Areas: "00-inbox,mocs"}
	if cfg.Vaults["pessoal"] != want {
		t.Fatalf("Vaults[pessoal] = %+v, want %+v — the file's path must survive, the env areas must win", cfg.Vaults["pessoal"], want)
	}
}

// TestLoad_EmptyEnvRosterClearsTheFile pins the half of "the environment wins" that is easy to lose:
// winning has to include winning with nothing.
//
// An operator clearing a host's roster exports KNOWRAG_VAULTS= and expects no vault to be ingested.
// Read with Getenv and a non-empty test, that is indistinguishable from unset, so the file's roster
// survives and the host keeps ingesting vaults the operator believes they turned off — silently,
// because nothing in the run mentions the config file at all.
func TestLoad_EmptyEnvRosterClearsTheFile(t *testing.T) {
	clearEnv(t)
	path := writeConfigFile(t, `vaults:
  pessoal:
    path: /from/file
    areas: 00-inbox
`)
	t.Setenv("KNOWRAG_CONFIG_FILE", path)
	t.Setenv(vaultsEnv, "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if len(cfg.Vaults) != 0 {
		t.Fatalf("Vaults = %+v, want empty — %s was set to nothing, which is a roster of no vaults, not an unset variable",
			cfg.Vaults, vaultsEnv)
	}
	if err := cfg.RequireVaults(); err == nil {
		t.Error("RequireVaults() on the cleared roster = nil, want the error that names the missing roster")
	}
}

// TestLoad_RejectsNamesThatAreNotSlugs is the point of this validation: the value is written
// verbatim into the payload and into point_hash, so a name that is not a slug is refused at load
// rather than normalised. Normalising it would move every hash, and since the point ID does not
// include `vault`, the old points would be overwritten rather than orphaned — a full reindex
// reported as a clean run.
func TestLoad_RejectsNamesThatAreNotSlugs(t *testing.T) {
	bad := []string{
		"Pessoal",     // uppercase
		"nome vault",  // space
		"nome\tvault", // other whitespace
		"nomê",        // accent
		"nome_vault",  // underscore
		"-nome",       // leading hyphen
		"nome-",       // trailing hyphen
		"nome--vault", // doubled hyphen
	}

	for _, name := range bad {
		t.Run("vault "+name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv(vaultsEnv, name)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() = %v, want no error: a malformed name is refused when a command asks "+
					"for that vault, not when the binary starts", err)
			}
			err = cfg.RequireVaults(name)
			if err == nil {
				t.Fatalf("RequireVaults(%q) = nil, want an error", name)
			}
			assertNamesValueAndFormat(t, err, name)
		})

		t.Run("area "+name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv(vaultsEnv, "pessoal")
			t.Setenv("KNOWRAG_VAULT_PESSOAL_PATH", "/vaults/pessoal")
			t.Setenv("KNOWRAG_VAULT_PESSOAL_AREAS", "00-inbox,"+name)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() = %v, want no error: see the vault subtest", err)
			}
			err = cfg.RequireVaults("pessoal")
			if err == nil {
				t.Fatalf("RequireVaults with area %q = nil, want an error", name)
			}
			assertNamesValueAndFormat(t, err, name)
		})
	}
}

// TestLoad_MalformedVaultDoesNotBreakUnrelatedCommands is the operability half of the rule above,
// and the reason the check moved out of Load at all.
//
// Load runs once in main() before any subcommand exists. While it validated every rostered vault,
// one mistyped area took the whole binary down — including `--help`, and including commands that
// touch no vault, which cmd/cli/ingest.go and cmd/cli/schema.go both state in comments must keep
// working without a valid configuration. An operator with several vaults could lose the CLI over a
// typo in one they were not using, with no way to print the syntax that would fix it.
func TestLoad_MalformedVaultDoesNotBreakUnrelatedCommands(t *testing.T) {
	clearEnv(t)
	t.Setenv(vaultsEnv, "pessoal,trabalho")
	t.Setenv("KNOWRAG_VAULT_PESSOAL_PATH", "/vaults/pessoal")
	t.Setenv("KNOWRAG_VAULT_PESSOAL_AREAS", "00-inbox,research")
	t.Setenv("KNOWRAG_VAULT_TRABALHO_PATH", "/vaults/trabalho")
	t.Setenv("KNOWRAG_VAULT_TRABALHO_AREAS", "Infra") // the typo, in the vault we will not ask for

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v, want no error", err)
	}
	if err := cfg.RequireVaults("pessoal"); err != nil {
		t.Errorf("RequireVaults(\"pessoal\") = %v, want no error — the fault is in another vault", err)
	}
	if err := cfg.RequireVaults("trabalho"); err == nil {
		t.Error("RequireVaults(\"trabalho\") = nil, want the error naming its malformed area")
	}
}

// assertNamesValueAndFormat holds the error to what an operator needs: the offending value, so they
// do not have to guess which of their names is wrong, and the format, so they do not have to guess
// whether the problem was the accent or the space.
func assertNamesValueAndFormat(t *testing.T, err error, value string) {
	t.Helper()
	// Quoted, the way the error prints it: a raw tab in a message an operator has to read tells
	// them nothing, so the value is escaped and the assertion follows it.
	if quoted := fmt.Sprintf("%q", value); !strings.Contains(err.Error(), quoted) {
		t.Errorf("error %q does not name the offending value %s", err, quoted)
	}
	if !strings.Contains(err.Error(), slugRe.String()) {
		t.Errorf("error %q does not state the expected format", err)
	}
}

// TestLoad_SlugsThatMustBeAccepted is the other half, and the one that says this breaks nothing:
// every vault name and area the real installation uses today.
func TestLoad_SlugsThatMustBeAccepted(t *testing.T) {
	clearEnv(t)
	t.Setenv(vaultsEnv, "pessoal,trabalho")
	t.Setenv("KNOWRAG_VAULT_PESSOAL_PATH", "/vaults/Pessoal")
	t.Setenv("KNOWRAG_VAULT_PESSOAL_AREAS", "00-inbox,mocs,personal,research")
	t.Setenv("KNOWRAG_VAULT_TRABALHO_PATH", "/vaults/Trabalho")
	t.Setenv("KNOWRAG_VAULT_TRABALHO_AREAS", "00-inbox,alfa,beta,gama,infra,delta")

	if _, err := Load(); err != nil {
		t.Fatalf("Load() refused a name the real installation already uses: %v", err)
	}
}

// TestLoad_OrphanedVaultSetting_ReturnsError covers the failure that has no symptom: a vault dropped
// from the roster with its variables still exported is never ingested, and its points go stale
// without a single line of output saying so.
func TestLoad_OrphanedVaultSetting_ReturnsError(t *testing.T) {
	t.Run("environment", func(t *testing.T) {
		clearEnv(t)
		t.Setenv(vaultsEnv, "pessoal")
		t.Setenv("KNOWRAG_VAULT_PESSOAL_PATH", "/vaults/Pessoal")
		t.Setenv("KNOWRAG_VAULT_PESSOAL_AREAS", "00-inbox")
		t.Setenv("KNOWRAG_VAULT_TRABALHO_PATH", "/vaults/Trabalho")

		cfg, err := Load()
		if err == nil {
			t.Fatalf("Load() with a vault variable outside the roster = %+v, want an error", cfg)
		}
		for _, want := range []string{"KNOWRAG_VAULT_TRABALHO_PATH", vaultsEnv} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %s", err, want)
			}
		}
	})

	t.Run("config file", func(t *testing.T) {
		clearEnv(t)
		path := writeConfigFile(t, `vaults:
  pessoal:
    path: /vaults/Pessoal
    areas: 00-inbox
  trabalho:
    path: /vaults/Trabalho
    areas: 00-inbox
`)
		t.Setenv("KNOWRAG_CONFIG_FILE", path)
		t.Setenv(vaultsEnv, "pessoal")

		cfg, err := Load()
		if err == nil {
			t.Fatalf("Load() with a config-file vault outside the roster = %+v, want an error", cfg)
		}
		if !strings.Contains(err.Error(), "trabalho") {
			t.Errorf("error %q does not name the dropped vault", err)
		}
	})

	// No roster at all is not an orphan report: every vault variable would look orphaned, and the
	// real fault is the missing roster, which RequireVaults names. This is also what keeps
	// `schema apply` running on a host that has vault variables and no KNOWRAG_VAULTS.
	t.Run("no roster is not an orphan report", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("KNOWRAG_VAULT_TRABALHO_PATH", "/vaults/Trabalho")

		if _, err := Load(); err != nil {
			t.Fatalf("Load() with vault variables and no roster: %v", err)
		}
	})
}

// TestRequireVaults_NoRoster_NamesTheRosterVariable keeps the empty roster from being read as
// "zero vaults, nothing to do" — which exits 0 and looks like a successful run.
func TestRequireVaults_NoRoster_NamesTheRosterVariable(t *testing.T) {
	clearEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	for _, names := range [][]string{nil, {"pessoal"}} {
		err := cfg.RequireVaults(names...)
		if err == nil {
			t.Fatalf("RequireVaults(%q) with no roster = nil, want an error", names)
		}
		if !strings.Contains(err.Error(), vaultsEnv) {
			t.Errorf("error %q does not name %s", err, vaultsEnv)
		}
	}
}

// TestRequireVaults_ReportsEveryMissingSettingAtOnce keeps the property Require exists for: an
// operator setting up a host sees the whole list in one run, not one restart at a time.
func TestRequireVaults_ReportsEveryMissingSettingAtOnce(t *testing.T) {
	clearEnv(t)
	t.Setenv(vaultsEnv, "pessoal,trabalho")
	t.Setenv("KNOWRAG_VAULT_PESSOAL_PATH", "/vaults/Pessoal")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	err = cfg.RequireVaults(cfg.VaultNames()...)
	if err == nil {
		t.Fatal("RequireVaults with two half-configured vaults = nil, want an error")
	}
	for _, want := range []string{
		"KNOWRAG_VAULT_PESSOAL_AREAS", "KNOWRAG_VAULT_TRABALHO_PATH", "KNOWRAG_VAULT_TRABALHO_AREAS",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
	if strings.Contains(err.Error(), "KNOWRAG_VAULT_PESSOAL_PATH") {
		t.Errorf("error %q names KNOWRAG_VAULT_PESSOAL_PATH, which is set", err)
	}
}

// TestRequireVaults_OnlyChecksTheVaultsAsked mirrors Require's per-command rule: `--vault trabalho`
// must not be refused for a vault it was not pointed at.
func TestRequireVaults_OnlyChecksTheVaultsAsked(t *testing.T) {
	clearEnv(t)
	setRequiredVaultEnv(t)
	if err := os.Unsetenv("KNOWRAG_VAULT_PESSOAL_PATH"); err != nil {
		t.Fatalf("unsetting: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if err := cfg.RequireVaults("trabalho"); err != nil {
		t.Fatalf("RequireVaults(trabalho) with trabalho fully configured: %v", err)
	}
	if err := cfg.RequireVaults(cfg.VaultNames()...); err == nil {
		t.Fatal("RequireVaults over the whole roster with pessoal's path missing = nil, want an error")
	}
}

// TestRequireVaults_UnknownVault_NamesTheRoster covers the name a command was pointed at that the
// roster does not have: the fix is the roster, so the message says so.
func TestRequireVaults_UnknownVault_NamesTheRoster(t *testing.T) {
	clearEnv(t)
	setRequiredVaultEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	err = cfg.RequireVaults("ghost")
	if err == nil {
		t.Fatal("RequireVaults(ghost) with ghost outside the roster = nil, want an error")
	}
	for _, want := range []string{"ghost", vaultsEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
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
	if err := cfg.RequireVaults(cfg.VaultNames()...); err != nil {
		t.Fatalf("RequireVaults with every required var set: %v", err)
	}
	want := Config{
		QdrantEndpoint:    "qdrant.example:6334",
		QdrantAPIKey:      "env-key",
		EmbedderEndpoint:  "http://embedder.example:8080",
		DefaultCollection: "knowrag_interno",
		LogLevel:          "info",
		Vaults: map[string]VaultSettings{
			"pessoal":  {Path: "/vaults/Pessoal", Areas: "00-inbox,mocs,personal,research"},
			"trabalho": {Path: "/vaults/Trabalho", Areas: "00-inbox,alfa,beta"},
		},
	}
	if !reflect.DeepEqual(*cfg, want) {
		t.Fatalf("Load() = %+v, want %+v", *cfg, want)
	}
}

// TestRequire_MissingVar_ReturnsError asserts the check that used to live inside Load: every
// setting some command requires, absent, is reported by name. It asks for every Need at once, which
// is the "all of them" case the old all-or-nothing Load covered.
func TestRequire_MissingVar_ReturnsError(t *testing.T) {
	// The per-vault settings are not here: their names depend on the roster, so RequireVaults
	// checks them — see TestRequireVaults_ReportsEveryMissingSettingAtOnce.
	missing := []string{"QDRANT_ENDPOINT", "KNOWRAG_ADMIN_QDRANT_API_KEY", "EMBEDDER_ENDPOINT", "DEFAULT_COLLECTION"}

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

// TestRequire_MissingAdminKey_SaysWhichCredentialItWants covers the one setting whose name is not
// the whole instruction.
//
// This installation has two Qdrant credentials on purpose: an administrative one, which is this
// CLI's, and a scoped runtime one, which is the MCP server's. An operator told only "set
// KNOWRAG_ADMIN_QDRANT_API_KEY" on a host where the other is already exported has every reason to
// paste the one they have — and it would work well enough to be wrong quietly. The message has to
// say which of the two it is asking for.
func TestRequire_MissingAdminKey_SaysWhichCredentialItWants(t *testing.T) {
	clearEnv(t)
	setRequiredEnv(t)
	if err := os.Unsetenv(AdminQdrantAPIKeyEnv); err != nil {
		t.Fatalf("unsetting %s: %v", AdminQdrantAPIKeyEnv, err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	err = cfg.Require(NeedQdrant)
	if err == nil {
		t.Fatalf("Require(NeedQdrant) with %s unset = nil, want an error", AdminQdrantAPIKeyEnv)
	}
	for _, want := range []string{AdminQdrantAPIKeyEnv, "administrative"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not mention %q", err, want)
		}
	}
	// Naming the other credential here would be worse than saying nothing: it is the variable an
	// operator must not reach for, and a message that mentions it reads as permission.
	if strings.Contains(err.Error(), "MCP_QDRANT_API_KEY") {
		t.Errorf("the refusal %q names the MCP server's runtime credential", err)
	}
}

// TestRequire_OnlyChecksWhatTheCommandNeeds is the regression this refactor exists for: `schema
// apply` talks to Qdrant and nothing else, and used to be refused for a missing EMBEDDER_ENDPOINT
// it never reads. Adding the vault paths to a single global list would have made it demand a vault
// too.
func TestRequire_OnlyChecksWhatTheCommandNeeds(t *testing.T) {
	clearEnv(t)
	t.Setenv("QDRANT_ENDPOINT", "qdrant.example:6334")
	t.Setenv("KNOWRAG_ADMIN_QDRANT_API_KEY", "env-key")

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
	err = cfg.Require(NeedEmbedder | NeedCollection)
	if err == nil {
		t.Fatal("Require with nothing set = nil, want an error")
	}
	for _, name := range []string{"EMBEDDER_ENDPOINT", "DEFAULT_COLLECTION"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("Require error %q does not name %s", err, name)
		}
	}
	// The needs not asked for must not appear: a message that lists settings the command does not
	// read is what sent the operator to set EMBEDDER_ENDPOINT for a schema apply.
	if strings.Contains(err.Error(), "QDRANT_ENDPOINT") {
		t.Errorf("Require error %q names QDRANT_ENDPOINT, which NeedEmbedder|NeedCollection does not cover", err)
	}
}

// TestVaultSettings_SplitLists pins the comma-separated encoding the env vars carry, including the
// entries that must be dropped: a trailing comma is a typo, and an exclusion of "" would match
// nothing while looking like it matched something.
func TestVaultSettings_SplitLists(t *testing.T) {
	v := VaultSettings{
		Areas:            " 00-inbox, mocs ,,research ",
		ExcludeFolders:   " PowerAI, resources ,,templates ",
		ExcludeRootFiles: "",
	}

	wantAreas := []string{"00-inbox", "mocs", "research"}
	if got := v.AreaNames(); !slices.Equal(got, wantAreas) {
		t.Errorf("AreaNames() = %q, want %q", got, wantAreas)
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
