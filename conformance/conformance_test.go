package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// referencePython returns the canonical-environment interpreter or skips/fails.
func referencePython(t *testing.T) string {
	t.Helper()
	if py := os.Getenv("LISA_CONFORMANCE_PYTHON"); py != "" {
		return py
	}
	if os.Getenv("LISA_CONFORMANCE_REQUIRED") == "1" {
		t.Fatal("LISA_CONFORMANCE_REQUIRED=1 but LISA_CONFORMANCE_PYTHON is not set")
	}
	t.Skip("LISA_CONFORMANCE_PYTHON not set")
	return ""
}

// buildServer compiles cmd/<name> into a temp dir and returns the binary path.
func buildServer(t *testing.T, name string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), name)
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", out, "../cmd/"+name)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build %s: %v", name, err)
	}
	return out
}

// impl is one side of a differential comparison.
type impl struct {
	name    string
	dataDir string
	session *mcp.ClientSession
}

// launch spawns a server over stdio with the contract CLI and connects to it.
// Logs go to a temp file so stderr and the working directory stay clean.
func launch(t *testing.T, name string, argv []string, extra ...string) *impl {
	t.Helper()
	dataDir := t.TempDir()
	args := append([]string{}, argv[1:]...)
	args = append(args, "--data-dir", dataDir, "--log-level", "ERROR")
	args = append(args, extra...)
	cmd := exec.Command(argv[0], args...)
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "LISA_MCP_LOG_PATH="+filepath.Join(t.TempDir(), name+".jsonl"))
	client := mcp.NewClient(&mcp.Implementation{Name: "lisa-conformance", Version: "0.1.0"}, nil)
	cs, err := client.Connect(context.Background(), &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect %s: %v", name, err)
	}
	t.Cleanup(func() { cs.Close() })
	return &impl{name: name, dataDir: dataDir, session: cs}
}

// outcome is the normalized, comparable result of one tool call.
type outcome struct {
	IsError bool     `json:"is_error"`
	Content []string `json:"content"`
}

// call invokes a tool and normalizes the result. Protocol-level errors are
// folded into IsError so both error paths compare on equal footing.
func (s *impl) call(t *testing.T, tool string, args map[string]any) outcome {
	t.Helper()
	res, err := s.session.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if errors.Is(err, mcp.ErrConnectionClosed) || errors.Is(err, io.EOF) {
		t.Fatalf("%s server exited during %s: %v", s.name, tool, err)
	}
	if err != nil {
		return outcome{IsError: true, Content: []string{s.normalize(err.Error())}}
	}
	o := outcome{IsError: res.IsError}
	for _, c := range res.Content {
		tc, ok := c.(*mcp.TextContent)
		if !ok {
			t.Fatalf("%s %s: unexpected content type %T", s.name, tool, c)
		}
		text := tc.Text
		if res.IsError {
			// The Python MCP SDK frames ToolError text with this prefix; it is
			// SDK presentation, not server behavior.
			text = strings.TrimPrefix(text, "Error executing tool "+tool+": ")
		}
		o.Content = append(o.Content, s.normalize(text))
	}
	return o
}

var timestampPattern = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})?`)

// normalize canonicalizes JSON payloads (sorted keys, preserving null vs
// missing and [] vs null), masks the per-run data dir, and masks wall-clock
// timestamps, which legitimately differ between runs.
func (s *impl) normalize(text string) string {
	text = strings.ReplaceAll(text, s.dataDir, "<data_dir>")
	text = strings.ReplaceAll(text, filepath.ToSlash(s.dataDir), "<data_dir>")
	var v any
	if err := json.Unmarshal([]byte(text), &v); err == nil {
		if b, err := json.Marshal(maskVolatile(v)); err == nil {
			text = string(b)
		}
	}
	return timestampPattern.ReplaceAllString(text, "<timestamp>")
}

// maskVolatile replaces the values of known wall-clock fields.
func maskVolatile(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if k == "modified" && val != nil {
				x[k] = "<timestamp>"
				continue
			}
			x[k] = maskVolatile(val)
		}
	case []any:
		for i, val := range x {
			x[i] = maskVolatile(val)
		}
	}
	return v
}

// step is one tool call in a scenario.
type step struct {
	name string
	tool string
	args map[string]any
}

// runDifferential replays steps against both implementations in order and
// reports every divergence.
func runDifferential(t *testing.T, ref, port *impl, steps []step) {
	t.Helper()
	for _, st := range steps {
		want := mustJSON(t, ref.call(t, st.tool, st.args))
		got := mustJSON(t, port.call(t, st.tool, st.args))
		if want != got {
			t.Errorf("%s (%s): divergence\n%s: %s\n%s: %s", st.name, st.tool, ref.name, want, port.name, got)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// toolSurface is the comparable shape of a declared tool. Descriptions and
// SDK-specific schema presentation are deliberately excluded.
type toolSurface struct {
	Name        string   `json:"name"`
	ReadOnly    bool     `json:"read_only"`
	Destructive bool     `json:"destructive"`
	Properties  []string `json:"properties"`
	Required    []string `json:"required"`
}

func listSurface(t *testing.T, s *impl) []toolSurface {
	t.Helper()
	res, err := s.session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("%s list tools: %v", s.name, err)
	}
	out := make([]toolSurface, 0, len(res.Tools))
	for _, tool := range res.Tools {
		ts := toolSurface{Name: tool.Name}
		if a := tool.Annotations; a != nil {
			ts.ReadOnly = a.ReadOnlyHint
			ts.Destructive = a.DestructiveHint != nil && *a.DestructiveHint
		}
		var schema struct {
			Properties map[string]any `json:"properties"`
			Required   []string       `json:"required"`
		}
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("%s marshal schema %s: %v", s.name, tool.Name, err)
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("%s unmarshal schema %s: %v", s.name, tool.Name, err)
		}
		for p := range schema.Properties {
			ts.Properties = append(ts.Properties, p)
		}
		sort.Strings(ts.Properties)
		sort.Strings(schema.Required)
		ts.Required = schema.Required
		out = append(out, ts)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func compareSurface(t *testing.T, ref, port *impl) {
	t.Helper()
	want := mustJSON(t, listSurface(t, ref))
	got := mustJSON(t, listSurface(t, port))
	if want != got {
		t.Errorf("tool surface divergence\n%s: %s\n%s: %s", ref.name, want, port.name, got)
	}
}
