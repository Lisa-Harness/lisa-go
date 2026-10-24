// lisa-memory is the Go port of the Lisa Memory MCP server
// (lisa/mcp_server/memory/server.py). It is a thin facade over
// connector/memory: store semantics, ranking, backups and the embedding
// adapter all live in the backend, never in the server. Registered with the
// runtime as a binary-runtime MCP server (no kernel change); spawned over
// stdio per the contract (lisa/docs/GO_CONNECTOR_SDK.md).
//
// Tools (7 total): add_memory, search_memory, list_memories, get_memory,
// delete_memory, retire_memory, count_memories — the native wire names the
// Python pack's tool_map publishes memory.add / memory.search / etc. over.
// Keep the names, signatures and response strings identical: the capability
// alias contract depends on them.
//
// Error contract: not-found failures surface as CallToolResult(is_error=true)
// the same way the Python server's ToolError does. Cross-surface reads and
// writes remain impossible — scope filters are applied server-side.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Lisa-Harness/lisa-go/connector"
	"github.com/Lisa-Harness/lisa-go/connector/memory"
)

const serverName = "lisa-memory"

// dataSubdir mirrors sdk.layout.DATA_DIR: the SQLite file and the backups
// dir resolve under <data-dir>/data/.
const dataSubdir = "data"

// Tool annotations matching the Python server's ToolAnnotations:
// read_only_hint=True for search/get/list/count, destructive_hint=True for
// delete/retire; add carries no hints (ToolAnnotations() default).
var (
	readOnlyAnnotations    = &mcp.ToolAnnotations{ReadOnlyHint: true}
	destructiveAnnotations = &mcp.ToolAnnotations{DestructiveHint: boolPtr(true)}
	defaultAnnotations     = &mcp.ToolAnnotations{}
)

func boolPtr(b bool) *bool { return &b }

// ── Tool argument shapes (mirror the Python tool signatures) ──────────────

type addMemoryArgs struct {
	UserSlug      string         `json:"user_slug"`
	Content       string         `json:"content"`
	MemoryScope   string         `json:"memory_scope,omitempty"`
	WorkspaceID   string         `json:"workspace_id,omitempty"`
	SurfaceID     string         `json:"surface_id,omitempty"`
	MemoryTier    string         `json:"memory_tier,omitempty"`
	MemoryKind    string         `json:"memory_kind,omitempty"`
	OperationID   string         `json:"operation_id,omitempty"`
	InstructionID string         `json:"instruction_id,omitempty"`
	ContentJSON   map[string]any `json:"content_json,omitempty"`
	Salience      float64        `json:"salience,omitempty"`
	SupersedesID  *int64         `json:"supersedes_id,omitempty"`
	ItemIDs       []int64        `json:"item_ids,omitempty"`
	Confidence    *float64       `json:"confidence,omitempty"`
	ValidUntil    *string        `json:"valid_until,omitempty"`
}

// memoryIDStr carries the Python `int | str` memory_id parameter through
// JSON: numbers decode to float64, strings pass through verbatim.
type memoryIDStr struct {
	Raw any `json:"memory_id"`
}

