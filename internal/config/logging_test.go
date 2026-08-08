package config

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestNewLogger_ParsesLevel(t *testing.T) {
	cases := []struct {
		level   string
		enabled []slog.Level
		off     []slog.Level
	}{
		{level: "debug", enabled: []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelError}},
		{level: "info", enabled: []slog.Level{slog.LevelInfo, slog.LevelWarn}, off: []slog.Level{slog.LevelDebug}},
		{level: "warn", enabled: []slog.Level{slog.LevelWarn, slog.LevelError}, off: []slog.Level{slog.LevelInfo}},
		{level: "error", enabled: []slog.Level{slog.LevelError}, off: []slog.Level{slog.LevelWarn}},
		{level: "DEBUG", enabled: []slog.Level{slog.LevelDebug}},
		// An unusable level must fall back to info rather than silencing the process or panicking.
		{level: "bogus", enabled: []slog.Level{slog.LevelInfo}, off: []slog.Level{slog.LevelDebug}},
		{level: "", enabled: []slog.Level{slog.LevelInfo}, off: []slog.Level{slog.LevelDebug}},
	}

	ctx := context.Background()
	for _, tc := range cases {
		t.Run(tc.level, func(t *testing.T) {
			logger := NewLogger(tc.level)
			for _, lvl := range tc.enabled {
				if !logger.Enabled(ctx, lvl) {
					t.Errorf("NewLogger(%q): level %s is disabled, want enabled", tc.level, lvl)
				}
			}
			for _, lvl := range tc.off {
				if logger.Enabled(ctx, lvl) {
					t.Errorf("NewLogger(%q): level %s is enabled, want disabled", tc.level, lvl)
				}
			}
		})
	}
}

func TestNewLogger_EmitsStructuredJSON(t *testing.T) {
	var buf bytes.Buffer
	newLogger("info", &buf).Info("hello")

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("log line %q is not valid JSON: %v", buf.String(), err)
	}
	if line["msg"] != "hello" {
		t.Errorf("msg = %v, want %q", line["msg"], "hello")
	}
	if line["level"] != "INFO" {
		t.Errorf("level = %v, want %q", line["level"], "INFO")
	}
}
