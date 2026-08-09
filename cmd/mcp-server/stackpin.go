//go:build stackpins

// This file exists only to keep the MCP SDK (PRD-contrato §2.3b) in go.mod until S08 wires the
// real tool surface here: `go mod tidy` reads files behind build tags, so the pin survives, while
// the tag — which nothing ever sets — keeps the import out of the binary.
//
// Delete this file when S08 lands the first real MCP import in this command.
package main

import _ "github.com/modelcontextprotocol/go-sdk/mcp"
