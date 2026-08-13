// Package config loads runtime settings from an optional file plus the environment,
// and builds the shared structured logger.
package config

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// redacted replaces the API key wherever a Config is logged.
const redacted = "[REDACTED]"

// configFileEnv names the one variable that points at an optional config file.
// There is no default path: an operator who wants file-based config sets this.
const configFileEnv = "KNOWRAG_CONFIG_FILE"

// vaultsEnv names the roster: which vaults this installation has. Everything per-vault hangs off it,
// and a run that needs a vault and finds it empty is refused rather than reported as "zero vaults,
// nothing to do" — an exit 0 that looks like success is how a stale index goes unnoticed.
const vaultsEnv = "KNOWRAG_VAULTS"

// vaultEnvPrefix is what every per-vault variable starts with. It is also what the orphan scan
// looks for: any variable with this prefix that no rostered vault can produce is a misconfiguration.
// KNOWRAG_VAULTS itself does not match — it has no trailing underscore.
const vaultEnvPrefix = "KNOWRAG_VAULT_"

// Config is the settings every entrypoint needs. No value here is ever committed to the repo:
// it comes from the config file or the environment, with the environment winning.
type Config struct {
	QdrantEndpoint string `yaml:"qdrant_endpoint"` // QDRANT_ENDPOINT — host:6334
	// QdrantAPIKey is the administrative credential — KNOWRAG_ADMIN_QDRANT_API_KEY. The name says
	// administrative because the CLI is: it accepts any --tenant and any --collection it is given
	// (ADR-002 §2.4), so the key it presents is the operator's, not a scoped runtime one. The MCP
	// server reads MCP_QDRANT_API_KEY instead (cmd/mcp-server/config.go) and this package never
	// looks at that variable — there is no fallback between the two in either direction.
	QdrantAPIKey      string `yaml:"qdrant_api_key"`
	EmbedderEndpoint  string `yaml:"embedder_endpoint"`  // EMBEDDER_ENDPOINT
	DefaultCollection string `yaml:"default_collection"` // DEFAULT_COLLECTION
	LogLevel          string `yaml:"log_level"`          // LOG_LEVEL, optional, default "info"

	// Vaults is the roster and its per-vault settings, keyed by vault name — the installation's
	// vaults, not the domain's. KNOWRAG_VAULTS names the keys; KNOWRAG_VAULT_<NAME>_* fills each
	// one. See vaultEnv for how <NAME> is spelled.
	Vaults map[string]VaultSettings `yaml:"vaults"`
}

// VaultNames returns the rostered vault names, sorted. Every place that iterates the roster goes
// through this: map order is random, and `--vault both` scanning in a different order on each run
// would make an output diff meaningless.
func (c *Config) VaultNames() []string { return slices.Sorted(maps.Keys(c.Vaults)) }

// VaultSettings is where one vault lives and what inside it is not indexed.
//
// The exclusions are settings and not a list compiled into internal/vault, because PRD-contrato
// §2.4b makes re-including an excluded folder an operator decision: adding `resources` back to the
// index has to be one line of configuration, not a code change, a build and a deploy. Both lists
// are comma-separated so they fit the same env-var-to-string binding every other setting uses —
// see Folders and RootFiles for the split.
type VaultSettings struct {
	Path string `yaml:"path"`
	// Areas names the first-level folders that are valid `area` values in this vault,
	// comma-separated. Which areas exist is installation configuration, not contract (D-26).
	Areas string `yaml:"areas"`
	// ExcludeFolders names folders skipped in silence, comma-separated. An entry is either a
	// first-level folder name or a path relative to the vault root (`arcanto/14-internal-work`),
	// which skips that subtree and only it. This package splits and trims; what a path means — case,
	// separator, segment-not-prefix — is vault.Exclusions.Folders in internal/vault/walk.go.
	ExcludeFolders string `yaml:"exclude_folders"`
	// ExcludeRootFiles names root-level `.md` files skipped in silence, comma-separated.
	ExcludeRootFiles string `yaml:"exclude_root_files"`
}

// AreaNames returns the vault's area names, empty entries dropped.
func (v VaultSettings) AreaNames() []string { return splitList(v.Areas) }

