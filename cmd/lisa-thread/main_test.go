// End-to-end tests for the lisa-thread Go MCP server. These spawn the real
// binary over stdio via CommandTransport and drive it with the SDK's own
// client — exactly how the Lisa runtime spawns it (RuntimeKind.BINARY).
// The thread fixture is synthetic: <base>.current.jsonl plus a sealed
// <base>.<segid>.jsonl.zst segment and its <base>.index.jsonl, written by
// the test (zstd via the already-vendored klauspost/compress).
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Lisa-Harness/lisa-go/connector/thread"
)

// buildBinary compiles the lisa-thread binary once per test.
func buildBinary(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "lisa-thread-test.exe")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build lisa-thread: %v", err)
	}
	return out
}

// writeFixture writes a synthetic thread store at base:
//   - one sealed segment (<base>.seg-0001.jsonl.zst + <base>.index.jsonl)
//     holding entries 0-1;
//   - the active segment <base>.current.jsonl holding entries 2-4, one of
//     which contains a default redaction pattern and one a searchable token.
func writeFixture(t *testing.T, base string) {
	t.Helper()
	sealed := []map[string]any{
		{
			"timestamp_utc": "2026-10-20T00:00:00Z",
			"query":         "sealed question about ALPHA_TOKEN",
			"response_text": "sealed answer",
			"mode_id":       "chat",
			"task_id":       "task-alpha",
			"provenance":    map[string]any{"source": "model"},
		},
		{
			"timestamp_utc": "2026-10-20T00:01:00Z",
			"query":         "second sealed question",
			"response_text": "I've hit my iteration budget in chat (cap 5).",
			"mode_id":       "chat",
			"task_id":       "task-alpha",
			"provenance":    map[string]any{"source": "model"},
		},
	}
	var segLines [][]byte
	for _, e := range sealed {
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal sealed entry: %v", err)
		}
		segLines = append(segLines, line)
	}
	payload := bytes.Join(segLines, []byte("\n"))
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd encoder: %v", err)
	}
	compressed := enc.EncodeAll(payload, nil)
	if err := enc.Close(); err != nil {
		t.Fatalf("zstd close: %v", err)
	}
	if err := os.WriteFile(base+".seg-0001.jsonl.zst", compressed, 0o600); err != nil {
		t.Fatalf("write sealed segment: %v", err)
	}
	index := map[string]any{
		"seg_id": "seg-0001", "turn_lo": 0, "turn_hi": 1,
		"ts_lo": sealed[0]["timestamp_utc"], "ts_hi": sealed[1]["timestamp_utc"],
		"bytes_raw": len(payload), "bytes_zst": len(compressed),
	}
	indexLine, err := json.Marshal(index)
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	if err := os.WriteFile(base+".index.jsonl", append(indexLine, '\n'), 0o600); err != nil {
		t.Fatalf("write index: %v", err)
	}
	active := []map[string]any{
		{
			"timestamp_utc": "2026-10-21T00:00:00Z",
			"query":         "active question one",
			"response_text": "active answer one",
			"mode_id":       "chat",
			"task_id":       "task-beta",
			"provenance":    map[string]any{"source": "model"},
		},
		{
			"timestamp_utc": "2026-10-21T00:02:00Z",
			"query":         "need the ALPHA_TOKEN details",
			"response_text": "here are the ALPHA_TOKEN details",
			"mode_id":       "chat",
			"task_id":       "task-beta",
			"provenance":    map[string]any{"source": "model"},
		},
		{
			"timestamp_utc": "2026-10-21T00:03:00Z",
			"query":         "status?",
			"response_text": "I've reached a resource budget for this turn. pausing.",
			"mode_id":       "chat",
			"task_id":       "task-beta",
			"provenance":    map[string]any{"source": "model"},
		},
	}
	var activeLines [][]byte
	for _, e := range active {
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal active entry: %v", err)
		}
		activeLines = append(activeLines, line)
	}
	if err := os.WriteFile(base+".current.jsonl", bytes.Join(activeLines, []byte("\n")), 0o600); err != nil {
		t.Fatalf("write active segment: %v", err)
	}
}

// connect spawns the server binary and returns a connected client session.
func connect(t *testing.T, bin, dataDir, threadDir, userSlug string) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, bin,
		"--data-dir", dataDir,
		"--thread-dir", threadDir,
		"--user-slug", userSlug,
		"--log-level", "ERROR",
	)
	transport := &mcp.CommandTransport{Command: cmd}
	client := mcp.NewClient(&mcp.Implementation{Name: "lisa-thread-test", Version: "0.1.0"}, nil)
	cs, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// callText invokes a tool and returns its text payload (the JSON string the
