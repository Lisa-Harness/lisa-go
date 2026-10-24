// lisa-fetch is the Go port of the Lisa Fetch MCP server
// (lisa/mcp_server/fetch/server.py). It is a thin pass-through over
// connector/fetch: all network/cache/SSRF logic lives in the fetcher, never
// in the server. Registered with the runtime as a binary-runtime MCP server
// (no kernel change); spawned over stdio per the contract.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Lisa-Harness/lisa-go/connector"
	"github.com/Lisa-Harness/lisa-go/connector/fetch"
)

const serverName = "lisa-fetch"

// fetchURLArgs mirrors the Python fetch_url tool parameters.
type fetchURLArgs struct {
	URL          string   `json:"url"`
	TTLSeconds   *float64 `json:"ttl_seconds,omitempty"`
	ForceRefresh *bool    `json:"force_refresh,omitempty"`
	MaxBytes     *int     `json:"max_bytes,omitempty"`
}

// invalidateArgs mirrors the Python invalidate_fetch_cache tool parameters.
type invalidateArgs struct {
	URL *string `json:"url,omitempty"`
}

func main() {
	cli, err := connector.ParseCLI(serverName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lisa-fetch:", err)
		os.Exit(2)
	}

	logger, closeLog, err := connector.SetupLogger(serverName, cli.LogLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lisa-fetch: logger:", err)
		os.Exit(2)
	}
	defer closeLog()
	logger.Info("lisa-fetch starting", "data_dir", cli.DataDir)

	fetcher, err := fetch.New(cli.DataDir)
	if err != nil {
		logger.Error("init fetcher failed", "err", err)
		os.Exit(1)
	}

	server := mcp.NewServer(connector.NewImplementation("Lisa Fetch Server"), nil)

	// fetch_url: fetch one http(s) URL and return its text, caching per the
	// effective policy. Optional overrides build a per-call policy on top of
	// the server default. SSRF rejection is an error; on a transport failure
	// with a valid stale cache, the stale content is returned.
	server.AddTool(&mcp.Tool{
		Name: "fetch_url",
		Description: "Fetch one http(s) URL and return its text, caching per the " +
			"effective policy. SSRF rejection (scheme/host/private-IP) is an error. " +
			"On a network failure with a valid stale cache, the stale content is returned.",
		InputSchema: rawSchema(`{
			"type": "object",
			"properties": {
				"url":           {"type": "string", "description": "The http(s) URL to fetch."},
				"ttl_seconds":   {"type": "number", "description": "Per-call cache TTL override (seconds)."},
				"force_refresh": {"type": "boolean", "description": "Bypass the cache and fetch fresh."},
				"max_bytes":     {"type": "integer", "description": "Per-call response-size cap override."}
			},
			"required": ["url"]
		}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args fetchURLArgs
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		policy := fetch.DefaultPolicy()
		if args.TTLSeconds != nil || args.ForceRefresh != nil || args.MaxBytes != nil {
			if args.TTLSeconds != nil {
				policy.TTLSeconds = *args.TTLSeconds
			}
			if args.ForceRefresh != nil {
				policy.ForceRefresh = *args.ForceRefresh
			}
			if args.MaxBytes != nil {
				policy.MaxBytes = *args.MaxBytes
			}
		}
		result, err := fetcher.Fetch(args.URL, &policy)
		if err != nil {
			return nil, err
		}
		payload, err := json.Marshal(map[string]any{
			"url":           result.URL,
			"status_code":   result.StatusCode,
			"text":          result.Text,
			"content_type":  result.ContentType,
			"etag":          result.ETag,
			"last_modified": result.LastModified,
			"fetched_at":    result.FetchedAt,
			"from_cache":    result.FromCache,
		})
		if err != nil {
			return nil, err
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: string(payload)}},
		}, nil
	})

	// invalidate_fetch_cache: clear the cache. If url is given, clear only
	// that entry.
	server.AddTool(&mcp.Tool{
		Name: "invalidate_fetch_cache",
		Description: "Clear the fetch cache. If `url` is given, clear only that " +
			"entry; otherwise clear the entire cache.",
		InputSchema: rawSchema(`{
			"type": "object",
			"properties": {
				"url": {"type": "string", "description": "Optional URL to invalidate. Omit to clear everything."}
			}
		}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args invalidateArgs
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		fetcher.Invalidate(args.URL)
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: `{"ok": true}`}},
		}, nil
	})

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		logger.Error("server exited", "err", err)
		os.Exit(1)
	}
}

func rawSchema(s string) json.RawMessage { return json.RawMessage(s) }
