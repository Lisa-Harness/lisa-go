// lisa-storage is the Go port of the Lisa Storage MCP server
// (lisa/mcp_server/storage/server.py). It is a thin pass-through over
// connector/storage: the sandbox resolution, deny-glob policy, and
// model-manifest read/write all live in the backend, never in the server.
// Registered with the runtime as a binary-runtime MCP server (no kernel
// change); spawned over stdio per the contract
// (lisa/docs/GO_CONNECTOR_SDK.md).
//
// Tools (10 total): file I/O (8): read_file, read_file_range, write_file,
// append_file, list_files, delete_file, stat_file, make_dir; model
// manifests (2): read_model_manifest, write_model_manifest.
//
// Error contract: the backend returns json.dumps({"error": ...}) strings
// on failure; the server parses those returns and surfaces a structured
// CallToolResult(is_error=True) — the same behavior the Python server's
// _raise_on_error produces via ToolError. Success results pass through
// unchanged.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Lisa-Harness/lisa-go/connector"
	"github.com/Lisa-Harness/lisa-go/connector/storage"
)

const serverName = "lisa-storage"

// ── Tool argument shapes (mirror the Python tool signatures) ──────────────

type readFileArgs struct {
	Path     string `json:"path"`
	Encoding string `json:"encoding,omitempty"`
}

type readFileRangeArgs struct {
	Path     string `json:"path"`
	Start    *int   `json:"start,omitempty"`
	Length   *int   `json:"length,omitempty"`
	Encoding string `json:"encoding,omitempty"`
}

type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type appendFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type listFilesArgs struct {
	Prefix string `json:"prefix,omitempty"`
}

type pathArgs struct {
	Path string `json:"path"`
}

type readManifestArgs struct {
	Which string `json:"which,omitempty"`
}

type writeManifestArgs struct {
	Content string `json:"content"`
	Which   string `json:"which,omitempty"`
}

// raiseOnError converts a backend error-string into an MCP tool error. The
// backend returns a JSON string; when it parses to an object with an
// "error" key the server returns CallToolResult with IsError=true (the MCP
// client sees a structured error, matching the Python ToolError path).
// Success results pass through unchanged.
func raiseOnError(raw string) (*mcp.CallToolResult, error) {
	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
		if obj, ok := parsed.(map[string]any); ok {
			if msg, ok := obj["error"].(string); ok {
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: msg}},
				}, nil
			}
		}
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: raw}},
	}, nil
}

var (
	// Tool annotations matching the Python server's ToolAnnotations:
	//   read_only_hint=True   for read_file, read_file_range, list_files,
	//                         stat_file, read_model_manifest
	//   destructive_hint=True for write_file, append_file, delete_file,
	//                         make_dir, write_model_manifest
	readOnlyAnnotations    = &mcp.ToolAnnotations{ReadOnlyHint: true}
	destructiveAnnotations = &mcp.ToolAnnotations{DestructiveHint: &[]bool{true}[0]}
)

