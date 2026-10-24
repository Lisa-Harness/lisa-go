// End-to-end tests for the lisa-storage Go MCP server. These spawn the
// real binary over stdio via CommandTransport and drive it with the SDK's
// own client — exactly how the Lisa runtime spawns it (RuntimeKind.BINARY).
package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// buildBinary compiles the lisa-storage binary once per test.
func buildBinary(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "lisa-storage-test.exe")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build lisa-storage: %v", err)
	}
	return out
}

// connect spawns the server binary and returns a connected client session.
func connect(t *testing.T, bin, dataDir string, extra ...string) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	args := append([]string{"--data-dir", dataDir, "--log-level", "ERROR"}, extra...)
	cmd := exec.CommandContext(ctx, bin, args...)
	transport := &mcp.CommandTransport{Command: cmd}
	client := mcp.NewClient(&mcp.Implementation{Name: "lisa-storage-test", Version: "0.1.0"}, nil)
	cs, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// callText invokes a tool and returns its text payload, failing the test
// on transport or tool-level (IsError) errors.
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

// callError invokes a tool expecting rejection: either a transport error
// or a CallToolResult with IsError=true (the {"error": ...} path).
func callError(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return err.Error()
	}
	if !res.IsError {
		t.Fatalf("call %s(%v) expected error, got success: %v", name, args, res.Content)
	}
	if len(res.Content) == 0 {
		t.Fatalf("call %s: error result with empty content", name)
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("call %s: unexpected content type %T", name, res.Content[0])
	}
	return tc.Text
}

func TestServerListsTools(t *testing.T) {
	bin := buildBinary(t)
	dataDir := t.TempDir()
	cs := connect(t, bin, dataDir)

	res, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	names := map[string]bool{}
	for _, tool := range res.Tools {
		names[tool.Name] = true
	}
	want := []string{
		"read_file", "read_file_range", "write_file", "append_file",
		"list_files", "delete_file", "stat_file", "make_dir",
		"read_model_manifest", "write_model_manifest",
	}
	for _, name := range want {
		if !names[name] {
			t.Errorf("expected tool %q, got %v", name, names)
		}
	}
	if len(res.Tools) != len(want) {
		t.Errorf("expected %d tools, got %d: %v", len(want), len(res.Tools), names)
	}
}

// TestPathTraversalRejected: "../x" and absolute paths must never reach
// the filesystem — read, write, and delete all reject them.
func TestPathTraversalRejected(t *testing.T) {
	bin := buildBinary(t)
	dataDir := t.TempDir()
	cs := connect(t, bin, dataDir)

	bad := []string{"../x", "ok/../../x", "/etc/passwd", `C:\Windows\system.ini`}
	for _, p := range bad {
		for _, tool := range []string{"read_file", "write_file", "delete_file", "stat_file"} {
			args := map[string]any{"path": p}
			if tool == "write_file" {
				args["content"] = "nope"
			}
			if out := callError(t, cs, tool, args); !strings.Contains(out, "rejected") {
				t.Errorf("%s(%q): error %q does not mention rejection", tool, p, out)
			}
		}
	}

	// Nothing escaped into the data dir outside the files sandbox.
	matches, err := filepath.Glob(filepath.Join(dataDir, "x"))
	if err == nil && len(matches) > 0 {
		t.Errorf("escape artefact found outside sandbox: %v", matches)
	}
}

// TestDenyGlobPolicy: a --deny-glob "*.db" passed to the binary denies
// matching writes, reads, and listings (case-insensitively).
func TestDenyGlobPolicy(t *testing.T) {
	bin := buildBinary(t)
	dataDir := t.TempDir()
	cs := connect(t, bin, dataDir, "--deny-glob", "*.db")

	if out := callError(t, cs, "write_file", map[string]any{"path": "state.db", "content": "nope"}); !strings.Contains(out, "denied") {
		t.Errorf("write *.db: expected denial, got %q", out)
	}
	// Case-insensitive: .DB matches "*.db".
	if out := callError(t, cs, "write_file", map[string]any{"path": "upper.DB", "content": "nope"}); !strings.Contains(out, "denied") {
		t.Errorf("write *.DB: expected case-insensitive denial, got %q", out)
	}
	if out := callError(t, cs, "read_file", map[string]any{"path": "state.db"}); !strings.Contains(out, "denied") {
		t.Errorf("read *.db: expected denial, got %q", out)
	}

	// Denied entries are filtered from listings; allowed files still work.
	callText(t, cs, "write_file", map[string]any{"path": "ok.txt", "content": "fine"})
	out := callText(t, cs, "list_files", map[string]any{})
	if strings.Contains(out, ".db") {
		t.Errorf("denied .db leaked into listing: %s", out)
	}
	if !strings.Contains(out, "ok.txt") {
		t.Errorf("ok.txt missing from listing: %s", out)
	}
}

