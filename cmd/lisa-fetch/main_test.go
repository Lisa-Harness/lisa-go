// End-to-end tests for the lisa-fetch Go MCP server. These spawn the real
// binary over stdio via CommandTransport and drive it with the SDK's own
// client — exactly how the Lisa runtime spawns it (RuntimeKind.BINARY).
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

// binaryPath is the compiled lisa-fetch binary, built once per test run.
func buildBinary(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "lisa-fetch-test.exe")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build lisa-fetch: %v", err)
	}
	return out
}

// connect spawns the server binary and returns a connected client session.
func connect(t *testing.T, bin, dataDir string) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, bin, "--data-dir", dataDir, "--log-level", "ERROR")
	transport := &mcp.CommandTransport{Command: cmd}
	client := mcp.NewClient(&mcp.Implementation{Name: "lisa-fetch-test", Version: "0.1.0"}, nil)
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
	if !names["fetch_url"] {
		t.Errorf("expected fetch_url tool, got %v", names)
	}
	if !names["invalidate_fetch_cache"] {
		t.Errorf("expected invalidate_fetch_cache tool, got %v", names)
	}
}

func TestFetchURLBlocksPrivateIP(t *testing.T) {
	bin := buildBinary(t)
	dataDir := t.TempDir()
	cs := connect(t, bin, dataDir)

	// The SSRF gate must reject loopback. We expect CallTool to surface an
	// error rather than content.
	_, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "fetch_url",
		Arguments: map[string]any{"url": "http://127.0.0.1/admin"},
	})
	if err == nil {
		t.Fatal("expected SSRF rejection for 127.0.0.1, got success")
	}
	if !strings.Contains(err.Error(), "private/loopback") && !strings.Contains(err.Error(), "Blocked") && !strings.Contains(err.Error(), "HTTPS") {
		t.Errorf("expected SSRF/HTTPS block error, got: %v", err)
	}
}

func TestFetchURLBlocksNonHTTPSByDefault(t *testing.T) {
	bin := buildBinary(t)
	dataDir := t.TempDir()
	cs := connect(t, bin, dataDir)

	_, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "fetch_url",
		Arguments: map[string]any{"url": "http://example.com"},
	})
	if err == nil {
		t.Fatal("expected HTTPS-required rejection for http:// URL, got success")
	}
}

// TestFetchAndCacheRoundTrip exercises a live HTTPS fetch and the L1/L2 cache.
// It requires network access; skipped in offline environments.
func TestFetchAndCacheRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live fetch in short mode")
	}
	bin := buildBinary(t)
	dataDir := t.TempDir()
	cs := connect(t, bin, dataDir)

	// First fetch: from_cache=false.
	first := callText(t, cs, "fetch_url", map[string]any{"url": "https://example.com"})
	var r1 struct {
		URL        string `json:"url"`
		StatusCode int    `json:"status_code"`
		FromCache  bool   `json:"from_cache"`
		Text       string `json:"text"`
	}
	if err := json.Unmarshal([]byte(first), &r1); err != nil {
		t.Fatalf("unmarshal first result: %v (%q)", err, first)
	}
	if r1.StatusCode != 200 {
		t.Fatalf("expected 200, got %d (%q)", r1.StatusCode, first)
	}
	if r1.FromCache {
		t.Error("first fetch should not be served from cache")
	}
	if !strings.Contains(r1.Text, "Example Domain") {
		t.Errorf("unexpected body for example.com: %q", first[:min(200, len(first))])
	}

	// Second fetch: should be served from cache.
	second := callText(t, cs, "fetch_url", map[string]any{"url": "https://example.com"})
	var r2 struct {
		FromCache bool `json:"from_cache"`
	}
	if err := json.Unmarshal([]byte(second), &r2); err != nil {
		t.Fatalf("unmarshal second result: %v", err)
	}
	if !r2.FromCache {
		t.Error("second fetch should be served from cache")
	}

	// Invalidate, then force a re-fetch: not from cache.
	if out := callText(t, cs, "invalidate_fetch_cache", map[string]any{"url": "https://example.com"}); !strings.Contains(out, `"ok": true`) {
		t.Fatalf("invalidate returned %q", out)
	}
	third := callText(t, cs, "fetch_url", map[string]any{"url": "https://example.com"})
	var r3 struct {
		FromCache bool `json:"from_cache"`
	}
	if err := json.Unmarshal([]byte(third), &r3); err != nil {
		t.Fatalf("unmarshal third result: %v", err)
	}
	if r3.FromCache {
		t.Error("fetch after invalidate should not be served from cache")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
