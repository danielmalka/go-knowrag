package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"

	"github.com/danielmalka/go-knowrag/internal/config"
)

// The instance's own environment variables. They are deliberately *not* the ones internal/config
// reads (QDRANT_API_KEY and friends): ADR-002 §2.2 fixes collection and tenant per instance, and
// the PRD scope line fixes the Qdrant credential this process uses as the read-scoped runtime key.
// A shared variable name would mean one exported secret serves both the administrative CLI and the
// long-lived MCP process, which is the separation this list exists to make impossible to fumble.
const (
	envCollection     = "MCP_COLLECTION"
	envTenantID       = "MCP_TENANT_ID"
	envQdrantEndpoint = "MCP_QDRANT_ENDPOINT"
	// #nosec G101 -- this is the name of an environment variable, not a credential. The value is
	// read from the environment at startup and never appears in source or in a log line.
	envQdrantAPIKey     = "MCP_QDRANT_API_KEY"
	envEmbedderEndpoint = "MCP_EMBEDDER_ENDPOINT"
	// envAreas is this instance's own area list (D-26): the areas it advertises to a client and
	// accepts as an `area` filter are installation configuration, not a compile-time enum, same as
	// the ingestor's per-vault roster. It does not have to agree with any one vault's roster — an
	// instance can search a collection assembled from several — so it is its own variable rather
	// than a reference to one of KNOWRAG_VAULT_*_AREAS.
	envAreas = "MCP_AREAS"
)

// Config is this server instance's fixed scope plus the endpoints it needs to answer a search.
//
// Collection and TenantID are the whole point. In stdio there is no authenticated connection
// identity to derive them from — the client is the parent of this process and controls everything
// the protocol carries (ADR-002 §2.1) — so they are written by whoever deploys the instance and
// are never reachable from a tool argument, from the handshake, or from the query text. Serving a
// different scope is a second process with a second config, not a parameter.
type Config struct {
	Collection       string
	TenantID         string
	QdrantEndpoint   string
	QdrantAPIKey     string
	EmbedderEndpoint string
	// Areas is what this instance answers `area` filters and its own tool description against.
	// Sorted and deduplicated once here, at load, rather than by every caller of canonicalAreas.
	Areas []string
}

// LoadConfig reads the instance configuration from the environment. It returns an error naming
// every missing variable at once, plus every malformed area at once alongside it; it never panics
// and never falls back to a default scope — a server that guessed its own tenant would be worse
// than one that refused to start.
func LoadConfig() (Config, error) {
	cfg := Config{
		Collection:       os.Getenv(envCollection),
		TenantID:         os.Getenv(envTenantID),
		QdrantEndpoint:   os.Getenv(envQdrantEndpoint),
		QdrantAPIKey:     os.Getenv(envQdrantAPIKey),
		EmbedderEndpoint: os.Getenv(envEmbedderEndpoint),
		Areas:            splitAreas(os.Getenv(envAreas)),
	}

	var missing []string
	for _, f := range []struct {
		env   string
		empty bool
	}{
		{envCollection, strings.TrimSpace(cfg.Collection) == ""},
		{envTenantID, strings.TrimSpace(cfg.TenantID) == ""},
		{envQdrantEndpoint, strings.TrimSpace(cfg.QdrantEndpoint) == ""},
		{envQdrantAPIKey, strings.TrimSpace(cfg.QdrantAPIKey) == ""},
		{envEmbedderEndpoint, strings.TrimSpace(cfg.EmbedderEndpoint) == ""},
		// A list that is empty after the trim is the same as never having set it: the alternative
		// is a server that starts, advertises zero areas, and refuses every filtered search in
		// silence instead of failing at boot where the mistake is cheap to see.
		{envAreas, len(cfg.Areas) == 0},
	} {
		if f.empty {
			missing = append(missing, f.env)
		}
	}

	var errs []error
	if len(missing) > 0 {
		errs = append(errs, fmt.Errorf("mcp-server: missing required environment variable(s): %s",
			strings.Join(missing, ", ")))
	}
	// Validated against the same rule the ingestor's roster uses (config.ValidateSlug), so the two
	// can never disagree about what an area name is — see envAreas.
	for _, a := range cfg.Areas {
		if err := config.ValidateSlug("area", a); err != nil {
			errs = append(errs, fmt.Errorf("mcp-server: %w", err))
		}
	}
	return cfg, errors.Join(errs...)
}

// splitAreas turns "a, b ,, c" into the sorted, deduplicated [a b c]: the same comma-list shape
// KNOWRAG_VAULT_*_AREAS uses, with empty entries dropped rather than kept as a filter that would
// silently match nothing.
func splitAreas(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// LogValue implements slog.LogValuer so logging a Config never prints the Qdrant credential. The
// process logs its own config at startup, which is exactly the line that would otherwise ship the
// key to wherever stderr goes.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("collection", c.Collection),
		slog.String("tenant_id", c.TenantID),
		slog.String("qdrant_endpoint", c.QdrantEndpoint),
		slog.String("qdrant_api_key", "[REDACTED]"),
		slog.String("embedder_endpoint", c.EmbedderEndpoint),
		slog.String("areas", strings.Join(c.Areas, ",")),
	)
}
