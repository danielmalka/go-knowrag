// Command mcp-server exposes the knowledge layer to MCP clients over stdio.
// The tool surface itself lands in S08; this entrypoint only proves config and logging wire up.
package main

import (
	"fmt"
	"os"

	"github.com/danielmalka/go-knowrag/internal/config"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		// The error goes to stderr rather than the logger: the log level lives in the config
		// that just failed to load.
		fmt.Fprintln(os.Stderr, "mcp-server:", err)
		os.Exit(1)
	}

	config.NewLogger(cfg.LogLevel).Info("mcp-server starting", "config", cfg)
}
