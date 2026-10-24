// End-to-end tests for the lisa-memory Go MCP server. These spawn the real
// binary over stdio via CommandTransport and drive it with the SDK's own
// client — exactly how the Lisa runtime spawns it (RuntimeKind.BINARY).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// buildBinary compiles the lisa-memory binary once per test.
func buildBinary(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "lisa-memory-test.exe")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build lisa-memory: %v", err)
	}
	return out
}

// connect spawns the server binary and returns a connected client session.
func connect(t *testing.T, bin, dataDir string) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, bin,
		"--data-dir", dataDir,
		"--log-level", "ERROR",
	)
	transport := &mcp.CommandTransport{Command: cmd}
	client := mcp.NewClient(&mcp.Implementation{Name: "lisa-memory-test", Version: "0.1.0"}, nil)
	cs, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// callText invokes a tool and returns its text payload, failing the test on
// a tool error.
func callText(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("call %s returned error: %v", name, res.Content)
	}
	if len(res.Content) == 0 {
		t.Fatalf("call %s: empty content", name)
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("call %s: unexpected content type %T", name, res.Content[0])
	}
	return tc.Text
}

// callErr invokes a tool and returns its text payload, expecting an error
// result (the CallToolResult(is_error=true) path the Python ToolError maps
// to).
func callErr(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	if !res.IsError {
		t.Fatalf("call %s: expected error result, got %v", name, res.Content)
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("call %s: unexpected error content type %T", name, res.Content[0])
	}
	return tc.Text
}

// setup builds the binary and connects.
func setup(t *testing.T) *mcp.ClientSession {
	t.Helper()
	bin := buildBinary(t)
	return connect(t, bin, filepath.Join(t.TempDir(), "data"))
}

func TestServerListsTools(t *testing.T) {
	cs := setup(t)
	res, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	names := map[string]bool{}
	for _, tool := range res.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{
		"add_memory", "search_memory", "list_memories",
		"get_memory", "delete_memory", "retire_memory", "count_memories",
	} {
		if !names[want] {
			t.Errorf("expected tool %s, got %v", want, names)
		}
	}
	if len(res.Tools) != 7 {
		t.Errorf("expected exactly 7 tools, got %d (%v)", len(res.Tools), names)
	}
}

// add stores one memory and returns its parsed record id ("Memory stored
// with ID N").
func add(t *testing.T, cs *mcp.ClientSession, content string) int64 {
	t.Helper()
	out := callText(t, cs, "add_memory", map[string]any{
		"user_slug": "adam",
		"content":   content,
	})
	var id int64
	if _, err := fmt.Sscanf(out, "Memory stored with ID %d", &id); err != nil {
		t.Fatalf("unexpected add response %q: %v", out, err)
	}
	return id
}

func mustDecodeArray(t *testing.T, out string) []map[string]any {
	t.Helper()
	var decoded []map[string]any
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("unmarshal %q: %v", out, err)
	}
	return decoded
}

func TestFullRoundTrip(t *testing.T) {
	cs := setup(t)

	id := add(t, cs, "We chose PostgreSQL for the control plane.")

	// get_memory returns the record as a JSON object.
	got := callText(t, cs, "get_memory", map[string]any{"memory_id": id, "user_slug": "adam"})
	var record map[string]any
	if err := json.Unmarshal([]byte(got), &record); err != nil {
		t.Fatalf("get_memory unmarshal %q: %v", got, err)
	}
	if record["content"] != "We chose PostgreSQL for the control plane." {
		t.Errorf("get content = %v", record["content"])
	}
	if record["status"] != "active" {
		t.Errorf("get status = %v, want active", record["status"])
	}

	// list_memories returns the record as a JSON array.
	listed := mustDecodeArray(t, callText(t, cs, "list_memories", map[string]any{"user_slug": "adam"}))
	if len(listed) != 1 {
		t.Fatalf("list len = %d, want 1", len(listed))
	}

	// search_memory finds it.
	found := mustDecodeArray(t, callText(t, cs, "search_memory", map[string]any{
		"user_slug": "adam", "query": "PostgreSQL control plane",
	}))
	if len(found) != 1 {
		t.Fatalf("search len = %d, want 1: %v", len(found), found)
	}

	// count_memories counts it.
	if got := callText(t, cs, "count_memories", map[string]any{"user_slug": "adam"}); got != "1" {
		t.Errorf("count = %q, want 1", got)
	}

	// retire_memory marks it retired and drops it from the default list.
	retiredOut := callText(t, cs, "retire_memory", map[string]any{"memory_id": id, "user_slug": "adam"})
	if retiredOut != "Retired memory 1." {
		t.Errorf("retire response = %q", retiredOut)
	}
	if got := callText(t, cs, "count_memories", map[string]any{"user_slug": "adam"}); got != "0" {
		t.Errorf("count after retire = %q, want 0", got)
	}

	// Re-retiring is a no-op error (no active record).
	if msg := callErr(t, cs, "retire_memory", map[string]any{"memory_id": id, "user_slug": "adam"}); msg != "No active memory found." {
		t.Errorf("re-retire error = %q", msg)
	}

	// delete_memory removes it permanently; the follow-up get errors.
	delOut := callText(t, cs, "delete_memory", map[string]any{"memory_id": id, "user_slug": "adam"})
	if delOut != "Deleted memory 1." {
		t.Errorf("delete response = %q", delOut)
	}
	if msg := callErr(t, cs, "get_memory", map[string]any{"memory_id": id, "user_slug": "adam"}); msg != "No memory found." {
		t.Errorf("get after delete error = %q", msg)
	}
	if got := callText(t, cs, "list_memories", map[string]any{"user_slug": "adam"}); got != "[]" {
		t.Errorf("list empty = %q, want []", got)
	}
}