func main() {
	cli, fs := connector.NewFlagSet(serverName)
	var denyGlobs multiFlag
	fs.Var(&denyGlobs, "deny-glob", "Glob pattern to deny in the files sandbox (repeatable). Matched against forward-slash relative paths via fnmatch.")
	if err := fs.Parse(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "lisa-storage:", err)
		os.Exit(2)
	}
	if cli.DataDir == "" {
		fmt.Fprintln(os.Stderr, "lisa-storage: --data-dir is required")
		os.Exit(2)
	}
	// resolve_data_dir: no env-var fallback, no hardcoded default; the server
	// uses what it gets and creates it (mirrors connector.CLI.resolve, which
	// is unexported; server-specific flags forced NewFlagSet + Parse here).
	if err := os.MkdirAll(cli.DataDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "lisa-storage: resolve data dir:", err)
		os.Exit(2)
	}
	if cli.ServerID == "" {
		cli.ServerID = serverName
	}

	// LISA_STORAGE_DENY_GLOBS (comma-separated) merges into the deny list so
	// hosts can set policy via env without editing the launch YAML.
	globs := append([]string{}, denyGlobs...)
	if env := os.Getenv("LISA_STORAGE_DENY_GLOBS"); env != "" {
		for _, g := range strings.Split(env, ",") {
			if g = strings.TrimSpace(g); g != "" {
				globs = append(globs, g)
			}
		}
	}

	logger, closeLog, err := connector.SetupLogger(serverName, cli.LogLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lisa-storage: logger:", err)
		os.Exit(2)
	}
	defer closeLog()
	logger.Info("lisa-storage starting", "data_dir", cli.DataDir, "readonly", cli.Readonly)

	backend := storage.New(cli.DataDir, cli.Readonly, globs)

	server := mcp.NewServer(connector.NewImplementation("Lisa Storage Server"), nil)

	server.AddTool(&mcp.Tool{
		Name: "read_file",
		Description: "Read a text file from the files sandbox. " +
			"`path` is relative within the files sandbox (no .. or absolute). " +
			"Returns the file content as a string.",
		InputSchema: rawSchema(`{
			"type": "object",
			"properties": {
				"path":     {"type": "string", "description": "Relative path within the files sandbox (no .. or absolute)."},
				"encoding": {"type": "string", "description": "Text encoding to use (default utf-8)."}
			},
			"required": ["path"]
		}`),
		Annotations: readOnlyAnnotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args readFileArgs
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		return raiseOnError(backend.ReadFile(args.Path, args.Encoding))
	})

	server.AddTool(&mcp.Tool{
		Name: "read_file_range",
		Description: "Read a char-bounded slice of a text file from the files sandbox. " +
			"Slicing is char-based so multibyte codepoints are never split. " +
			"Returns {content, start, length, total, eof}.",
		InputSchema: rawSchema(`{
			"type": "object",
			"properties": {
				"path":     {"type": "string", "description": "Relative path within the files sandbox (no .. or absolute)."},
				"start":    {"type": "integer", "description": "Char offset to begin reading (0-based). Negative coerced to 0."},
				"length":   {"type": "integer", "description": "Max chars to return. Coerced to >= 0."},
				"encoding": {"type": "string", "description": "Text encoding (default utf-8)."}
			},
			"required": ["path"]
		}`),
		Annotations: readOnlyAnnotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args readFileRangeArgs
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		start, length := 0, 4096
		if args.Start != nil {
			start = *args.Start
		}
		if args.Length != nil {
			length = *args.Length
		}
		return raiseOnError(backend.ReadFileRange(args.Path, start, length, args.Encoding))
	})

	server.AddTool(&mcp.Tool{
		Name: "write_file",
		Description: "Write text content to a file in the files sandbox. " +
			"Creates parent directories as needed. Respects --readonly. " +
			"Returns {ok, path, bytes, trailing_newline_added}.",
		InputSchema: rawSchema(`{
			"type": "object",
			"properties": {
				"path":    {"type": "string", "description": "Relative path within the files sandbox (no .. or absolute)."},
				"content": {"type": "string", "description": "Text content to write."}
			},
			"required": ["path", "content"]
		}`),
		Annotations: destructiveAnnotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args writeFileArgs
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		return raiseOnError(backend.WriteFile(args.Path, args.Content))
	})

	server.AddTool(&mcp.Tool{
		Name: "append_file",
		Description: "Append text content to a file in the files sandbox. " +
			"Creates parent directories and the file if absent. Respects --readonly. " +
			"Returns {ok, path, bytes, appended_bytes} (bytes is the new total file size).",
		InputSchema: rawSchema(`{
			"type": "object",
			"properties": {
				"path":    {"type": "string", "description": "Relative path within the files sandbox (no .. or absolute)."},
				"content": {"type": "string", "description": "Text content to append."}
			},
			"required": ["path", "content"]
		}`),
		Annotations: destructiveAnnotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args appendFileArgs
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		return raiseOnError(backend.AppendFile(args.Path, args.Content))
	})

	server.AddTool(&mcp.Tool{
		Name: "list_files",
		Description: "List files under a prefix in the files sandbox. " +
			"Returns a JSON array of relative paths (forward slashes). " +
			"Denied paths are filtered from results.",
		InputSchema: rawSchema(`{
			"type": "object",
			"properties": {
				"prefix": {"type": "string", "description": "Optional prefix to list under."}
			}
		}`),
		Annotations: readOnlyAnnotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args listFilesArgs
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		return raiseOnError(backend.ListFiles(args.Prefix))
	})

	server.AddTool(&mcp.Tool{
		Name: "delete_file",
		Description: "Delete a file from the files sandbox. " +
			"Fails on directories (delete is files-only). Respects --readonly.",
		InputSchema: rawSchema(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "Relative path within the files sandbox (no .. or absolute)."}
			},
			"required": ["path"]
		}`),
		Annotations: destructiveAnnotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args pathArgs
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		return raiseOnError(backend.DeleteFile(args.Path))
	})

	server.AddTool(&mcp.Tool{
		Name:        "stat_file",
		Description: "Return file metadata (path, size, modified, is_dir) for a path.",
		InputSchema: rawSchema(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "Relative path within the files sandbox (no .. or absolute)."}
			},
			"required": ["path"]
		}`),
		Annotations: readOnlyAnnotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args pathArgs
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		return raiseOnError(backend.StatFile(args.Path))
	})

	server.AddTool(&mcp.Tool{
		Name: "make_dir",
		Description: "Create a directory (and parents) in the files sandbox. " +
			"Respects --readonly.",
		InputSchema: rawSchema(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "Relative path within the files sandbox (no .. or absolute)."}
			},
			"required": ["path"]
		}`),
		Annotations: destructiveAnnotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args pathArgs
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		return raiseOnError(backend.MakeDir(args.Path))
	})

	server.AddTool(&mcp.Tool{
		Name: "read_model_manifest",
		Description: "Read the active or proposed model manifest as text. " +
			"`which` is 'active' for models.yaml or 'proposal' for " +
			"models.proposed.yaml (default 'proposal').",
		InputSchema: rawSchema(`{
			"type": "object",
			"properties": {
				"which": {"type": "string", "description": "'active' or 'proposal' (default 'proposal')."}
			}
		}`),
		Annotations: readOnlyAnnotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args readManifestArgs
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		return raiseOnError(backend.ReadModelManifest(args.Which))
	})

	server.AddTool(&mcp.Tool{
		Name: "write_model_manifest",
		Description: "Write text content to the active or proposed model manifest. " +
			"Creates the parent directory if needed. Respects --readonly. " +
			"Returns {ok, which, bytes}.",
		InputSchema: rawSchema(`{
			"type": "object",
			"properties": {
				"content": {"type": "string", "description": "The full manifest text (YAML) to write."},
				"which":    {"type": "string", "description": "'active' or 'proposal' (default 'proposal')."}
			},
			"required": ["content"]
		}`),
		Annotations: destructiveAnnotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args writeManifestArgs
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		return raiseOnError(backend.WriteModelManifest(args.Content, args.Which))
	})

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		logger.Error("server exited", "err", err)
		os.Exit(1)
	}
}

// multiFlag collects a repeatable string flag (the contract's --deny-glob
// is repeatable, matching the Python `action="append"`).
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func rawSchema(s string) json.RawMessage { return json.RawMessage(s) }