// Folders returns the excluded folder names, empty entries dropped.
func (v VaultSettings) Folders() []string { return splitList(v.ExcludeFolders) }

// RootFiles returns the excluded root file names, empty entries dropped.
func (v VaultSettings) RootFiles() []string { return splitList(v.ExcludeRootFiles) }

// String renders the settings compactly for the two redaction paths on Config. Nothing here is
// secret — a vault path is not a credential — so nothing is hidden.
func (v VaultSettings) String() string {
	return fmt.Sprintf("{path:%q areas:%q exclude_folders:%q exclude_root_files:%q}",
		v.Path, v.Areas, v.ExcludeFolders, v.ExcludeRootFiles)
}

// splitList turns "a, b ,, c" into [a b c]. Empty entries are dropped rather than passed on: a
// trailing comma is a typo, and an exclusion entry of "" would match nothing while looking like it
// matched something.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// vaultEnv spells the variable that carries suffix for vault name: the name uppercased with every
// `-` turned into `_`, between the prefix and the suffix. A vault named `my-notes` reads
// KNOWRAG_VAULT_MY_NOTES_PATH.
//
// The mapping is not injective on arbitrary strings — `my-notes` and `my_notes` would collide — and
// it does not have to be: slugRe rejects underscores, so only one of the two spellings is ever a
// legal vault name.
func vaultEnv(name, suffix string) string {
	return vaultEnvPrefix + strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_" + suffix
}

// vaultField binds one per-vault variable to the setting it fills, so the override pass, the
// required-field check and the orphan scan all read the same list.
type vaultField struct {
	suffix string
	// required is whether a rostered vault must have this set. A vault that excludes nothing is a
	// legal configuration; a vault with no path or no areas is not — it cannot be scanned, and
	// "no areas" would make every folder in it unknown.
	required bool
	ptr      func(*VaultSettings) *string
}

var vaultFields = []vaultField{
	{suffix: "PATH", required: true, ptr: func(v *VaultSettings) *string { return &v.Path }},
	{suffix: "AREAS", required: true, ptr: func(v *VaultSettings) *string { return &v.Areas }},
	{suffix: "EXCLUDE_FOLDERS", ptr: func(v *VaultSettings) *string { return &v.ExcludeFolders }},
	{suffix: "EXCLUDE_ROOT_FILES", ptr: func(v *VaultSettings) *string { return &v.ExcludeRootFiles }},
}

// slugRe is the one shape a vault name or an area name may have: ASCII lowercase letters and digits
// in hyphen-separated groups. It rejects uppercase, whitespace, accented characters, underscores,
// leading, trailing and doubled hyphens, and the empty string.
var slugRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// errNotSlug rejects a name; it never normalises one, and that is deliberate.
//
// `vault` and `area` are written into the point payload and into `point_hash` as their literal
// strings. Lowercasing a name behind the operator's back would move every hash, and because the
// point ID is uuid5(tenant_id + uid + chunk_index) — no `vault` in it — the old points would be
// overwritten rather than orphaned. The operator would see a full silent reindex reported as a clean
// run. Refusing to start is the cheap failure; accepting the name and rewriting the corpus is not.
func errNotSlug(kind, value string) error {
	return fmt.Errorf(
		"%s %q is not a valid name — use lowercase ASCII letters and digits in hyphen-separated "+
			"groups (%s): no uppercase, no spaces, no accents, no underscores. The value is stored verbatim "+
			"in the payload and in point_hash, so it is rejected rather than silently normalised",
		kind, value, slugRe.String())
}

// ValidateSlug rejects value unless it has the one shape a vault name or an area name may have —
// see slugRe. It is exported because the vault roster is not the only place a value ends up in a
// payload and in point_hash: cmd/mcp-server publishes its own area list to an MCP client, and if the
// two disagreed on what counts as a name, an area the ingestor accepted could be one the server never
// advertises (or the reverse) — a search that returns nothing, with no error anywhere naming why.
func ValidateSlug(kind, value string) error {
	if !slugRe.MatchString(value) {
		return errNotSlug(kind, value)
	}
	return nil
}

