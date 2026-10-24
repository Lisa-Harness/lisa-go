// Package connector implements the language-neutral Lisa MCP server contract
// defined in lisa/docs/GO_CONNECTOR_SDK.md. It is the Go Connector SDK: the
// server-builder and host-embed side for connecting to Lisa. It consumes the
// neutral contract only — it never reimplements the Python framework or hosts
// packs.
//
// PROTOCOL version: see PROTOCOL.md.
package connector

import "time"

// CLI is the standard first-party server flag surface. Path-like config is
// carried here, never in env vars (env is for behavioral config only).
type CLI struct {
	DataDir       string // --data-dir (required): canonical data root
	Transport     string // --transport (default "stdio"); only stdio in v1
	LogLevel      string // --log-level (default $LOG_LEVEL or INFO)
	WorkspaceRoot string // --workspace-root (optional)
	ConfigPath    string // --config (optional JSON/YAML server config)
	ServerID      string // --server-id (optional diagnostics identity)
	Readonly      bool   // --readonly (only when the server supports it)

	serverID string // internal default identity used when --server-id is empty
}

// LaunchEnv holds the env contract the host compiler injects at spawn.
type LaunchEnv struct {
	// LogPath is LISA_MCP_LOG_PATH: the per-server JSONL log path under the
	// resolved data dir. When empty, fall back to <cwd>/<server_id>.jsonl and
	// never log to bare stderr (the stdio SDK inherits the host terminal).
	LogPath string
	// LogStderr mirrors logs to stderr when LISA_LOG_STDERR=1 (debugging).
	LogStderr bool
}

// Server describes a first-party MCP server the runtime spawns as a binary
// subprocess over stdio. It mirrors the neutral fields of the server's
// mcp_servers.yaml entry (RuntimeKind.BINARY) without binding to the Python
// MCPServerSpec type.
type Server struct {
	ID             string
	CommandPath    string        // compiled binary path (${VAR} already expanded)
	CLIFlags       CLI           // standard contract flags to pass
	Args           []string      // spec.cli_args appended verbatim, last
	DataDirSubdir  string        // namespaced storage under the host data dir
	EnsureDataDir  bool          // pre-create the data dir before spawn
	Readonly       bool          // append --readonly
	LaunchTimeouts time.Duration // spawn/connect timeout budget
}
