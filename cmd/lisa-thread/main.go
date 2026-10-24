// lisa-thread is the Go port of the Lisa Thread MCP server
// (lisa/mcp_server/thread/server.py). It is a thin facade over
// connector/thread: the read path lives in ThreadStore/ThreadReader, never
// in the server. Registered with the runtime as a binary-runtime MCP server
// (no kernel change); spawned over stdio per the contract.
//
// Tools (6 total, all read-only): thread_read_range, thread_read_recent,
// thread_search, thread_read_content, thread_find_content, thread_task_view.
//
// Projection contract: ThreadStore.LoadRange / LoadRecent return RAW entries
// by design (the lossless read contract); the server applies the resolved
// redaction policy to every model-facing projection, exactly like the Python
// server's ThreadDeps.redaction. The raw-vs-projected split and redaction
// are security invariants; nothing is weakened here.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Lisa-Harness/lisa-go/connector"
	"github.com/Lisa-Harness/lisa-go/connector/thread"
)

const serverName = "lisa-thread"

// Redaction env contract (mirrors mcp_server/thread/server.py).
const (
	redactionEnabledEnv  = "LISA_THREAD_REDACTION_ENABLED"
	redactionPatternsEnv = "LISA_THREAD_REDACTION_PATTERNS"
)

var readOnlyAnnotations = &mcp.ToolAnnotations{ReadOnlyHint: true}

// ── Tool argument shapes (mirror the Python tool signatures) ──────────────

type readRangeArgs struct {
	StartTurn         int     `json:"start_turn"`
	EndTurn           int     `json:"end_turn"`
	IncludeTranscript *bool   `json:"include_transcript,omitempty"`
	Cursor            *string `json:"cursor,omitempty"`
	BudgetBytes       *int    `json:"budget_bytes,omitempty"`
}

type readRecentArgs struct {
	Limit             *int    `json:"limit,omitempty"`
	IncludeTranscript *bool   `json:"include_transcript,omitempty"`
	Cursor            *string `json:"cursor,omitempty"`
	BudgetBytes       *int    `json:"budget_bytes,omitempty"`
}

type searchArgs struct {
	Query       string  `json:"query"`
	Limit       *int    `json:"limit,omitempty"`
	Cursor      *string `json:"cursor,omitempty"`
	BudgetBytes *int    `json:"budget_bytes,omitempty"`
	ToolName    *string `json:"tool_name,omitempty"`
	Path        *string `json:"path,omitempty"`
	TaskID      *string `json:"task_id,omitempty"`
	StartTurn   *int    `json:"start_turn,omitempty"`
	EndTurn     *int    `json:"end_turn,omitempty"`
}

type readContentArgs struct {
	Ref         string `json:"ref"`
	OffsetChars *int   `json:"offset_chars,omitempty"`
	LimitChars  *int   `json:"limit_chars,omitempty"`
	BudgetBytes *int   `json:"budget_bytes,omitempty"`
}

type findContentArgs struct {
	Ref         string `json:"ref"`
	Query       string `json:"query"`
	OffsetChars *int   `json:"offset_chars,omitempty"`
	Limit       *int   `json:"limit,omitempty"`
	BudgetBytes *int   `json:"budget_bytes,omitempty"`
}

type taskViewArgs struct {
	TaskID      *string `json:"task_id,omitempty"`
	BudgetBytes *int    `json:"budget_bytes,omitempty"`
}

func main() {
	cli, fs := connector.NewFlagSet(serverName)
	var threadDir, userSlug string
	fs.StringVar(&threadDir, "thread-dir", "",
		"Directory containing per-user thread stores. The active store is "+
			"--thread-dir/--user-slug. Required.")
	fs.StringVar(&userSlug, "user-slug", "",
		"User slug selecting the per-user thread store. Required.")
	if err := fs.Parse(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "lisa-thread:", err)
		os.Exit(2)
	}
	if cli.DataDir == "" {
		fmt.Fprintln(os.Stderr, "lisa-thread: --data-dir is required")
		os.Exit(2)
	}
	if err := os.MkdirAll(cli.DataDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "lisa-thread: resolve data dir:", err)
		os.Exit(2)
	}
	if cli.ServerID == "" {
		cli.ServerID = serverName
	}
	if threadDir == "" {
		fmt.Fprintln(os.Stderr, "lisa-thread: --thread-dir is required")
		os.Exit(2)
	}
	if userSlug == "" {
		fmt.Fprintln(os.Stderr, "lisa-thread: --user-slug is required")
		os.Exit(2)
	}

	logger, closeLog, err := connector.SetupLogger(serverName, cli.LogLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lisa-thread: logger:", err)
		os.Exit(2)
	}
	defer closeLog()
	logger.Info("lisa-thread starting", "data_dir", cli.DataDir, "thread_dir", threadDir, "user_slug", userSlug)

	// The thread base path is <thread-dir>/<user-slug>; no env-var path
	// resolution. The redaction policy comes from the env contract
	// (LISA_THREAD_REDACTION_ENABLED / LISA_THREAD_REDACTION_PATTERNS) via
	// connector/thread.ResolveRedactionPolicy and is applied to EVERY
	// model-facing projection below (raw load paths stay lossless).
	base := filepath.Join(threadDir, userSlug)
	policy := thread.ResolveRedactionPolicy(
		os.Getenv(redactionEnabledEnv), os.Getenv(redactionPatternsEnv), logger)
	store := thread.NewThreadStore(base, policy, logger)
	reader := thread.NewThreadReader(store, policy, thread.NewPageCache())

	server := mcp.NewServer(connector.NewImplementation("Lisa Thread Server"), nil)
	addThreadTools(server, reader)

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		logger.Error("server exited", "err", err)
		os.Exit(1)
	}
}