type getMemoryArgs struct {
	memoryIDStr
	UserSlug    string `json:"user_slug"`
	MemoryScope string `json:"memory_scope,omitempty"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	SurfaceID   string `json:"surface_id,omitempty"`
}

type searchMemoryArgs struct {
	UserSlug    string         `json:"user_slug"`
	Query       string         `json:"query"`
	MemoryScope string         `json:"memory_scope,omitempty"`
	WorkspaceID string         `json:"workspace_id,omitempty"`
	SurfaceID   string         `json:"surface_id,omitempty"`
	Limit       *int           `json:"limit,omitempty"`
	MemoryKind  *string        `json:"memory_kind,omitempty"`
	Status      optionalStatus `json:"status,omitempty"`
}

type listMemoriesArgs struct {
	UserSlug    string         `json:"user_slug"`
	MemoryScope string         `json:"memory_scope,omitempty"`
	WorkspaceID string         `json:"workspace_id,omitempty"`
	SurfaceID   string         `json:"surface_id,omitempty"`
	Limit       *int           `json:"limit,omitempty"`
	MemoryKind  *string        `json:"memory_kind,omitempty"`
	Status      optionalStatus `json:"status,omitempty"`
}

type countMemoriesArgs = listMemoriesArgs

// memoryIDArg converts the Python `int | str` memory_id. Returns the parsed
// id, the caller's raw text (for confirmation strings), and an error.
func memoryIDArg(v any) (int64, string, error) {
	switch t := v.(type) {
	case nil:
		return 0, "", fmt.Errorf("memory_id is required")
	case float64:
		// JSON number: render like Python's str(int) after int() coercion.
		return int64(t), strconv.FormatInt(int64(t), 10), nil
	case int64:
		return t, strconv.FormatInt(t, 10), nil
	case int:
		return int64(t), strconv.Itoa(t), nil
	case json.Number:
		n, err := t.Int64()
		if err != nil {
			return 0, "", fmt.Errorf("invalid memory_id: %v", err)
		}
		return n, t.String(), nil
	case string:
		n, err := strconv.ParseInt(t, 10, 64)
		if err != nil {
			return 0, "", fmt.Errorf("invalid literal for int() with base 10: %q", t)
		}
		return n, t, nil
	default:
		return 0, "", fmt.Errorf("invalid memory_id type %T", v)
	}
}

func decodeArgs[T any](req *mcp.CallToolRequest) (T, error) {
	var args T
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return args, err
	}
	return args, nil
}

// ── status filter semantics ───────────────────────────────────────────────
//
// Python's `status: str | None = "active"` distinguishes three cases the
// JSON surface must preserve: omitted → "active" (store default), explicit
// null → no filter (store StatusNoFilter), and a string → filter on it.
// JSON cannot distinguish null from omitted on a *string, so the server
// decodes with a json.RawMessage presence check.

type optionalStatus struct {
	raw json.RawMessage
}

func (o *optionalStatus) UnmarshalJSON(data []byte) error {
	o.raw = append(o.raw[:0], data...)
	return nil
}

// value maps the three JSON cases to the store's *string filter: omitted →
// nil (the store applies its "active" default), null → StatusNoFilter
// (Python status=None), string → the value.
func (o *optionalStatus) value() *string {
	if len(o.raw) == 0 || string(o.raw) == "null" {
		if len(o.raw) > 0 {
			return memory.StatusNoFilter
		}
		return nil
	}
	var s string
	if err := json.Unmarshal(o.raw, &s); err != nil {
		// Non-string value: schema validation rejects this upstream; treat
		// as the default rather than failing the whole call.
		return nil
	}
	return &s
}

// statusFilter converts an optionalStatus to the store's *string filter.
func statusFilter(o *optionalStatus) *string { return o.value() }

// kindFilter converts an optional memory_kind: omitted → "" (no filter),
// null → "" (no filter), string → the value.
func kindFilter(o *string) string {
	if o == nil {
		return ""
	}
	return *o
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

func rawSchema(s string) json.RawMessage { return json.RawMessage(s) }

func main() {
	cli, fs := connector.NewFlagSet(serverName)
	var embeddingModel, embeddingEndpoint string
	var backupKeep int
	fs.StringVar(&embeddingModel, "embedding-model", "",
		"Embedding model id for semantic recall (e.g. 'nomic-embed-text'). "+
			"When omitted, the store stays lexical-only. Also settable via "+
			"LISA_EMBEDDING_MODEL.")
	fs.StringVar(&embeddingEndpoint, "embedding-endpoint", "",
		"OpenAI-compatible embeddings base URL (e.g. 'http://localhost:11434/v1'). "+
			"Required when --embedding-model is set. Also settable via "+
			"LISA_EMBEDDING_ENDPOINT.")
	fs.IntVar(&backupKeep, "backup-keep", backupKeepDefault(),
		"Number of timestamped memory.db dumps to retain under "+
			"<data-dir>/data/backups/ (default 10). Also settable via "+
			"LISA_MEMORY_BACKUP_KEEP.")
	if err := fs.Parse(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, serverName+":", err)
		os.Exit(2)
	}
	if cli.DataDir == "" {
		fmt.Fprintln(os.Stderr, serverName+": --data-dir is required")
		os.Exit(2)
	}
	if cli.ServerID == "" {
		cli.ServerID = serverName
	}

	logger, closeLog, err := connector.SetupLogger(serverName, cli.LogLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, serverName+": logger:", err)
		os.Exit(2)
	}
	defer closeLog()
	logger.Info(serverName+" starting", "data_dir", cli.DataDir)

	// Point-in-time backup before the store opens the DB: dump memory.db to
	// a timestamped SQL file under <data_dir>/data/backups/ and prune
	// beyond keep. Skipped (with a log) when the DB does not exist yet.
	dbPath := filepath.Join(cli.DataDir, dataSubdir, "memory.db")
	if _, err := memory.BackupMemoryDB(cli.DataDir, memory.BackupOptions{
		DBPath: dbPath,
		Keep:   backupKeep,
	}); err != nil {
		logger.Warn("memory backup failed", "err", err)
	}

	// Embedding wiring: CLI flags win over the LISA_EMBEDDING_* env; when
	// either the model or the endpoint is absent the store stays
	// lexical-only (no vector sidecar) — the Python default.
	embCfg, err := memory.ResolveEmbeddingConfig(embeddingModel, embeddingEndpoint)
	if err != nil {
		fmt.Fprintln(os.Stderr, serverName+":", err)
		os.Exit(2)
	}
	var storeOpts []memory.Option
	if embCfg.Provider != nil {
		storeOpts = append(storeOpts, memory.WithEmbeddingProvider(embCfg.Provider, embCfg.Dimension))
	}
	storeOpts = append(storeOpts, memory.WithLogger(logger))
	store, err := memory.New(dbPath, storeOpts...)
	if err != nil {
		logger.Error("init memory store failed", "err", err)
		os.Exit(1)
	}
	defer store.Close()

	server := mcp.NewServer(connector.NewImplementation("Lisa Memory Server"), nil)
	addMemoryTools(server, store)

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		logger.Error("server exited", "err", err)
		os.Exit(1)
	}
}

// backupKeepDefault resolves the backup retention count from env, defaulting
// to 10 (parity: the Python _backup_keep_default clamps to >= 1 and warns on
// invalid input instead of failing startup).
func backupKeepDefault() int {
	raw := os.Getenv("LISA_MEMORY_BACKUP_KEEP")
	if raw == "" {
		return memory.DefaultKeepBackups
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return memory.DefaultKeepBackups
	}
	return n
}

// scopeOpts builds the shared scope/filters from the common optional fields.
func scopeOpts(memoryScope, workspaceID, surfaceID string) memory.ListOptions {
	if memoryScope == "" {
		memoryScope = "personal"
	}
	if workspaceID == "" {
		workspaceID = "default"
	}
	return memory.ListOptions{MemoryScope: memoryScope, WorkspaceID: workspaceID, SurfaceID: surfaceID}
}

func addMemoryTools(server *mcp.Server, store *memory.Store) {
	// add_memory: store a scoped memory snippet with surface-scoped privacy
	// isolation. The model-facing description is kept verbatim from the
	// Python tool (it carries the curation contract).
	server.AddTool(&mcp.Tool{
		Name: "add_memory",
		Description: "Store a new memory snippet scoped to the provided user identity and surface. " +
			"Enforces Lisa's surface-scoped privacy isolation. " +
			"Curation contract — memory is ONLY what is true beyond this thread " +
			"(the thread log already records what happened). Do NOT store: " +
			"ephemeral tool/MCP errors, registration gaps, session drops; " +
			"iteration-budget pauses or execution-flow state; " +
			"point-in-time volatile infrastructure status (query it live instead). " +
			"DO store durable user preferences, stable architectural invariants, " +
			"verified problem/solution pairs, and permanent project configuration. " +
			"Optional metadata fields (memory_tier, memory_kind, operation_id, " +
			"instruction_id, content_json, salience, supersedes_id, item_ids, " +
			"confidence, valid_until) let callers control retention, provenance, " +
			"ranking, and consolidation supersession. When omitted, sensible " +
			"defaults are applied so the tool remains usable as a simple " +
			"'store this' call.",
		InputSchema: rawSchema(`{
			"type": "object",
			"properties": {
				"user_slug":      {"type": "string", "description": "The user identity to scope the memory to."},
				"content":        {"type": "string", "description": "The memory text to store."},
				"memory_scope":   {"type": "string", "description": "Privacy scope (personal, shared, channel). Default personal."},
				"workspace_id":   {"type": "string", "description": "Workspace isolation key. Default 'default'."},
				"surface_id":     {"type": "string", "description": "Surface isolation key (e.g. channel id). Default ''."},
				"memory_tier":    {"type": "string", "description": "Retention tier (daily, weekly, long_term, permanent). Controls TTL expiry. Default daily."},
				"memory_kind":    {"type": "string", "description": "Cognitive kind (episodic, semantic, procedural, reflection, preference). Default episodic."},
				"operation_id":   {"type": "string", "description": "Provenance tag identifying the operation that created this memory. Default mcp.add_memory."},
				"instruction_id": {"type": "string", "description": "Provenance tag for the instruction version. Default mcp.v1."},
				"content_json":   {"type": "object", "description": "Structured metadata dict stored alongside content."},
				"salience":       {"type": "number", "description": "Ranking weight (0.0-1.0) for recall ordering. Default 0.0."},
				"supersedes_id":  {"type": "integer", "description": "Id of the record this one supersedes (consolidation)."},
				"item_ids":       {"type": "array", "items": {"type": "integer"}, "description": "Ids of the records this one was derived from."},
				"confidence":     {"type": "number", "description": "Confidence in the memory (0.0-1.0). Default 1.0."},
				"valid_until":    {"type": "string", "description": "Optional semantic-validity bound (ISO 8601); overrides the tier TTL when set."}
			},
			"required": ["user_slug", "content"]
		}`),
		Annotations: defaultAnnotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := decodeArgs[addMemoryArgs](req)
		if err != nil {
			return nil, err
		}
		// content_json default {"source": "mcp_tool"} — mirrors the Python
		// tool's `content_json or {"source": "mcp_tool"}` provenance (the
		// store's own default is {} for non-MCP callers).
		contentJSON := args.ContentJSON
		if contentJSON == nil {
			contentJSON = map[string]any{"source": "mcp_tool"}
		}
		record, err := store.RecordMemory(ctx, memory.MemoryWriteRequest{
			UserSlug:      args.UserSlug,
			Content:       args.Content,
			MemoryScope:   stringDefault(args.MemoryScope, "personal"),
			WorkspaceID:   stringDefault(args.WorkspaceID, "default"),
			SurfaceID:     args.SurfaceID,
			OperationID:   stringDefault(args.OperationID, "mcp.add_memory"),
			InstructionID: stringDefault(args.InstructionID, "mcp.v1"),
			MemoryTier:    stringDefault(args.MemoryTier, "daily"),
			MemoryKind:    stringDefault(args.MemoryKind, "episodic"),
			ContentJSON:   contentJSON,
			Salience:      args.Salience,
			SupersedesID:  args.SupersedesID,
			ItemIDs:       args.ItemIDs,
			Confidence:    confidenceValue(args.Confidence),
			ValidUntil:    args.ValidUntil,
		})
		if err != nil {
			return toolTextErr(err)
		}
		return toolText(fmt.Sprintf("Memory stored with ID %d", record.ID))
	})

	// search_memory: keyword recall for one user. Returns a JSON array of
	// memory records ("[]" when empty) so the recall_memory operation can
	// parse the result.
	server.AddTool(&mcp.Tool{
		Name: "search_memory",
		Description: "Search for surface-scoped memories for a specific user. " +
			"Records from other surfaces or users cannot be accessed. " +
			"The recall engine uses lexical-overlap ranking with a salience boost, " +
			"falling back to semantic KNN when embeddings are available. When no " +
			"query tokens match any stored content, the most recent records are " +
			"returned (ordered by id DESC) so the caller always gets context. " +
			"Returns a JSON array of memory records so the recall_memory " +
			"operation can parse the result. Returns \"[]\" when the scoped " +
			"store is empty. status defaults to \"active\" so retired/superseded " +
			"records drop out of recall automatically; pass null to include all.",
		InputSchema: rawSchema(`{
			"type": "object",
			"properties": {
				"user_slug":    {"type": "string", "description": "The user identity to scope the search to."},
				"query":        {"type": "string", "description": "The recall query."},
				"memory_scope": {"type": "string", "description": "Privacy scope. Default personal."},
				"workspace_id": {"type": "string", "description": "Workspace isolation key. Default 'default'."},
				"surface_id":   {"type": "string", "description": "Surface isolation key. Default ''."},
				"limit":        {"type": "integer", "description": "Max records to return (default 5)."},
				"memory_kind":  {"type": "string", "description": "Filter by episodic/semantic/procedural/reflection/preference."},
				"status":       {"type": ["string", "null"], "description": "Record status filter; null includes all. Default 'active'."}
			},
			"required": ["user_slug", "query"]
		}`),
		Annotations: readOnlyAnnotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := decodeArgs[searchMemoryArgs](req)
		if err != nil {
			return nil, err
		}
		limit := 5
		if args.Limit != nil {
			limit = *args.Limit
		}
		records, err := store.RecallRecords(ctx, args.Query, args.UserSlug, memory.RecallOptions{
			WorkspaceID: stringDefault(args.WorkspaceID, "default"),
			MemoryScope: stringDefault(args.MemoryScope, "personal"),
			SurfaceID:   args.SurfaceID,
			Limit:       limit,
			MemoryKind:  kindFilter(args.MemoryKind),
			Status:      statusFilter(&args.Status),
		})
		if err != nil {
			return toolTextErr(err)
		}
		return toolText(recordsJSON(records))
	})

	// get_memory: fetch a single record by id, scoped. JSON object; tool
	// error when no record matches.
	server.AddTool(&mcp.Tool{
		Name: "get_memory",
		Description: "Fetch a single memory record by id, scoped to the caller's surface. " +
			"Returns the record as a JSON object. Returns an error when no " +
			"record matches the id within the caller's surface scope. " +
			"Cross-surface reads are impossible — the scope filters are " +
			"applied server-side.",
		InputSchema: rawSchema(`{
			"type": "object",
			"properties": {
				"memory_id":    {"type": ["integer", "string"], "description": "Record id."},
				"user_slug":    {"type": "string", "description": "The user identity to scope to."},
				"memory_scope": {"type": "string", "description": "Privacy scope. Default personal."},
				"workspace_id": {"type": "string", "description": "Workspace isolation key. Default 'default'."},
				"surface_id":   {"type": "string", "description": "Surface isolation key. Default ''."}
			},
			"required": ["memory_id", "user_slug"]
		}`),
		Annotations: readOnlyAnnotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := decodeArgs[getMemoryArgs](req)
		if err != nil {
			return nil, err
		}
		memoryID, _, err := memoryIDArg(args.Raw)
		if err != nil {
			return toolTextErr(err)
		}
		record, err := store.GetMemory(ctx, memoryID, args.UserSlug, scopeOpts(args.MemoryScope, args.WorkspaceID, args.SurfaceID))
		if err != nil {
			return toolTextErr(err)
		}
		if record == nil {
			return toolTextErr(errNoMemoryFound)
		}
		out, err := json.Marshal(record)
		if err != nil {
			return toolTextErr(err)
		}
		return toolText(string(out))
	})

	// delete_memory: remove a single record by id, scoped.
	server.AddTool(&mcp.Tool{
		Name: "delete_memory",
		Description: "Delete a single memory record by id, scoped to the caller's surface. " +
			"Returns \"Deleted memory <id>.\" on success. Returns an error when " +
			"no record matched the id within the caller's surface scope. " +
			"Cross-surface deletion is impossible — the scope filters are " +
			"applied server-side.",
		InputSchema: rawSchema(`{
			"type": "object",
			"properties": {
				"memory_id":    {"type": ["integer", "string"], "description": "Record id."},
				"user_slug":    {"type": "string", "description": "The user identity to scope to."},
				"memory_scope": {"type": "string", "description": "Privacy scope. Default personal."},
				"workspace_id": {"type": "string", "description": "Workspace isolation key. Default 'default'."},
				"surface_id":   {"type": "string", "description": "Surface isolation key. Default ''."}
			},
			"required": ["memory_id", "user_slug"]
		}`),
		Annotations: destructiveAnnotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := decodeArgs[getMemoryArgs](req)
		if err != nil {
			return nil, err
		}
		memoryID, idText, err := memoryIDArg(args.Raw)
		if err != nil {
			return toolTextErr(err)
		}
		deleted, err := store.DeleteMemory(ctx, memoryID, args.UserSlug, scopeOpts(args.MemoryScope, args.WorkspaceID, args.SurfaceID))
		if err != nil {
			return toolTextErr(err)
		}
		if !deleted {
			return toolTextErr(errNoMemoryFound)
		}
		return toolText(fmt.Sprintf("Deleted memory %s.", renderMemoryID(idText, memoryID)))
	})

	// list_memories: raw JSON array of recent records, most recent first.
	server.AddTool(&mcp.Tool{
		Name: "list_memories",
		Description: "Returns a raw JSON array string of memory records. " +
			"status defaults to \"active\" so retired records are excluded; " +
			"pass null to list all statuses. memory_kind filters by " +
			"episodic/semantic/… when given.",
		InputSchema: rawSchema(`{
			"type": "object",
			"properties": {
				"user_slug":    {"type": "string", "description": "The user identity to scope to."},
				"memory_scope": {"type": "string", "description": "Privacy scope. Default personal."},
				"workspace_id": {"type": "string", "description": "Workspace isolation key. Default 'default'."},
				"surface_id":   {"type": "string", "description": "Surface isolation key. Default ''."},
				"limit":        {"type": "integer", "description": "Max records (default 20)."},
				"memory_kind":  {"type": "string", "description": "Filter by episodic/semantic/procedural/reflection/preference."},
				"status":       {"type": ["string", "null"], "description": "Record status filter; null includes all. Default 'active'."}
			},
			"required": ["user_slug"]
		}`),
		Annotations: readOnlyAnnotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := decodeArgs[listMemoriesArgs](req)
		if err != nil {
			return nil, err
		}
		limit := 20
		if args.Limit != nil {
			limit = *args.Limit
		}
		records, err := store.ListMemories(ctx, args.UserSlug, memory.ListOptions{
			WorkspaceID: stringDefault(args.WorkspaceID, "default"),
			MemoryScope: stringDefault(args.MemoryScope, "personal"),
			SurfaceID:   args.SurfaceID,
			Limit:       limit,
			MemoryKind:  kindFilter(args.MemoryKind),
			Status:      statusFilter(&args.Status),
		})
		if err != nil {
			return toolTextErr(err)
		}
		return toolText(recordsJSON(records))
	})

	// retire_memory: mark a record retired (audit trail preserved).
	server.AddTool(&mcp.Tool{
		Name: "retire_memory",
		Description: "Retire a memory record by id, scoped to the caller's surface. " +
			"Retirement (not deletion) preserves the audit trail — the record " +
			"stays queryable with a null status filter but drops out of the " +
			"default active recall pool. Used by consolidation to retire " +
			"episodic originals after a semantic successor is written. " +
			"Returns \"Retired memory <id>.\" on success. Returns an error " +
			"when no active record matched the id within the caller's surface scope.",
		InputSchema: rawSchema(`{
			"type": "object",
			"properties": {
				"memory_id":    {"type": ["integer", "string"], "description": "Record id."},
				"user_slug":    {"type": "string", "description": "The user identity to scope to."},
				"memory_scope": {"type": "string", "description": "Privacy scope. Default personal."},
				"workspace_id": {"type": "string", "description": "Workspace isolation key. Default 'default'."},
				"surface_id":   {"type": "string", "description": "Surface isolation key. Default ''."}
			},
			"required": ["memory_id", "user_slug"]
		}`),
		Annotations: destructiveAnnotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := decodeArgs[getMemoryArgs](req)
		if err != nil {
			return nil, err
		}
		memoryID, idText, err := memoryIDArg(args.Raw)
		if err != nil {
			return toolTextErr(err)
		}
		retired, err := store.RetireMemory(ctx, memoryID, args.UserSlug, scopeOpts(args.MemoryScope, args.WorkspaceID, args.SurfaceID))
		if err != nil {
			return toolTextErr(err)
		}
		if !retired {
			return toolTextErr(errNoActiveMemoryFound)
		}
		return toolText(fmt.Sprintf("Retired memory %s.", renderMemoryID(idText, memoryID)))
	})

	// count_memories: count matching records.
	server.AddTool(&mcp.Tool{
		Name: "count_memories",
		Description: "Return the count of memory records matching the scope and filters. " +
			"status defaults to \"active\" so retired records are excluded; " +
			"pass null to count all statuses. memory_kind filters by " +
			"episodic/semantic/… when given.",
		InputSchema: rawSchema(`{
			"type": "object",
			"properties": {
				"user_slug":    {"type": "string", "description": "The user identity to scope to."},
				"memory_scope": {"type": "string", "description": "Privacy scope. Default personal."},
				"workspace_id": {"type": "string", "description": "Workspace isolation key. Default 'default'."},
				"surface_id":   {"type": "string", "description": "Surface isolation key. Default ''."},
				"memory_kind":  {"type": "string", "description": "Filter by episodic/semantic/procedural/reflection/preference."},
				"status":       {"type": ["string", "null"], "description": "Record status filter; null includes all. Default 'active'."}
			},
			"required": ["user_slug"]
		}`),
		Annotations: readOnlyAnnotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := decodeArgs[countMemoriesArgs](req)
		if err != nil {
			return nil, err
		}
		count, err := store.CountMemories(ctx, args.UserSlug, memory.ListOptions{
			WorkspaceID: stringDefault(args.WorkspaceID, "default"),
			MemoryScope: stringDefault(args.MemoryScope, "personal"),
			SurfaceID:   args.SurfaceID,
			MemoryKind:  kindFilter(args.MemoryKind),
			Status:      statusFilter(&args.Status),
		})
		if err != nil {
			return toolTextErr(err)
		}
		return toolText(strconv.Itoa(count))
	})
}

// errNoMemoryFound mirrors the Python ToolError("No memory found.").
var errNoMemoryFound = fmt.Errorf("No memory found.")

// errNoActiveMemoryFound mirrors ToolError("No active memory found.").
var errNoActiveMemoryFound = fmt.Errorf("No active memory found.")

// recordsJSON serializes records the way search_memory/list_memories return
// them: a JSON array ("[]" when empty — never nil/null). The Python server
// serializes model_dump(mode="json") dicts; the Go record's JSON tags carry
// the same field names.
func recordsJSON(records []memory.MemoryRecord) string {
	if len(records) == 0 {
		return "[]"
	}
	out, err := json.Marshal(records)
	if err != nil {
		return "[]"
	}
	return string(out)
}

func stringDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func confidenceValue(v *float64) float64 {
	if v == nil {
		return 1.0
	}
	return *v
}

// renderMemoryID formats the confirmation string with the caller's id text
// (Python embeds the argument verbatim: f"Deleted memory {memory_id}.").
func renderMemoryID(rawText string, id int64) string {
	if rawText == "" {
		return strconv.FormatInt(id, 10)
	}
	return rawText
}