// server returns), failing the test on error.
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

// setup builds the binary, writes the fixture, and connects.
func setup(t *testing.T) *mcp.ClientSession {
	t.Helper()
	bin := buildBinary(t)
	root := t.TempDir()
	threadDir := filepath.Join(root, "threads")
	if err := os.MkdirAll(threadDir, 0o755); err != nil {
		t.Fatalf("mkdir thread dir: %v", err)
	}
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	writeFixture(t, filepath.Join(threadDir, "user-slug"))
	return connect(t, bin, dataDir, threadDir, "user-slug")
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
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("tool %s must be annotated read_only_hint", tool.Name)
		}
	}
	for _, want := range []string{
		"thread_read_range", "thread_read_recent", "thread_search",
		"thread_read_content", "thread_find_content", "thread_task_view",
	} {
		if !names[want] {
			t.Errorf("expected tool %s, got %v", want, names)
		}
	}
	if len(res.Tools) != 6 {
		t.Errorf("expected exactly 6 tools, got %d (%v)", len(res.Tools), names)
	}
}

func TestReadRecentReturnsEntries(t *testing.T) {
	cs := setup(t)
	out := callText(t, cs, "thread_read_recent", map[string]any{"limit": float64(10)})
	var page struct {
		Scope   string `json:"scope"`
		Entries []struct {
			Turn  int `json:"turn"`
			Query any `json:"query"`
		} `json:"entries"`
		HasMore bool `json:"has_more"`
	}
	if err := json.Unmarshal([]byte(out), &page); err != nil {
		t.Fatalf("unmarshal page: %v (%q)", err, out)
	}
	if page.Scope != "recorded_history" {
		t.Errorf("unexpected scope %q", page.Scope)
	}
	if len(page.Entries) != 5 {
		t.Fatalf("expected 5 entries (2 sealed + 3 active), got %d (%q)", len(page.Entries), out)
	}
	for i, e := range page.Entries {
		if e.Turn != i {
			t.Errorf("entry %d: expected turn %d, got %d", i, i, e.Turn)
		}
	}
	if got, ok := page.Entries[2].Query.(string); !ok || got != "active question one" {
		t.Errorf("entry 2 query: got %v", page.Entries[2].Query)
	}
	if page.HasMore {
		t.Errorf("unexpected has_more=true: %q", out)
	}
}

func TestReadRangeReturnsBoundedRange(t *testing.T) {
	cs := setup(t)
	out := callText(t, cs, "thread_read_range", map[string]any{
		"start_turn": float64(1), "end_turn": float64(3),
	})
	var page struct {
		Entries []struct {
			Turn  int `json:"turn"`
			Query any `json:"query"`
		} `json:"entries"`
		HasMore bool `json:"has_more"`
	}
	if err := json.Unmarshal([]byte(out), &page); err != nil {
		t.Fatalf("unmarshal page: %v (%q)", err, out)
	}
	if len(page.Entries) != 3 {
		t.Fatalf("expected 3 entries for turns 1-3, got %d (%q)", len(page.Entries), out)
	}
	for i, want := range []int{1, 2, 3} {
		if page.Entries[i].Turn != want {
			t.Errorf("entry %d: expected turn %d, got %d", i, want, page.Entries[i].Turn)
		}
	}
	if page.HasMore {
		t.Errorf("unexpected has_more=true: %q", out)
	}
}

// TestReadRecentAppliesRedaction is the security-invariant test: the raw
// store keeps the budget prose, but the projected thread_read_recent output
// must strikethrough the default cap-fire phrase.
func TestReadRecentAppliesRedaction(t *testing.T) {
	cs := setup(t)
	out := callText(t, cs, "thread_read_recent", map[string]any{"limit": float64(10)})
	if strings.Contains(out, "I've reached a resource budget") {
		t.Errorf("projected output leaked raw budget prose: %q", out)
	}
	if strings.Contains(out, "I've hit my iteration budget") {
		t.Errorf("projected output leaked raw iteration-budget prose: %q", out)
	}
	// The strikethrough marker must appear where each phrase was.
	if !strings.Contains(out, thread.RedactionMarker) {
		t.Errorf("expected redaction marker in output: %q", out)
	}
	// The raw store is lossless: read_content on the raw ref must still
	// carry the full text after redaction at the projection layer, so the
	// raw bytes on disk are untouched — verify via a second projection.
	out2 := callText(t, cs, "thread_read_content", map[string]any{
		"ref": "t1.response_text",
	})
	if !strings.Contains(out2, thread.RedactionMarker) {
		t.Errorf("expected redacted content page for t1.response_text: %q", out2)
	}
	if strings.Contains(out2, "I've hit my iteration budget") {
		t.Errorf("read_content leaked raw budget prose: %q", out2)
	}
}

