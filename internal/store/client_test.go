package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

const testAPIKey = "s3cr3t-not-a-real-key"

func TestNewQdrantClient_EmptyHost_ReturnsError(t *testing.T) {
	client, err := NewQdrantClient(Config{})
	if client != nil {
		t.Errorf("client = %v, want nil alongside the error", client)
	}
	if err == nil {
		t.Fatal("NewQdrantClient(Config{}) = nil error, want an error naming the missing endpoint")
	}
	if !strings.Contains(err.Error(), "QDRANT_ENDPOINT") {
		t.Errorf("error %q does not name the missing setting (QDRANT_ENDPOINT)", err)
	}
}

func TestNewQdrantClient_EmptyAPIKey_ReturnsError(t *testing.T) {
	client, err := NewQdrantClient(Config{Endpoint: "qdrant.example.com"})
	if client != nil {
		t.Errorf("client = %v, want nil alongside the error", client)
	}
	if err == nil {
		t.Fatal("missing API key returned a nil error, want an error saying the key is empty")
	}
	if !strings.Contains(err.Error(), "API key") {
		t.Errorf("error %q does not say which field is empty", err)
	}
	// The absent half is the assertion that matters, and it is why this test no longer looks for a
	// variable name. Two entrypoints fill Config.APIKey from two different credentials, so a
	// message naming either one is a message that misdirects the operator of the other. Each
	// entrypoint names its own before it builds a Config (config.Require, mcp-server's LoadConfig).
	for _, env := range []string{"KNOWRAG_ADMIN_QDRANT_API_KEY", "MCP_QDRANT_API_KEY", "QDRANT_API_KEY"} {
		if strings.Contains(err.Error(), env) {
			t.Errorf("error %q names %s, but this constructor serves both entrypoints and cannot "+
				"know which variable filled the Config it was handed", err, env)
		}
	}
}

// TestNewQdrantClient_RejectsNonGRPCPort covers the rule that only the host is configurable: the
// protocol and port are fixed (gRPC 6334, PRD-contrato §2.3b). An endpoint pointing at Qdrant's
// REST port is a configuration mistake that must be named, not quietly rewritten to 6334 — the
// operator who typed 6333 believes something about this system that is false.
func TestNewQdrantClient_RejectsNonGRPCPort(t *testing.T) {
	for _, endpoint := range []string{"qdrant.example.com:6333", "qdrant.example.com:80"} {
		t.Run(endpoint, func(t *testing.T) {
			client, err := NewQdrantClient(Config{Endpoint: endpoint, APIKey: testAPIKey})
			if client != nil {
				t.Errorf("client = %v, want nil alongside the error", client)
			}
			if err == nil {
				t.Fatalf("NewQdrantClient(%q) = nil error, want a rejected-port error", endpoint)
			}
			if !strings.Contains(err.Error(), "6334") {
				t.Errorf("error %q does not name the only supported port (6334)", err)
			}
		})
	}
}

// TestNewQdrantClient_AcceptsExplicitGRPCPort exists because config.Config documents
// QDRANT_ENDPOINT as "host:6334": the spelling the rest of the project uses must not be rejected
// by the check above.
func TestNewQdrantClient_AcceptsExplicitGRPCPort(t *testing.T) {
	client, err := NewQdrantClient(Config{Endpoint: "qdrant.example.com:6334", APIKey: testAPIKey})
	if err != nil {
		t.Fatalf("NewQdrantClient with an explicit :6334 endpoint: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
}

// TestNewQdrantClient_Error_DoesNotLeakAPIKey pins the same rule config.Config enforces for
// logging: the key must not travel inside an error either, since errors are printed and shipped to
// logs by every caller in this repo.
func TestNewQdrantClient_Error_DoesNotLeakAPIKey(t *testing.T) {
	_, err := NewQdrantClient(Config{Endpoint: "qdrant.example.com:6333", APIKey: testAPIKey})
	if err == nil {
		t.Fatal("want an error to inspect")
	}
	if strings.Contains(err.Error(), testAPIKey) {
		t.Errorf("error %q contains the API key in clear text", err)
	}
}

// TestNewQdrantClient_Close_ReturnsNoError is what lets the integration tests build a client per
// test: the returned value owns its grpc.ClientConn, and closing it releases the socket. The
// endpoint is never dialed — gRPC connects lazily on the first RPC — so this stays a unit test.
func TestNewQdrantClient_Close_ReturnsNoError(t *testing.T) {
	client, err := NewQdrantClient(Config{Endpoint: "127.0.0.1", APIKey: testAPIKey})
	if err != nil {
		t.Fatalf("NewQdrantClient: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Errorf("first Close() = %v, want nil", err)
	}
	// A second Close is what a `defer client.Close()` next to an explicit close produces. It must
	// not report a failure that would fail a caller's error check.
	if err := client.Close(); err != nil {
		t.Errorf("second Close() = %v, want nil", err)
	}
}

// secretConfig is the config the redaction tests log and format.
func secretConfig() Config {
	return Config{Endpoint: "qdrant.example:6334", APIKey: testAPIKey}
}

// TestConfig_LogValue_RedactsAPIKey covers both ways a Config reaches a logger. The value case is
// not redundant with the pointer one: config.Config once had a pointer-receiver LogValue, and
// logging that struct by value bypassed it and wrote the key in clear text. Same type shape here,
// same trap, so the same two cases.
func TestConfig_LogValue_RedactsAPIKey(t *testing.T) {
	cfg := secretConfig()
	cases := map[string]any{
		"value":   cfg,
		"pointer": &cfg,
	}

	for name, logged := range cases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			slog.New(slog.NewJSONHandler(&buf, nil)).Error("qdrant", "cfg", logged)

			if strings.Contains(buf.String(), testAPIKey) {
				t.Fatalf("log output leaked the API key: %s", buf.String())
			}
			if !strings.Contains(buf.String(), redacted) {
				t.Fatalf("log output %s does not contain the redaction marker %q", buf.String(), redacted)
			}
			// Redaction that swallows the endpoint too would leave nothing worth logging.
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

// TestConfig_String_RedactsAPIKey closes the route that never reaches slog at all.
func TestConfig_String_RedactsAPIKey(t *testing.T) {
	cfg := secretConfig()
	cases := map[string]any{
		"value":   cfg,
		"pointer": &cfg,
	}

	for name, formatted := range cases {
		t.Run(name, func(t *testing.T) {
			out := fmt.Sprintf("%+v", formatted)
			if strings.Contains(out, testAPIKey) {
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

// TestConfig_Redaction_EmptyKeyStaysEmpty keeps "not set" distinguishable from "set but hidden": an
// absent key marked [REDACTED] would hide the misconfiguration NewQdrantClient exists to report.
func TestConfig_Redaction_EmptyKeyStaysEmpty(t *testing.T) {
	cfg := secretConfig()
	cfg.APIKey = ""

	if out := fmt.Sprintf("%v", cfg); strings.Contains(out, redacted) {
		t.Fatalf("an unset API key was reported as redacted: %s", out)
	}
}