// TestReadonlyMode: with --readonly, write_file returns the read-only
// error payload.
func TestReadonlyMode(t *testing.T) {
	bin := buildBinary(t)
	dataDir := t.TempDir()
	cs := connect(t, bin, dataDir, "--readonly")

	if out := callError(t, cs, "write_file", map[string]any{"path": "w.txt", "content": "nope"}); !strings.Contains(out, "server is read-only") {
		t.Errorf("readonly write: expected \"server is read-only\", got %q", out)
	}
}

// TestStorageRoundTrip: write → append → stat → list → read → delete,
// asserting the file physically lands under <data-dir>/data/files/.
func TestStorageRoundTrip(t *testing.T) {
	bin := buildBinary(t)
	dataDir := t.TempDir()
	cs := connect(t, bin, dataDir)

	// make_dir creates the parent.
	callText(t, cs, "make_dir", map[string]any{"path": "notes"})

	// write adds the trailing newline and reports it.
	w := callText(t, cs, "write_file", map[string]any{"path": "notes/a.txt", "content": "line one"})
	if !strings.Contains(w, `"trailing_newline_added":true`) {
		t.Errorf("write receipt: %s", w)
	}

	// The file must live under <data-dir>/data/files/.
	disk := filepath.Join(dataDir, "data", "files", "notes", "a.txt")
	if _, err := os.Stat(disk); err != nil {
		t.Fatalf("expected file under data/files: %v", err)
	}

	// append, then read the combined content.
	callText(t, cs, "append_file", map[string]any{"path": "notes/a.txt", "content": "line two\n"})
	if got := callText(t, cs, "read_file", map[string]any{"path": "notes/a.txt"}); got != "line one\nline two\n" {
		t.Errorf("read after append = %q", got)
	}

	// stat reports size and is_dir.
	s := callText(t, cs, "stat_file", map[string]any{"path": "notes/a.txt"})
	var stat map[string]any
	if err := json.Unmarshal([]byte(s), &stat); err != nil {
		t.Fatalf("stat unmarshal: %v (%q)", err, s)
	}
	if stat["is_dir"].(bool) || int(stat["size"].(float64)) != len("line one\nline two\n") {
		t.Errorf("unexpected stat: %v", stat)
	}

	// read_file_range slices by chars.
	r := callText(t, cs, "read_file_range", map[string]any{"path": "notes/a.txt", "start": 5, "length": 3})
	var rr struct {
		Content string `json:"content"`
		EOF     bool   `json:"eof"`
	}
	if err := json.Unmarshal([]byte(r), &rr); err != nil {
		t.Fatalf("range unmarshal: %v (%q)", err, r)
	}
	if rr.Content != "one" || rr.EOF {
		t.Errorf("unexpected range result: %+v", rr)
	}

	// list under the prefix names the file.
	l := callText(t, cs, "list_files", map[string]any{"prefix": "notes"})
	if !strings.Contains(l, "notes/a.txt") {
		t.Errorf("list_files: %s", l)
	}

	// delete removes it; a subsequent read fails with not-found.
	callText(t, cs, "delete_file", map[string]any{"path": "notes/a.txt"})
	if out := callError(t, cs, "read_file", map[string]any{"path": "notes/a.txt"}); !strings.Contains(out, "not found") {
		t.Errorf("read after delete: %q", out)
	}
	if _, err := os.Stat(disk); !os.IsNotExist(err) {
		t.Errorf("file still on disk after delete: %v", err)
	}

	// Model manifests live via their own tools (byte-faithful, no
	// newline normalization).
	callText(t, cs, "write_model_manifest", map[string]any{"content": "models: []", "which": "proposal"})
	if got := callText(t, cs, "read_model_manifest", map[string]any{"which": "proposal"}); got != "models: []" {
		t.Errorf("read_model_manifest = %q", got)
	}
}