func TestSearchFindsHits(t *testing.T) {
	cs := setup(t)
	out := callText(t, cs, "thread_search", map[string]any{"query": "ALPHA_TOKEN"})
	var page struct {
		Entries []struct {
			Turn    int     `json:"turn"`
			Score   float64 `json:"score"`
			Matches []struct {
				Ref     string `json:"ref"`
				Excerpt string `json:"excerpt"`
			} `json:"matches"`
		} `json:"entries"`
		HasMore bool `json:"has_more"`
	}
	if err := json.Unmarshal([]byte(out), &page); err != nil {
		t.Fatalf("unmarshal search page: %v (%q)", err, out)
	}
	if len(page.Entries) == 0 {
		t.Fatalf("expected search hits for ALPHA_TOKEN, got none (%q)", out)
	}
	// Entries without a literal hit but with query/response text survive at
	// score 0 with an empty matches list (mirrors _search._score_entry).
	turns := map[int]bool{}
	scored := map[int]bool{}
	for _, e := range page.Entries {
		turns[e.Turn] = true
		if e.Score > 0 {
			scored[e.Turn] = true
			if len(e.Matches) == 0 {
				t.Errorf("entry turn %d scored %v with no matches", e.Turn, e.Score)
			}
		} else if len(e.Matches) != 0 {
			t.Errorf("entry turn %d scored 0 with matches", e.Turn)
		}
	}
	// Turns 0 (sealed) and 3 (active) both mention ALPHA_TOKEN with score > 0.
	if !scored[0] || !scored[3] {
		t.Errorf("expected scored hits at turns 0 and 3, got %v (%q)", scored, out)
	}
	// All 5 turns appear (score-0 pass-through for text-bearing entries).
	if len(turns) != 5 {
		t.Errorf("expected 5 entries in search page, got %v (%q)", turns, out)
	}
}

func TestTaskViewSummarizesLatestTask(t *testing.T) {
	cs := setup(t)
	out := callText(t, cs, "thread_task_view", map[string]any{})
	var view struct {
		Scope     string `json:"scope"`
		TaskID    string `json:"task_id"`
		Status    string `json:"status"`
		Turns     int    `json:"turns"`
		Selection string `json:"selection"`
	}
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		t.Fatalf("unmarshal task view: %v (%q)", err, out)
	}
	if view.Scope != "recorded_history" || view.Selection != "latest_recorded" {
		t.Errorf("unexpected scope/selection: %q %q", view.Scope, view.Selection)
	}
	if view.TaskID != "task-beta" {
		t.Errorf("expected latest task task-beta, got %q", view.TaskID)
	}
	if view.Turns != 3 {
		t.Errorf("expected 3 turns for task-beta, got %d", view.Turns)
	}
}

func TestSearchAndFindContentPagedRead(t *testing.T) {
	cs := setup(t)
	// find_content inside a redacted value: the match offsets must land on
	// the redacted text, with the query itself still literal.
	out := callText(t, cs, "thread_find_content", map[string]any{
		"ref": "t3.response_text", "query": "ALPHA_TOKEN",
	})
	var page struct {
		Ref     string `json:"ref"`
		Matches []struct {
			OffsetChars      int    `json:"offset_chars"`
			MatchOffsetChars int    `json:"match_offset_chars"`
			Excerpt          string `json:"excerpt"`
		} `json:"matches"`
		TotalChars int  `json:"total_chars"`
		HasMore    bool `json:"has_more"`
	}
	if err := json.Unmarshal([]byte(out), &page); err != nil {
		t.Fatalf("unmarshal find page: %v (%q)", err, out)
	}
	if page.Ref != "t3.response_text" {
		t.Errorf("unexpected ref %q", page.Ref)
	}
	if len(page.Matches) != 1 {
		t.Fatalf("expected exactly one ALPHA_TOKEN match, got %d (%q)", len(page.Matches), out)
	}
	if !strings.Contains(page.Matches[0].Excerpt, "ALPHA_TOKEN") {
		t.Errorf("excerpt missing query text: %q", page.Matches[0].Excerpt)
	}

	// read_content pages the full (redacted) value.
	out2 := callText(t, cs, "thread_read_content", map[string]any{
		"ref": "t3.response_text", "limit_chars": float64(4000),
	})
	var content struct {
		Content    string `json:"content"`
		TotalChars int    `json:"total_chars"`
		HasMore    bool   `json:"has_more"`
	}
	if err := json.Unmarshal([]byte(out2), &content); err != nil {
		t.Fatalf("unmarshal content page: %v (%q)", err, out2)
	}
	if !strings.Contains(content.Content, "ALPHA_TOKEN details") {
		t.Errorf("content page missing body text: %q", content.Content)
	}
	if content.HasMore || content.TotalChars != len([]rune("here are the ALPHA_TOKEN details")) {
		t.Errorf("unexpected paging: %+v", content)
	}
}

