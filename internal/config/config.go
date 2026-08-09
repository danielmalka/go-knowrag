// Package config loads runtime settings from an optional file plus the environment,
// and builds the shared structured logger.
package config

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/danielmalka/go-knowrag/internal/schema"
)

// redacted replaces the API key wherever a Config is logged.
const redacted = "[REDACTED]"

// configFileEnv names the one variable that points at an optional config file.
// There is no default path: an operator who wants file-based config sets this.
const configFileEnv = "KNOWRAG_CONFIG_FILE"

// Config is the settings every entrypoint needs. No value here is ever committed to the repo:
// it comes from the config file or the environment, with the environment winning.
type Config struct {
	QdrantEndpoint    string `yaml:"qdrant_endpoint"`    // QDRANT_ENDPOINT — host:6334
	QdrantAPIKey      string `yaml:"qdrant_api_key"`     // QDRANT_API_KEY
	EmbedderEndpoint  string `yaml:"embedder_endpoint"`  // EMBEDDER_ENDPOINT
	DefaultCollection string `yaml:"default_collection"` // DEFAULT_COLLECTION
	LogLevel          string `yaml:"log_level"`          // LOG_LEVEL, optional, default "info"

	VaultMalkaLife VaultSettings `yaml:"vault_malkalife"` // KNOWRAG_VAULT_MALKALIFE_*
	VaultMalkaWay  VaultSettings `yaml:"vault_malkaway"`  // KNOWRAG_VAULT_MALKAWAY_*
}

// VaultSettings is where one vault lives and what inside it is not indexed.
//
// The exclusions are settings and not a list compiled into internal/vault, because PRD-contrato
// §2.4b makes re-including an excluded folder an operator decision: adding `resources` back to the
// index has to be one line of configuration, not a code change, a build and a deploy. Both lists
// are comma-separated so they fit the same env-var-to-string binding every other setting uses —
// see Folders and RootFiles for the split.
type VaultSettings struct {
	Path string `yaml:"path"`
	// ExcludeFolders names first-level folders skipped in silence, comma-separated.
	ExcludeFolders string `yaml:"exclude_folders"`
	// ExcludeRootFiles names root-level `.md` files skipped in silence, comma-separated.
	ExcludeRootFiles string `yaml:"exclude_root_files"`
}

// Folders returns the excluded folder names, empty entries dropped.
func (v VaultSettings) Folders() []string { return splitList(v.ExcludeFolders) }

// RootFiles returns the excluded root file names, empty entries dropped.
func (v VaultSettings) RootFiles() []string { return splitList(v.ExcludeRootFiles) }

// String renders the settings compactly for the two redaction paths on Config. Nothing here is
// secret — a vault path is not a credential — so nothing is hidden.
func (v VaultSettings) String() string {
	return fmt.Sprintf("{path:%q exclude_folders:%q exclude_root_files:%q}",
		v.Path, v.ExcludeFolders, v.ExcludeRootFiles)
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
	NeedVaultMalkaLife
	NeedVaultMalkaWay
)

// field binds one setting to its environment variable, for both the env override pass and the
// required-field check, so the two can never drift apart.
type field struct {
	env string
	// need is which command group requires this setting. Zero means no command requires it.
	need Need
	ptr  func(*Config) *string
}

var fields = []field{
	{env: "QDRANT_ENDPOINT", need: NeedQdrant, ptr: func(c *Config) *string { return &c.QdrantEndpoint }},
	{env: "QDRANT_API_KEY", need: NeedQdrant, ptr: func(c *Config) *string { return &c.QdrantAPIKey }},
	{env: "EMBEDDER_ENDPOINT", need: NeedEmbedder, ptr: func(c *Config) *string { return &c.EmbedderEndpoint }},
	{env: "DEFAULT_COLLECTION", need: NeedCollection, ptr: func(c *Config) *string { return &c.DefaultCollection }},
	{env: "LOG_LEVEL", ptr: func(c *Config) *string { return &c.LogLevel }},

	{env: "KNOWRAG_VAULT_MALKALIFE_PATH", need: NeedVaultMalkaLife, ptr: func(c *Config) *string { return &c.VaultMalkaLife.Path }},
	// The exclusion lists are read but never required: a vault that excludes nothing is a legal
	// configuration, and refusing to start without them would only teach an operator to set them
	// to a placeholder.
	{env: "KNOWRAG_VAULT_MALKALIFE_EXCLUDE_FOLDERS", ptr: func(c *Config) *string { return &c.VaultMalkaLife.ExcludeFolders }},
	{env: "KNOWRAG_VAULT_MALKALIFE_EXCLUDE_ROOT_FILES", ptr: func(c *Config) *string { return &c.VaultMalkaLife.ExcludeRootFiles }},

	{env: "KNOWRAG_VAULT_MALKAWAY_PATH", need: NeedVaultMalkaWay, ptr: func(c *Config) *string { return &c.VaultMalkaWay.Path }},
	{env: "KNOWRAG_VAULT_MALKAWAY_EXCLUDE_FOLDERS", ptr: func(c *Config) *string { return &c.VaultMalkaWay.ExcludeFolders }},
	{env: "KNOWRAG_VAULT_MALKAWAY_EXCLUDE_ROOT_FILES", ptr: func(c *Config) *string { return &c.VaultMalkaWay.ExcludeRootFiles }},
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
	return cfg, nil
}

// VaultOf returns v's settings and the Need bit that makes its path required. An unregistered vault
// yields the zero settings and a zero Need, which Require reads as "nothing to check".
func (c *Config) VaultOf(v schema.Vault) (VaultSettings, Need) {
	switch v {
	case schema.VaultMalkaLife():
		return c.VaultMalkaLife, NeedVaultMalkaLife
	case schema.VaultMalkaWay():
		return c.VaultMalkaWay, NeedVaultMalkaWay
	}
	return VaultSettings{}, 0
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
			missing = append(missing, f.env)
		}
	}
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
		slog.String("vault_malkalife", c.VaultMalkaLife.String()),
		slog.String("vault_malkaway", c.VaultMalkaWay.String()),
	)
}

// String implements fmt.Stringer so the other route to a leak — fmt.Printf("%+v", cfg), which
// ignores slog entirely — prints the redacted form too. It must never format the struct with %v:
// that would call String on itself forever.
func (c Config) String() string {
	return fmt.Sprintf(
		"config.Config{QdrantEndpoint:%q QdrantAPIKey:%q EmbedderEndpoint:%q DefaultCollection:%q "+
			"LogLevel:%q VaultMalkaLife:%s VaultMalkaWay:%s}",
		c.QdrantEndpoint, c.redactedKey(), c.EmbedderEndpoint, c.DefaultCollection, c.LogLevel,
		c.VaultMalkaLife, c.VaultMalkaWay)
}

// redactedKey reports the API key's presence without its value: an empty key stays empty, so an
// operator can still tell "not set" from "set but hidden".
func (c Config) redactedKey() string {
	if c.QdrantAPIKey == "" {
		return ""
	}
	return redacted
}