// loadVaults builds the roster, overlays the per-vault environment on it, and refuses every
// misconfiguration it can see before the process does any work.
func loadVaults(cfg *Config) error {
	fromFile := cfg.Vaults
	// LookupEnv, not Getenv-and-test-for-empty: the environment wins over the file the same way it
	// does for every scalar setting, and that has to include being set to nothing. An operator who
	// exports KNOWRAG_VAULTS= to clear a host's roster means it — treating that as "unset" would let
	// the file's roster survive in silence, which is the one behaviour no other setting here has.
	if raw, ok := os.LookupEnv(vaultsEnv); ok {
		roster := splitList(raw)
		cfg.Vaults = make(map[string]VaultSettings, len(roster))
		for _, name := range roster {
			cfg.Vaults[name] = fromFile[name]
		}
	}

	for name, settings := range cfg.Vaults {
		for _, f := range vaultFields {
			if v, ok := os.LookupEnv(vaultEnv(name, f.suffix)); ok {
				*f.ptr(&settings) = v
			}
		}
		cfg.Vaults[name] = settings
	}

	// Slug validation is deliberately *not* here — it lives in RequireVaults, per command. Loading
	// happens once in main() before any subcommand exists, so a malformed area on a vault this run
	// never touches would take the whole CLI down with it, `--help` included, and two comments in
	// cmd/cli say in so many words that the command tree assembles without a valid configuration.
	// Refusing a bad name is right; refusing to print help because some other vault has a typo is
	// the same category error Need/Require was built to avoid.
	//
	// The cost of moving it is real and named: Load is a chokepoint nobody can forget, RequireVaults
	// is not. What stands between a malformed area and point_hash is now RequireVaults alone, and a
	// command that reads Vaults without calling it has no second line of defence.
	//
	// It is worth being exact about that, because the comfortable version of this sentence is wrong.
	// vault.deriveArea lowercases the on-disk folder before comparing, so it does catch an area that
	// differs from the folder only in case — `Research` against `research/`. It catches nothing else:
	// measured, `my area`, `nomê`, `nome_vault`, `-nome`, `nome-` and `a--b` all match a folder of
	// that exact name and are written verbatim into the payload and the hash. Case is the only
	// malformation with a backstop.
	return errors.Join(orphanedVaultSettings(cfg, fromFile)...)
}

// orphanedVaultSettings reports every per-vault setting that belongs to no rostered vault.
//
// Without this, dropping a vault from KNOWRAG_VAULTS while its variables stay exported means that
// vault is simply never ingested: no error, no mention in any output, and its points in the
// collection quietly go stale. The same scan catches a misspelled suffix, which fails the same way.
//
// It is skipped when the roster is empty, because then every vault variable looks orphaned and the
// real fault is the missing roster — which RequireVaults names, for the commands that need one.
// This is also what keeps `schema apply` working on a host that has vault variables and no roster.
func orphanedVaultSettings(cfg *Config, fromFile map[string]VaultSettings) []error {
	if len(cfg.Vaults) == 0 {
		return nil
	}

	known := make(map[string]bool, len(cfg.Vaults)*len(vaultFields))
	for name := range cfg.Vaults {
		for _, f := range vaultFields {
			known[vaultEnv(name, f.suffix)] = true
		}
	}

	var orphans []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, vaultEnvPrefix) && !known[name] {
			orphans = append(orphans, name)
		}
	}
	for name := range fromFile {
		if _, ok := cfg.Vaults[name]; !ok {
			orphans = append(orphans, "vaults."+name+" (config file)")
		}
	}
	if len(orphans) == 0 {
		return nil
	}
	slices.Sort(orphans)
	return []error{fmt.Errorf(
		"config: setting(s) for a vault that is not in the roster: %s — add the vault to %s or remove the setting; "+
			"left as they are, that vault is never ingested and its points go stale in silence",
		strings.Join(orphans, ", "), vaultsEnv)}
}

