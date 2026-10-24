package connector

import (
	"flag"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ParseCLI parses the standard first-party server flag surface defined by the
// contract (lisa/docs/GO_CONNECTOR_SDK.md). --data-dir is required; everything
// else has the contract default. Path-like config comes from flags only, never
// env vars.
//
// Servers that add flags beyond the standard set (e.g. lisa-storage's
// --deny-globs, lisa-thread's --thread-dir/--user-slug) should use NewFlagSet
// to obtain the shared FlagSet, register their extras on it, then call Parse.
func ParseCLI(serverID string) (*CLI, error) {
	c, fs := NewFlagSet(serverID)
	if err := fs.Parse(os.Args[1:]); err != nil {
		return nil, err
	}
	if err := c.resolve(); err != nil {
		return nil, err
	}
	return c, nil
}

// NewFlagSet returns a partially-populated CLI and the shared FlagSet with the
// standard contract flags already bound. Servers register server-specific
// flags on fs before calling fs.Parse + CLI.resolve (or simply ParseCLI when
// no extras are needed).
func NewFlagSet(serverID string) (*CLI, *flag.FlagSet) {
	fs := flag.NewFlagSet(serverID, flag.ContinueOnError)
	c := &CLI{serverID: serverID}
	fs.StringVar(&c.DataDir, "data-dir", "", "Canonical data root. All storage paths resolve relative to it (required).")
	fs.StringVar(&c.Transport, "transport", "stdio", "MCP transport (only stdio is supported in v1).")
	fs.StringVar(&c.LogLevel, "log-level", envOr("LOG_LEVEL", "INFO"), "Logging level (DEBUG, INFO, WARNING, ERROR, CRITICAL).")
	fs.StringVar(&c.WorkspaceRoot, "workspace-root", "", "Logical workspace root for relative path resolution.")
	fs.StringVar(&c.ConfigPath, "config", "", "Path to an optional JSON/YAML server config file.")
	fs.StringVar(&c.ServerID, "server-id", "", "Explicit server identity override for diagnostics.")
	fs.BoolVar(&c.Readonly, "readonly", false, "Disable write-side operations.")
	return c, fs
}

// resolve validates the parsed flags and applies defaults. Called by ParseCLI.
func (c *CLI) resolve() error {
	if c.DataDir == "" {
		return fmt.Errorf("--data-dir is required for server %q", c.serverID)
	}
	if err := os.MkdirAll(c.DataDir, 0o755); err != nil {
		return fmt.Errorf("resolve data dir: %w", err)
	}
	if c.ServerID == "" {
		c.ServerID = c.serverID
	}
	return nil
}

// NewImplementation builds the server identity the runtime sees over MCP.
func NewImplementation(name string) *mcp.Implementation {
	return &mcp.Implementation{Name: name, Version: Version}
}

// Version is the Connector SDK / server version reported over MCP.
const Version = "0.2.0"

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}
