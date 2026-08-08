package config

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// NewLogger builds the process-wide structured logger at the given level, writing JSON to stderr.
// An unrecognized or empty level falls back to info: a typo must not silence the process, and it
// must not stop it either.
func NewLogger(level string) *slog.Logger {
	return newLogger(level, os.Stderr)
}

// newLogger is NewLogger with the destination injected, so tests can read what was written.
func newLogger(level string, w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: parseLevel(level)}))
}

func parseLevel(level string) slog.Level {
	var parsed slog.Level
	if err := parsed.UnmarshalText([]byte(strings.ToUpper(level))); err != nil {
		return slog.LevelInfo
	}
	return parsed
}
