package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
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
}

// LoadConfig reads the instance configuration from the environment. It returns an error naming
// every missing variable at once; it never panics and never falls back to a default scope — a
// server that guessed its own tenant would be worse than one that refused to start.
func LoadConfig() (Config, error) {
	cfg := Config{
		Collection:       os.Getenv(envCollection),
		TenantID:         os.Getenv(envTenantID),
		QdrantEndpoint:   os.Getenv(envQdrantEndpoint),
		QdrantAPIKey:     os.Getenv(envQdrantAPIKey),
		EmbedderEndpoint: os.Getenv(envEmbedderEndpoint),
	}

	var missing []string
	for _, f := range []struct {
		env, value string
	}{
		{envCollection, cfg.Collection},
		{envTenantID, cfg.TenantID},
		{envQdrantEndpoint, cfg.QdrantEndpoint},
		{envQdrantAPIKey, cfg.QdrantAPIKey},
		{envEmbedderEndpoint, cfg.EmbedderEndpoint},
	} {
		if strings.TrimSpace(f.value) == "" {
			missing = append(missing, f.env)
		}
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("mcp-server: missing required environment variable(s): %s",
			strings.Join(missing, ", "))
	}
	return cfg, nil
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
	)
}