// RequireVaults reports every setting the named vaults need and do not have.
//
// It is the per-vault half of Require, split out because vault names are runtime configuration: a
// bitmask cannot carry one bit per vault when the set of vaults is not known at compile time. It
// keeps the property Require exists for — one run reports every missing setting at once, so an
// operator setting up a host sees the whole list instead of discovering it one restart at a time.
func (c *Config) RequireVaults(names ...string) error {
	if len(c.Vaults) == 0 {
		return fmt.Errorf(
			"config: no vaults are configured — set %s to the vault names this installation has, "+
				"each with its %s<NAME>_PATH and %s<NAME>_AREAS",
			vaultsEnv, vaultEnvPrefix, vaultEnvPrefix)
	}

	var missing []string
	var malformed []error
	for _, name := range slices.Sorted(slices.Values(names)) {
		// The name before anything derived from it: vaultEnv on a name with a space in it would name
		// a variable nobody can set, so reporting a missing PATH here would send the operator after
		// the wrong fault. A vault whose name is illegal has nothing else worth saying about it.
		if err := ValidateSlug("vault name", name); err != nil {
			malformed = append(malformed, fmt.Errorf("config: %w", err))
			continue
		}
		settings, ok := c.Vaults[name]
		if !ok {
			missing = append(missing, fmt.Sprintf("%s (vault %q is not in it)", vaultsEnv, name))
			continue
		}
		for _, f := range vaultFields {
			if f.required && *f.ptr(&settings) == "" {
				missing = append(missing, vaultEnv(name, f.suffix))
			}
		}
		for _, area := range settings.AreaNames() {
			if err := ValidateSlug("area", area); err != nil {
				malformed = append(malformed, fmt.Errorf("config: vault %q: %w", name, err))
			}
		}
	}
	// Joined, not returned at the first fault: a host with one missing path and one mistyped area
	// should hear both now rather than fix one and rediscover the other on the next run.
	return errors.Join(append(malformed, missingErr(missing))...)
}

// Need is the set of settings one command actually consumes.
//
// It exists because requirement is per command, not global. `schema apply` talks only to Qdrant; it
// used to be refused for a missing EMBEDDER_ENDPOINT it never reads, and adding the vault paths to
// a single global required list would have made it demand a vault too. Each command names what it
// needs and is told about exactly those.
type Need uint

const (
	NeedQdrant Need = 1 << iota
	NeedCollection
	NeedEmbedder
)

// AdminQdrantAPIKeyEnv is the variable that holds the CLI's administrative Qdrant credential.
//
// It is exported because two other places have to name it and must not spell it themselves: the
// root command's help, which declares what this tool is (cmd/cli/main.go), and the test that proves
// no CLI path reads the MCP server's runtime key instead. A literal copied into either of those
// goes stale the day this one changes, and nothing would report it.
const AdminQdrantAPIKeyEnv = "KNOWRAG_ADMIN_QDRANT_API_KEY" // #nosec G101 -- a variable name, not a credential

// field binds one setting to its environment variable, for both the env override pass and the
// required-field check, so the two can never drift apart.
type field struct {
	env string
	// need is which command group requires this setting. Zero means no command requires it.
	need Need
	// note is appended to the variable name in the "you have not set this" message, for the one
	// setting whose name alone does not say which of two credentials it must hold.
	note string
	ptr  func(*Config) *string
}

var fields = []field{
	{env: "QDRANT_ENDPOINT", need: NeedQdrant, ptr: func(c *Config) *string { return &c.QdrantEndpoint }},
	{env: AdminQdrantAPIKeyEnv, need: NeedQdrant, ptr: func(c *Config) *string { return &c.QdrantAPIKey },
		note: "the administrative Qdrant credential this privileged CLI presents; the MCP server's " +
			"runtime key is a different variable and is never read here"},
	{env: "EMBEDDER_ENDPOINT", need: NeedEmbedder, ptr: func(c *Config) *string { return &c.EmbedderEndpoint }},
	{env: "DEFAULT_COLLECTION", need: NeedCollection, ptr: func(c *Config) *string { return &c.DefaultCollection }},
	{env: "LOG_LEVEL", ptr: func(c *Config) *string { return &c.LogLevel }},
}