// TestSearchRespectsToolFilters checks the search filter path on a
// transcript-bearing fixture written inline (covers t<m>.c<i> evidence).
func TestSearchRespectsToolFilters(t *testing.T) {
	bin := buildBinary(t)
	root := t.TempDir()
	threadDir := filepath.Join(root, "threads")
	dataDir := filepath.Join(root, "data")
	for _, d := range []string{threadDir, dataDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	base := filepath.Join(threadDir, "user-two")
	entry := map[string]any{
		"timestamp_utc": "2026-10-22T00:00:00Z",
		"query":         "run the report",
		"response_text": "done",
		"task_id":       "task-gamma",
		"provenance":    map[string]any{"source": "model"},
		"transcript": []any{
			map[string]any{
				"role": "assistant",
				"tool_calls": []any{
					map[string]any{
						"id": "call-1",
						"function": map[string]any{
							"name":      "storage.write_file",
							"arguments": `{"path": "out/report.txt", "content": "REPORT_BODY"}`,
						},
					},
				},
			},
			map[string]any{
				"role":         "tool",
				"tool_call_id": "call-1",
				"name":         "storage.write_file",
				"content":      "wrote out/report.txt",
			},
		},
	}
	line, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(base+".current.jsonl", line, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	cs := connect(t, bin, dataDir, threadDir, "user-two")

	out := callText(t, cs, "thread_search", map[string]any{
		"query": "REPORT_BODY", "tool_name": "storage.write_file",
	})
	var page struct {
		Entries []struct {
			Turn    int `json:"turn"`
			Matches []struct {
				Ref      string `json:"ref"`
				ToolName string `json:"tool_name"`
			} `json:"matches"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(out), &page); err != nil {
		t.Fatalf("unmarshal: %v (%q)", err, out)
	}
	if len(page.Entries) != 1 || page.Entries[0].Turn != 0 {
		t.Fatalf("expected one matching entry at turn 0, got %+v", page)
	}
	found := false
	for _, m := range page.Entries[0].Matches {
		if m.Ref == "t0.m0.c0.arguments" && m.ToolName == "storage.write_file" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a t0.m0.c0.arguments match: %+v", page.Entries[0].Matches)
	}

	// A non-matching tool filter yields the recorded_history envelope with
	// zero entries (not an error).
	out2 := callText(t, cs, "thread_search", map[string]any{
		"query": "REPORT_BODY", "tool_name": "fetch.fetch_url",
	})
	var empty struct {
		Entries []any `json:"entries"`
	}
	if err := json.Unmarshal([]byte(out2), &empty); err != nil {
		t.Fatalf("unmarshal: %v (%q)", err, out2)
	}
	if len(empty.Entries) != 0 {
		t.Errorf("expected no entries with wrong tool filter: %q", out2)
	}
}

// TestRedactionEnvContract pins the LISA_THREAD_REDACTION_* env contract
// end-to-end: patterns=none disables projection redaction.
func TestRedactionEnvContract(t *testing.T) {
	bin := buildBinary(t)
	root := t.TempDir()
	threadDir := filepath.Join(root, "threads")
	dataDir := filepath.Join(root, "data")
	for _, d := range []string{threadDir, dataDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	writeFixture(t, filepath.Join(threadDir, "user-three"))
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, bin,
		"--data-dir", dataDir,
		"--thread-dir", threadDir,
		"--user-slug", "user-three",
		"--log-level", "ERROR",
	)
	cmd.Env = append(os.Environ(), "LISA_THREAD_REDACTION_PATTERNS=none")
	transport := &mcp.CommandTransport{Command: cmd}
	client := mcp.NewClient(&mcp.Implementation{Name: "lisa-thread-test", Version: "0.1.0"}, nil)
	cs, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })

	out := callText(t, cs, "thread_read_recent", map[string]any{"limit": float64(10)})
	if !strings.Contains(out, "I've reached a resource budget for this turn. pausing.") {
		t.Errorf("expected raw prose with redaction disabled: %q", out)
	}
	if strings.Contains(out, thread.RedactionMarker) {
		t.Errorf("unexpected redaction marker with patterns=none: %q", out)
	}

	// A custom patterns line replaces the defaults (hard cutover).
	cmd2 := exec.CommandContext(ctx, bin,
		"--data-dir", dataDir,
		"--thread-dir", threadDir,
		"--user-slug", "user-three",
		"--log-level", "ERROR",
	)
	custom := fmt.Sprintf("LISA_THREAD_REDACTION_PATTERNS=%s", "ALPHA_TOKEN")
	cmd2.Env = append(os.Environ(), custom)
	transport2 := &mcp.CommandTransport{Command: cmd2}
	client2 := mcp.NewClient(&mcp.Implementation{Name: "lisa-thread-test", Version: "0.1.0"}, nil)
	cs2, err := client2.Connect(ctx, transport2, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { cs2.Close() })

	out2 := callText(t, cs2, "thread_read_recent", map[string]any{"limit": float64(10)})
	if strings.Contains(out2, "ALPHA_TOKEN") {
		t.Errorf("custom pattern did not redact ALPHA_TOKEN: %q", out2)
	}
	// Defaults are replaced, not merged: budget prose now passes through.
	if !strings.Contains(out2, "I've reached a resource budget") {
		t.Errorf("custom patterns should replace defaults: %q", out2)
	}
}

// TestResultRefReadContent exercises the r<sha256> stored-result window
// path with a fixture object under <base>.results/.
func TestResultRefReadContent(t *testing.T) {
	bin := buildBinary(t)
	root := t.TempDir()
	threadDir := filepath.Join(root, "threads")
	dataDir := filepath.Join(root, "data")
	for _, d := range []string{threadDir, dataDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	base := filepath.Join(threadDir, "user-four")
	if err := os.WriteFile(base+".current.jsonl", []byte{}, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	resultsDir := base + ".results"
	if err := os.MkdirAll(resultsDir, 0o755); err != nil {
		t.Fatalf("mkdir results: %v", err)
	}
	payload, err := json.Marshal(map[string]any{"content": "R-REF-BODY " + strings.Repeat("x", 9000)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	sum := sha256.Sum256(payload)
	ref := fmt.Sprintf("r%x", sum)
	if err := os.WriteFile(filepath.Join(resultsDir, ref+".json"), payload, 0o600); err != nil {
		t.Fatalf("write result object: %v", err)
	}
	cmd := exec.CommandContext(context.Background(), bin,
		"--data-dir", dataDir,
		"--thread-dir", threadDir,
		"--user-slug", "user-four",
		"--log-level", "ERROR",
	)
	// No redaction patterns at all -> the r-ref path streams the window.
	cmd.Env = append(os.Environ(), "LISA_THREAD_REDACTION_PATTERNS=none")
	transport := &mcp.CommandTransport{Command: cmd}
	client := mcp.NewClient(&mcp.Implementation{Name: "lisa-thread-test", Version: "0.1.0"}, nil)
	cs, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })

	out := callText(t, cs, "thread_read_content", map[string]any{
		"ref": ref, "limit_chars": float64(4000),
	})
	var page struct {
		Ref        string `json:"ref"`
		Content    string `json:"content"`
		TotalChars int    `json:"total_chars"`
		Source     struct {
			Scope string `json:"scope"`
		} `json:"source"`
	}
	if err := json.Unmarshal([]byte(out), &page); err != nil {
		t.Fatalf("unmarshal: %v (%q)", err, out)
	}
	if page.Ref != ref || page.Source.Scope != "stored_tool_result" {
		t.Errorf("unexpected page head: %q", out)
	}
	if !strings.HasPrefix(page.Content, "R-REF-BODY ") {
		t.Errorf("unexpected content prefix: %q", page.Content[:min(40, len(page.Content))])
	}
	if page.TotalChars != len("R-REF-BODY ")+9000 {
		t.Errorf("unexpected total_chars %d", page.TotalChars)
	}
}

// TestRequiredFlags checks the server refuses to start without the
// server-specific flags.
func TestRequiredFlags(t *testing.T) {
	bin := buildBinary(t)
	cmd := exec.Command(bin, "--data-dir", t.TempDir())
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected failure without --thread-dir/--user-slug, got success: %s", out)
	}
	if !strings.Contains(string(out), "--thread-dir is required") {
		t.Errorf("expected --thread-dir error, got: %s", out)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
