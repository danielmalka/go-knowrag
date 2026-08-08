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
}

// field binds one setting to its environment variable, for both the env override pass and the
// required-field check, so the two can never drift apart.
type field struct {
	env      string
	required bool
	ptr      func(*Config) *string
}

var fields = []field{
	{env: "QDRANT_ENDPOINT", required: true, ptr: func(c *Config) *string { return &c.QdrantEndpoint }},
	{env: "QDRANT_API_KEY", required: true, ptr: func(c *Config) *string { return &c.QdrantAPIKey }},
	{env: "EMBEDDER_ENDPOINT", required: true, ptr: func(c *Config) *string { return &c.EmbedderEndpoint }},
	{env: "DEFAULT_COLLECTION", required: true, ptr: func(c *Config) *string { return &c.DefaultCollection }},
	{env: "LOG_LEVEL", required: false, ptr: func(c *Config) *string { return &c.LogLevel }},
}

// Load reads the optional config file named by KNOWRAG_CONFIG_FILE, overlays the environment on
// top of it, and reports every required setting still missing after the merge. It returns an
// error the operator can act on; it never panics.
func Load() (*Config, error) {
	cfg := &Config{LogLevel: "info"}
	if path := os.Getenv(configFileEnv); path != "" {
		if err := loadFile(path, cfg); err != nil {
			return nil, fmt.Errorf("loading config file %s: %w", path, err)
		}
	}
	overrideFromEnv(cfg)
	return cfg, validateRequired(cfg)
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

func validateRequired(cfg *Config) error {
	var missing []string
	for _, f := range fields {
		if f.required && *f.ptr(cfg) == "" {
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
	)
}

// String implements fmt.Stringer so the other route to a leak — fmt.Printf("%+v", cfg), which
// ignores slog entirely — prints the redacted form too. It must never format the struct with %v:
// that would call String on itself forever.
func (c Config) String() string {
	return fmt.Sprintf(
		"config.Config{QdrantEndpoint:%q QdrantAPIKey:%q EmbedderEndpoint:%q DefaultCollection:%q LogLevel:%q}",
		c.QdrantEndpoint, c.redactedKey(), c.EmbedderEndpoint, c.DefaultCollection, c.LogLevel)
}

// redactedKey reports the API key's presence without its value: an empty key stays empty, so an
// operator can still tell "not set" from "set but hidden".
func (c Config) redactedKey() string {
	if c.QdrantAPIKey == "" {
		return ""
	}
	return redacted
}
