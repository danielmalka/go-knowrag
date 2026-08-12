// Package clicmd holds the parts of the operator CLI that something outside cmd/cli has to be able
// to run.
//
// Everything else about the CLI stays in cmd/cli, and should: that package is `package main`, which
// is the right home for a binary's own wiring — dialing Qdrant, building the embedder, mapping an
// error to a process exit code — and an impossible home for anything a test elsewhere needs to
// reach, because a package main cannot be imported at all.
//
// Exactly one test needs to reach it. The CLI↔MCP search parity test drives both adapters in one
// process against one fake searcher, and the MCP adapter is itself unexported inside cmd/mcp-server
// (also package main), so the test has to live in that directory — see
// cmd/mcp-server/search_parity_test.go. The CLI half of that comparison is therefore here, and
// nothing else moved with it: this package holds the `search` command and the JSON envelope that
// command answers in, and no other command.
//
// The split is drawn so that the parity test exercises the real surface rather than a copy of it.
// What lives here is flag parsing, the retrieval.Query it builds and the output it writes — the
// three things a divergence from the MCP adapter would hide in. What stays in cmd/cli is the
// connection: the command takes a Connect function, so a test supplies a fake searcher and the
// binary supplies a real one.
package clicmd