// Load reads the optional config file named by KNOWRAG_CONFIG_FILE and overlays the environment on
// top of it. It returns an error the operator can act on; it never panics.
//
// It deliberately checks no setting for presence. What is required depends on what is about to run,
// so that check belongs to the command, through Require — see Need.
func Load() (*Config, error) {
	cfg := &Config{LogLevel: "info"}
	if path := os.Getenv(configFileEnv); path != "" {
		if err := loadFile(path, cfg); err != nil {
			return nil, fmt.Errorf("loading config file %s: %w", path, err)
		}
	}
	overrideFromEnv(cfg)
	// The one exception to "Load checks no setting": a vault or area name that is not a slug is
	// refused here, before any command runs, because by the time it reaches ingest it has already
	// been written into a payload and a point_hash. See errNotSlug.
	if err := loadVaults(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// loadFile decodes the YAML file in strict mode, so a typo'd key is an error rather than a
// setting that silently never took effect.
func loadFile(path string, cfg *Config) error {
	// #nosec G304 G703 -- the path comes from KNOWRAG_CONFIG_FILE, set by whoever starts the
	// process; it is operator configuration, not input crossing a trust boundary.
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// overrideFromEnv overwrites a field only when its variable is actually set, so a value present
// only in the config file survives.
func overrideFromEnv(cfg *Config) {
	for _, f := range fields {
		if v, ok := os.LookupEnv(f.env); ok {
			*f.ptr(cfg) = v
		}
	}
}

// Require reports every setting the given needs cover that is still empty after the merge.
//
// It reports all of them at once rather than the first: an operator setting up a new host should
// see the whole list in one run instead of discovering it one restart at a time.
func (c *Config) Require(need Need) error {
	var missing []string
	for _, f := range fields {
		if f.need&need != 0 && *f.ptr(c) == "" {
			name := f.env
			if f.note != "" {
				name += " (" + f.note + ")"
			}
			missing = append(missing, name)
		}
	}
	return missingErr(missing)
}

// missingErr is the one shape a "you have not set this" error takes, shared by Require and
// RequireVaults so the two never drift into two different messages for the same problem.
func missingErr(missing []string) error {
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf(
		"config: missing required setting(s): %s — set each as an environment variable, or as its snake_case key in the file named by %s",
		strings.Join(missing, ", "), configFileEnv)
}

// LogValue implements slog.LogValuer so logging a Config never leaks the API key.
//
// The receiver is a value, not a pointer, and that is the whole point: a value method is in the
// method set of both Config and *Config, so `slog.Info("…", "cfg", cfg)` redacts whether cfg is
// dereferenced or not. With a pointer receiver, logging a Config by value bypassed LogValue
// entirely and printed the key in clear text.
//
// A nil *Config has no nil branch to take here — a value receiver cannot see one. slog.Value.Resolve
// recovers the resulting panic and logs "LogValue panicked" instead, so a nil config is a visible
// bug in the log rather than a crashed process or a leaked key. Both are covered by tests.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("qdrant_endpoint", c.QdrantEndpoint),
		slog.String("qdrant_api_key", c.redactedKey()),
		slog.String("embedder_endpoint", c.EmbedderEndpoint),
		slog.String("default_collection", c.DefaultCollection),
		slog.String("log_level", c.LogLevel),
		slog.String("vaults", c.vaultsString()),
	)
}

// vaultsString renders the roster in name order, so two runs of the same configuration produce the
// same line and a diff of two logs means something.
func (c Config) vaultsString() string {
	var b strings.Builder
	for i, name := range c.VaultNames() {
		if i > 0 {
			b.WriteString(" ")
		}
		fmt.Fprintf(&b, "%s:%s", name, c.Vaults[name])
	}
	return b.String()
}

// String implements fmt.Stringer so the other route to a leak — fmt.Printf("%+v", cfg), which
// ignores slog entirely — prints the redacted form too. It must never format the struct with %v:
// that would call String on itself forever.
func (c Config) String() string {
	return fmt.Sprintf(
		"config.Config{QdrantEndpoint:%q QdrantAPIKey:%q EmbedderEndpoint:%q DefaultCollection:%q "+
			"LogLevel:%q Vaults:{%s}}",
		c.QdrantEndpoint, c.redactedKey(), c.EmbedderEndpoint, c.DefaultCollection, c.LogLevel,
		c.vaultsString())
}

// redactedKey reports the API key's presence without its value: an empty key stays empty, so an
// operator can still tell "not set" from "set but hidden".
func (c Config) redactedKey() string {
	if c.QdrantAPIKey == "" {
		return ""
	}
	return redacted
}