func TestSearchReturnsEmptyArray(t *testing.T) {
	cs := setup(t)
	if got := callText(t, cs, "search_memory", map[string]any{
		"user_slug": "adam", "query": "nothing stored yet",
	}); got != "[]" {
		t.Errorf("search = %q, want []", got)
	}
}

func TestCrossSurfaceIsolation(t *testing.T) {
	cs := setup(t)
	id := add(t, cs, "private channel note")
	// Same user, wrong surface: not found.
	if msg := callErr(t, cs, "get_memory", map[string]any{
		"memory_id": id, "user_slug": "adam", "surface_id": "other-surface",
	}); msg != "No memory found." {
		t.Errorf("cross-surface get = %q, want not-found", msg)
	}
	// Cross-surface deletion is impossible.
	if msg := callErr(t, cs, "delete_memory", map[string]any{
		"memory_id": id, "user_slug": "adam", "surface_id": "other-surface",
	}); msg != "No memory found." {
		t.Errorf("cross-surface delete = %q, want not-found", msg)
	}
	// The record is still there on its own surface.
	if got := callText(t, cs, "count_memories", map[string]any{"user_slug": "adam"}); got != "1" {
		t.Errorf("count after cross-surface attempts = %q, want 1", got)
	}
}

func TestUserIsolation(t *testing.T) {
	cs := setup(t)
	id := add(t, cs, "adam secret")
	if msg := callErr(t, cs, "get_memory", map[string]any{"memory_id": id, "user_slug": "nadia"}); msg != "No memory found." {
		t.Errorf("cross-user get = %q, want not-found", msg)
	}
}

func TestStringMemoryIDAccepted(t *testing.T) {
	cs := setup(t)
	add(t, cs, "string id probe")
	// The Python signature takes int | str; a string id must resolve.
	got := callText(t, cs, "get_memory", map[string]any{
		"memory_id": "1", "user_slug": "adam",
	})
	var record map[string]any
	if err := json.Unmarshal([]byte(got), &record); err != nil {
		t.Fatalf("get unmarshal %q: %v", got, err)
	}
	if record["content"] != "string id probe" {
		t.Errorf("content = %v", record["content"])
	}
}

// TestAddMemoryDefaultsContentJSONProvenance locks the Python tool parity:
// content_json omitted on add_memory becomes {"source": "mcp_tool"}.
func TestAddMemoryDefaultsContentJSONProvenance(t *testing.T) {
	cs := setup(t)
	id := add(t, cs, "provenance probe")
	got := callText(t, cs, "get_memory", map[string]any{"memory_id": id, "user_slug": "adam"})
	var record map[string]any
	if err := json.Unmarshal([]byte(got), &record); err != nil {
		t.Fatalf("get unmarshal %q: %v", got, err)
	}
	cj, ok := record["content_json"].(map[string]any)
	if !ok {
		t.Fatalf("content_json type = %T, want object", record["content_json"])
	}
	if cj["source"] != "mcp_tool" {
		t.Errorf("content_json.source = %v, want mcp_tool", cj["source"])
	}
}

func TestBackupsDirectoryPopulatedOnSecondBoot(t *testing.T) {
	bin := buildBinary(t)
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")

	// First boot: DB does not exist — no backup is written, the store
	// initializes.
	cs1 := connect(t, bin, dataDir)
	add(t, cs1, "survives backup round-trip")
	cs1.Close()

	// The default keep is 10; a second boot must produce exactly one
	// timestamped dump and never prune the fresh one.
	cs2 := connect(t, bin, dataDir)
	defer cs2.Close()
	backupDir := filepath.Join(dataDir, "data", "backups")
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatalf("read backups dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 backup, got %d", len(entries))
	}
	name := entries[0].Name()
	if len(name) < len("memory-") || name[:len("memory-")] != "memory-" || filepath.Ext(name) != ".sql" {
		t.Errorf("backup name %q malformed", name)
	}
	// The dump must be restorable SQLite text: schema + data INSERTs.
	raw, err := os.ReadFile(filepath.Join(backupDir, name))
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	sqlText := string(raw)
	for _, want := range []string{"BEGIN TRANSACTION;", "CREATE TABLE", "INSERT INTO", "COMMIT;"} {
		if !containsStr(sqlText, want) {
			t.Errorf("dump missing %q: %q", want, sqlText)
		}
	}
	if !containsStr(sqlText, "survives backup round-trip") {
		t.Errorf("dump missing memory content")
	}
}

// TestRetiredExcludedByDefaultAndNullStatusIncludesAll locks the JSON
// status-semantics parity: an omitted status defaults to "active" while an
// explicit null maps to Python status=None (no filter).
func TestRetiredExcludedByDefaultAndNullStatusIncludesAll(t *testing.T) {
	cs := setup(t)
	id := add(t, cs, "retire probe")
	callText(t, cs, "retire_memory", map[string]any{"memory_id": id, "user_slug": "adam"})

	// Omitted status → active default → record hidden.
	if got := callText(t, cs, "list_memories", map[string]any{"user_slug": "adam"}); got != "[]" {
		t.Errorf("default list = %q, want []", got)
	}
	// Explicit null → no filter → record visible.
	listed := mustDecodeArray(t, callText(t, cs, "list_memories", map[string]any{
		"user_slug": "adam", "status": nil,
	}))
	if len(listed) != 1 || listed[0]["status"] != "retired" {
		t.Errorf("null_status list = %v", listed)
	}
}

// ── tiny test helpers ─────────────────────────────────────────────────────

func containsStr(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
