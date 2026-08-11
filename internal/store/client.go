package store

import (
	"fmt"
	"log/slog"
	"net"
	"strings"

	"github.com/qdrant/go-client/qdrant"
)

// redacted replaces the API key wherever a Config is logged or formatted. Same marker as
// config.Config's, so an operator greps for one string across both.
const redacted = "[REDACTED]"

// grpcPort is Qdrant's gRPC port, and it is a constant rather than a setting on purpose: this
// codebase speaks gRPC only (PRD-contrato §2.3b), so a configurable port would exist solely to let
// someone point it at the REST port and get a connection that never works.
const grpcPort = 6334

// Config is what building a Qdrant client needs. It is a separate type from config.Config so that
// this package stays callable from a test with two literal fields instead of a whole runtime
// configuration.
type Config struct {
	// Endpoint is the Qdrant host, with or without an explicit :6334 suffix. Any other port is
	// rejected — see grpcHost.
	Endpoint string
	APIKey   string
}

// LogValue and String mirror config.Config's redaction, deliberately down to the value receiver.
//
// A value receiver is the load-bearing part, not a style choice: a value method is in the method
// set of both Config and *Config, so a Config logged by copy is redacted too. This project has
// already shipped the pointer-receiver version of exactly this once, and logging the struct by
// value walked straight past it and printed the key in clear text.
//
// Both routes are covered because both are idiomatic: slog.Error("…", "cfg", cfg) while debugging
// a connection, and fmt.Printf("%+v", cfg), which ignores slog entirely. The handler is JSON
// (internal/config/logging.go), so either one reaches aggregated logs.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("endpoint", c.Endpoint),
		slog.String("api_key", c.redactedKey()),
	)
}

// String must never format the struct with %v: that would call String on itself forever.
func (c Config) String() string {
	return fmt.Sprintf("store.Config{Endpoint:%q APIKey:%q}", c.Endpoint, c.redactedKey())
}

// redactedKey reports the key's presence without its value, so "not set" stays distinguishable
// from "set but hidden".
func (c Config) redactedKey() string {
	if c.APIKey == "" {
		return ""
	}
	return redacted
}

// NewQdrantClient builds the Qdrant client every other package talks through.
//
// It returns *qdrant.Client rather than a wrapper because that type already owns the lifecycle a
// caller needs: its Close() tears down every pooled grpc.ClientConn and is guarded by a sync.Once,
// so `defer client.Close()` next to an explicit close is safe. Wrapping it would add a delegation
// layer whose only job is to forward calls this package does not otherwise touch.
//
// The connection is not dialed here — gRPC connects lazily on the first RPC — so an unreachable
// endpoint surfaces at the first call, with that call's context deciding how long to wait, rather
// than as a stall inside construction.
func NewQdrantClient(cfg Config) (*qdrant.Client, error) {
	host, err := grpcHost(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	if cfg.APIKey == "" {
		// No variable is named here, and that is the correction rather than an omission: two
		// entrypoints build this Config from two different credentials — cmd/cli from the
		// administrative key, cmd/mcp-server from its scoped runtime one — so any single variable
		// this message named would be the wrong one for one of them. Each entrypoint's own
		// requirement check names its own variable before it ever reaches this constructor
		// (config.Require for the CLI, LoadConfig for the server).
		return nil, fmt.Errorf("store: the Qdrant API key in this Config is empty")
	}

	return qdrant.NewClient(&qdrant.Config{
		Host:   host,
		Port:   grpcPort,
		APIKey: cfg.APIKey,
		// One connection: every caller in this repo is a one-shot command or a single-threaded
		// server loop, so the default pool of three would open two sockets nothing ever uses.
		PoolSize: 1,
		// The client's own version probe is skipped because it only logs a warning, costs an RPC
		// with a one-minute timeout before construction returns, and duplicates a check this
		// project already makes statically: client and server versions are pinned as a pair in
		// go.mod (see the qdrant/go-client require block).
		SkipCompatibilityCheck: true,
	})
}

// grpcHost validates the configured endpoint and returns the bare host.
//
// It accepts both spellings the project uses — "host" and the "host:6334" documented on
// config.Config — and rejects any other port instead of silently substituting 6334, because an
// endpoint naming 6333 means the operator believes REST is in play and that belief needs
// correcting, not overriding.
func grpcHost(endpoint string) (string, error) {
	if endpoint == "" {
		return "", fmt.Errorf("store: no Qdrant endpoint — set QDRANT_ENDPOINT")
	}
	if !strings.Contains(endpoint, ":") {
		return endpoint, nil
	}

	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return "", fmt.Errorf("store: QDRANT_ENDPOINT %q is not a host or host:%d: %w", endpoint, grpcPort, err)
	}
	if port != fmt.Sprint(grpcPort) {
		return "", fmt.Errorf(
			"store: QDRANT_ENDPOINT %q names port %s, but this client speaks gRPC on %d only "+
				"(REST is not used) — drop the port or use :%d",
			endpoint, port, grpcPort, grpcPort)
	}
	if host == "" {
		return "", fmt.Errorf("store: QDRANT_ENDPOINT %q has no host part", endpoint)
	}
	return host, nil
}