// toolTextErr surfaces a handler failure the way the Python server does
// (ToolError -> CallToolResult(is_error=True)).
func toolTextErr(err error) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
	}, nil
}

func toolText(payload string) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: payload}},
	}, nil
}

func addThreadTools(server *mcp.Server, reader *thread.ThreadReader) {
	// thread_read_range: read recorded past exchanges by inclusive global
	// turn index. Returns {scope, interpretation, entries, next_cursor,
	// has_more}; entry fields are small-inline previews with {ref, head,
	// tail, total_chars, omitted_chars} for large values.
	server.AddTool(&mcp.Tool{
		Name: "thread_read_range",
		Description: "Read recorded past exchanges by inclusive global turn index, not the active request. " +
			"Search with thread_search first, then read the relevant turn range. " +
			"Returns {scope, interpretation, entries, next_cursor, has_more}. " +
			"Entries carry recorded timestamps (null if unknown) and provenance sources. " +
			"Earlier instructions and diagnoses are historical, not live runtime state. " +
			"Small fields remain inline; large fields have {ref, head, tail, total_chars, omitted_chars} previews. " +
			"Expand a ref with thread_read_content to obtain the complete permitted value through pages. " +
			"The archive is lossless; previews are not full data. " +
			"include_transcript adds tool calls/results as bounded message previews. " +
			"Follow next_cursor with the same range and include_transcript setting; " +
			"a page may continue within a turn (transcript_start gives its position). " +
			"budget_bytes defaults to 16384, clamped to 16384-32768 including metadata.",
		InputSchema: rawSchema(`{
			"type": "object",
			"properties": {
				"start_turn":         {"type": "integer", "description": "First global turn index (inclusive, >= 0)."},
				"end_turn":          {"type": "integer", "description": "Last global turn index (inclusive)."},
				"include_transcript": {"type": "boolean", "description": "Include bounded tool call/result message previews."},
				"cursor":            {"type": "string", "description": "Continuation cursor from a previous page."},
				"budget_bytes":      {"type": "integer", "description": "Serialized page budget in UTF-8 bytes (16384-32768)."}
			},
			"required": ["start_turn", "end_turn"]
		}`),
		Annotations: readOnlyAnnotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args readRangeArgs
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		include := args.IncludeTranscript != nil && *args.IncludeTranscript
		budget := thread.DEFAULT_READOUT_BYTES
		if args.BudgetBytes != nil {
			budget = *args.BudgetBytes
		}
		out, err := reader.ReadRange(args.StartTurn, args.EndTurn, include, args.Cursor, budget)
		if err != nil {
			return toolTextErr(err)
		}
		return toolText(out)
	})

	// thread_read_recent: read the most recent N recorded exchanges,
	// oldest-first in the window.
	server.AddTool(&mcp.Tool{
		Name: "thread_read_recent",
		Description: "Read recorded history, oldest-first (limit defaults to 10, capped at 50). " +
			"Returns the labeled history envelope as in thread_read_range, including recorded " +
			"timestamps and provenance sources. Latest recorded is not current. " +
			"Large fields are explicit previews; expand refs with thread_read_content. " +
			"include_transcript adds bounded tool call/result previews. Continue with " +
			"next_cursor and the same include_transcript setting; later appends do not " +
			"shift the window. budget_bytes defaults to 16384, clamped to 16384-32768 " +
			"serialized UTF-8 bytes including metadata. Use thread_search for topics.",
		InputSchema: rawSchema(`{
			"type": "object",
			"properties": {
				"limit":             {"type": "integer", "description": "Number of recent entries (default 10, capped at 50)."},
				"include_transcript": {"type": "boolean", "description": "Include bounded tool call/result message previews."},
				"cursor":            {"type": "string", "description": "Continuation cursor from a previous page."},
				"budget_bytes":      {"type": "integer", "description": "Serialized page budget in UTF-8 bytes (16384-32768)."}
			}
		}`),
		Annotations: readOnlyAnnotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args readRecentArgs
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		limit := 10
		if args.Limit != nil {
			limit = *args.Limit
		}
		include := args.IncludeTranscript != nil && *args.IncludeTranscript
		budget := thread.DEFAULT_READOUT_BYTES
		if args.BudgetBytes != nil {
			budget = *args.BudgetBytes
		}
		out, err := reader.ReadRecent(limit, include, args.Cursor, budget)
		if err != nil {
			return toolTextErr(err)
		}
		return toolText(out)
	})

	// thread_search: deterministic literal search over permitted archive
	// text and paired tool evidence.
	server.AddTool(&mcp.Tool{
		Name: "thread_search",
		Description: "Find prior turns and tool evidence by literal phrase/token matches. " +
			"Returns a recorded_history envelope ranked by relevance then recency, with turn " +
			"numbers, query/response previews, and score (0-1). Use thread_read_range for the " +
			"relevant turns (include_transcript=true for tool calls/results), or " +
			"thread_read_content to expand a preview ref. An empty query returns recent matches. " +
			"limit defaults to 10, capped at 20. Continue with next_cursor and the same " +
			"query/limit. budget_bytes defaults to 16384, clamped to 16384-32768 serialized " +
			"UTF-8 bytes including metadata. Filter by exact tool_name, path substring in " +
			"structured arguments, task_id, and inclusive start/end_turn. Each match has ref, " +
			"excerpt, and offset_chars for thread_read_content. Use thread_find_content to " +
			"locate text inside a particular large result.",
		InputSchema: rawSchema(`{
			"type": "object",
			"properties": {
				"query":        {"type": "string", "description": "Literal phrase/tokens to find."},
				"limit":        {"type": "integer", "description": "Max entries to return (default 10, capped at 20)."},
				"cursor":       {"type": "string", "description": "Continuation cursor from a previous page."},
				"budget_bytes": {"type": "integer", "description": "Serialized page budget in UTF-8 bytes (16384-32768)."},
				"tool_name":    {"type": "string", "description": "Filter to an exact tool name."},
				"path":         {"type": "string", "description": "Filter to tool arguments containing this path substring."},
				"task_id":      {"type": "string", "description": "Filter to one recorded task identity."},
				"start_turn":   {"type": "integer", "description": "First turn index (inclusive) to search."},
				"end_turn":     {"type": "integer", "description": "Last turn index (inclusive) to search."}
			},
			"required": ["query"]
		}`),
		Annotations: readOnlyAnnotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args searchArgs
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		limit := 10
		if args.Limit != nil {
			limit = *args.Limit
		}
		budget := thread.DEFAULT_READOUT_BYTES
		if args.BudgetBytes != nil {
			budget = *args.BudgetBytes
		}
		startTurn := 0
		if args.StartTurn != nil {
			startTurn = *args.StartTurn
		}
		out, err := reader.Search(args.Query, limit, args.Cursor, budget,
			args.ToolName, args.Path, args.TaskID, startTurn, args.EndTurn)
		if err != nil {
			return toolTextErr(err)
		}
		return toolText(out)
	})

	// thread_read_content: expand a referenced value through bounded pages.
	server.AddTool(&mcp.Tool{
		Name: "thread_read_content",
		Description: "Expand a thread preview ref without dumping unrelated history. " +
			"Returns {ref, source, content, offset_chars, total_chars, next_offset_chars, " +
			"has_more}. Follow next_offset_chars with the same ref to recover the complete " +
			"permitted value through pages. limit_chars defaults to 4000. budget_bytes " +
			"defaults to 16384, clamped to 16384-32768 UTF-8 bytes including metadata; a " +
			"page may contain fewer characters than asked. Refs are scoped to this thread: " +
			"t391.query, t391.response_text, t391.m20.content, t391.m19.tool_calls, or " +
			"t391.m20 for a full message. Search can also return t391.m19.c0.arguments or " +
			".name for one call. Live/replayed result previews carry r<sha256> refs. " +
			"Full reasoning_text and raw turn_input are not available through this tool.",
		InputSchema: rawSchema(`{
			"type": "object",
			"properties": {
				"ref":          {"type": "string", "description": "Thread content reference (t<turn>[.m<index>][.c<index>][.field] or r<sha256>)."},
				"offset_chars": {"type": "integer", "description": "Character offset into the value (default 0)."},
				"limit_chars":  {"type": "integer", "description": "Max characters per page (default 4000)."},
				"budget_bytes": {"type": "integer", "description": "Serialized page budget in UTF-8 bytes (16384-32768)."}
			},
			"required": ["ref"]
		}`),
		Annotations: readOnlyAnnotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args readContentArgs
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		if args.Ref == "" {
			return toolTextErr(errors.New("ref is required"))
		}
		offset := 0
		if args.OffsetChars != nil {
			offset = *args.OffsetChars
		}
		limit := 4000
		if args.LimitChars != nil {
			limit = *args.LimitChars
		}
		budget := thread.DEFAULT_READOUT_BYTES
		if args.BudgetBytes != nil {
			budget = *args.BudgetBytes
		}
		out, err := reader.ReadContent(args.Ref, offset, limit, budget)
		if err != nil {
			return toolTextErr(err)
		}
		return toolText(out)
	})

	// thread_find_content: locate literal evidence inside a referenced value.
	server.AddTool(&mcp.Tool{
		Name: "thread_find_content",
		Description: "Find literal text inside a referenced value, case-insensitively. " +
			"Returns bounded {ref, source, matches, next_offset_chars, has_more, total_chars}. " +
			"Matches give excerpt, offset_chars, and match_offset_chars after redaction. " +
			"Read the relevant span with thread_read_content(ref, offset_chars). Follow " +
			"next_offset_chars with the same ref/query to find further matches. limit " +
			"defaults to 10, capped at 20; budget_bytes is clamped to 16384-32768 UTF-8 " +
			"bytes including metadata.",
		InputSchema: rawSchema(`{
			"type": "object",
			"properties": {
				"ref":          {"type": "string", "description": "Thread content reference to search inside."},
				"query":        {"type": "string", "description": "Literal text to find (case-insensitive)."},
				"offset_chars": {"type": "integer", "description": "Character offset to start searching from (default 0)."},
				"limit":        {"type": "integer", "description": "Max matches to return (default 10, capped at 20)."},
				"budget_bytes": {"type": "integer", "description": "Serialized page budget in UTF-8 bytes (16384-32768)."}
			},
			"required": ["ref", "query"]
		}`),
		Annotations: readOnlyAnnotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args findContentArgs
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		if args.Ref == "" || args.Query == "" {
			return toolTextErr(errors.New("ref and query are required"))
		}
		offset := 0
		if args.OffsetChars != nil {
			offset = *args.OffsetChars
		}
		limit := 10
		if args.Limit != nil {
			limit = *args.Limit
		}
		budget := thread.DEFAULT_READOUT_BYTES
		if args.BudgetBytes != nil {
			budget = *args.BudgetBytes
		}
		out, err := reader.FindContent(args.Ref, args.Query, offset, limit, budget)
		if err != nil {
			return toolTextErr(err)
		}
		return toolText(out)
	})

	// thread_task_view: summarize one task's recorded state and evidence.
	server.AddTool(&mcp.Tool{
		Name: "thread_task_view",
		Description: "Summarize one task's recorded state instead of replaying its history. " +
			"task_id defaults to the most recent recorded task identity. Scans at most 200 " +
			"recent entries. This is NOT the active invocation's task identity. " +
			"scope, selection, and as_of_utc label the view as recorded history; status and " +
			"blockers are recorded observations, not verified live state. Returns objective / " +
			"latest previews and refs, blocking status, modes, receipt groups (capability_id, " +
			"effect, count, artifact_ids), and archive evidence refs for thread_read_content " +
			"or thread_find_content. history_may_be_incomplete warns that the objective or " +
			"earlier work may lie outside the scan. budget_bytes is positive, clamped to " +
			"1024-32768 UTF-8 bytes including metadata.",
		InputSchema: rawSchema(`{
			"type": "object",
			"properties": {
				"task_id":      {"type": "string", "description": "Recorded task identity; defaults to the most recent."},
				"budget_bytes": {"type": "integer", "description": "Serialized page budget in UTF-8 bytes (1024-32768)."}
			}
		}`),
		Annotations: readOnlyAnnotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args taskViewArgs
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		taskID := ""
		if args.TaskID != nil {
			taskID = *args.TaskID
		}
		// The Python MCP tool defaults budget_bytes to DEFAULT_READOUT_BYTES
		// (16384) — NOT the SDK's task-view default of 8192. The SDK-level
		// 8192 default never reaches the wire.
		budget := thread.DEFAULT_READOUT_BYTES
		if args.BudgetBytes != nil {
			budget = *args.BudgetBytes
		}
		out, err := reader.TaskView(taskID, thread.TaskScanLimit, budget)
		if err != nil {
			return toolTextErr(err)
		}
		return toolText(out)
	})
}

func rawSchema(s string) json.RawMessage { return json.RawMessage(s) }
