// Unit tests for the sandboxed storage backend. These exercise the two
// non-bypassable safety invariants (sandbox escape rejection, deny-glob
// policy) plus the write-time text normalization / binary byte-fidelity
// contract — directly against Backend on a temp data dir, no MCP server.
package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newBackend builds a read-write backend on a fresh temp data dir.
func newBackend(t *testing.T, denyGlobs []string) *Backend {
	t.Helper()
	return New(t.TempDir(), false, denyGlobs)
}

// mustError asserts the result is a {"error": ...} payload containing want.
func mustError(t *testing.T, got, want string) {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal([]byte(got), &obj); err != nil {
		t.Fatalf("expected JSON error payload, got %q", got)
	}
	msg, ok := obj["error"].(string)
	if !ok {
		t.Fatalf("expected {\"error\": ...} payload, got %q", got)
	}
	if want != "" && !strings.Contains(msg, want) {
		t.Fatalf("error %q does not contain %q", msg, want)
	}
}

// TestResolveRejectsSandboxEscape covers the first invariant: absolute
// paths and ".." segments resolve to rejection, never to a path outside
// the files root.
func TestResolveRejectsSandboxEscape(t *testing.T) {
	b := newBackend(t, nil)
	root := b.FilesRoot()

	bad := []string{
		"../x",
		"../../secrets.txt",
		"ok/../../x",
		"..",
		"/etc/passwd",
		"/x",
		`C:\Windows\system.ini`,
		`c:/temp/evil`,
		"",
		"   ",
	}
	for _, p := range bad {
		resolved, err := b.ResolveFilePath(p)
		if err != nil {
			t.Errorf("ResolveFilePath(%q): unexpected policy error %v (escape rejections must be silent)", p, err)
		}
		if resolved != "" {
			t.Errorf("ResolveFilePath(%q) = %q, want rejection", p, resolved)
		}
	}

	// Tool-level rejections surface as {"error": ...} payloads.
	mustError(t, b.ReadFile("../x", ""), "rejected")
	mustError(t, b.WriteFile("../x", "hi"), "rejected")
	mustError(t, b.DeleteFile("/etc/passwd"), "rejected")

	// A legitimate path resolves under the files root.
	resolved, err := b.ResolveFilePath("sub/ok.txt")
	if err != nil || resolved == "" {
		t.Fatalf("ResolveFilePath(ok) = %q, %v", resolved, err)
	}
	if !strings.HasPrefix(resolved, root) {
		t.Errorf("resolved %q not under files root %q", resolved, root)
	}
}

// TestDenyGlobCaseInsensitive covers the second invariant: deny globs
// match the forward-slash relative path case-insensitively.
func TestDenyGlobCaseInsensitive(t *testing.T) {
	b := newBackend(t, []string{"*.db", "secrets/*"})

	for _, p := range []string{"x.db", "X.DB", "deep/Dir/DATA.Db", "secrets/key.txt", "SECRETS/KEY.txt"} {
		if !b.DeniedByPolicy(p) {
			t.Errorf("DeniedByPolicy(%q) = false, want true", p)
		}
		resolved, err := b.ResolveFilePath(p)
		if _, ok := err.(*PathRejected); !ok {
			t.Errorf("ResolveFilePath(%q): err = %v, want *PathRejected", p, err)
		}
		if resolved != "" {
			t.Errorf("ResolveFilePath(%q) = %q, want rejection", p, resolved)
		}
	}

	for _, p := range []string{"x.txt", "data/json.db.txt", "other/key.txt"} {
		if b.DeniedByPolicy(p) {
			t.Errorf("DeniedByPolicy(%q) = true, want false", p)
		}
	}

	// Tool surface: writes, reads, and listing a denied prefix return
	// error payloads.
	mustError(t, b.WriteFile("X.Db", "nope"), "denied")
	mustError(t, b.ReadFile("x.db", ""), "denied")
	mustError(t, b.ListFiles("x.db"), "prefix denied")

	// Denied entries are filtered out of listings.
	if out := b.WriteFile("ok.txt", "hi"); !strings.Contains(out, `"ok":true`) {
		t.Fatalf("write ok.txt: %s", out)
	}
	var listed []string
	if err := json.Unmarshal([]byte(b.ListFiles("")), &listed); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	for _, f := range listed {
		if strings.HasSuffix(strings.ToLower(f), ".db") {
			t.Errorf("denied .db file leaked into listing: %v", listed)
		}
	}
	found := false
	for _, f := range listed {
		if f == "ok.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("ok.txt missing from listing: %v", listed)
	}
}

// TestWriteNormalizesTextTrailingNewline asserts text content is stored
// newline-terminated and the receipt reports the added newline.
func TestWriteNormalizesTextTrailingNewline(t *testing.T) {
	b := newBackend(t, nil)

	out := b.WriteFile("note.txt", "no trailing newline")
	var receipt map[string]any
	if err := json.Unmarshal([]byte(out), &receipt); err != nil {
		t.Fatalf("write receipt: %v (%q)", err, out)
	}
	if receipt["trailing_newline_added"] != true {
		t.Errorf("trailing_newline_added = %v, want true", receipt["trailing_newline_added"])
	}
	data, err := os.ReadFile(filepath.Join(b.FilesRoot(), "note.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "no trailing newline\n" {
		t.Errorf("stored bytes %q, want newline-terminated", string(data))
	}

	// Already-terminated content is stored as-is.
	out = b.WriteFile("ok.md", "done\n")
	if !strings.Contains(out, `"trailing_newline_added":false`) {
		t.Errorf("expected trailing_newline_added=false, got %q", out)
	}

	// Empty content stays empty.
	out = b.WriteFile("empty.txt", "")
	if !strings.Contains(out, `"bytes":0`) {
		t.Errorf("expected 0 bytes for empty write, got %q", out)
	}
}

// TestBinarySuffixByteFaithful asserts a binary-suffix file is stored
// byte-for-byte: no trailing newline is ever appended, which would corrupt
// the format.
func TestBinarySuffixByteFaithful(t *testing.T) {
	b := newBackend(t, nil)

	raw := "PNG-BYTES-NO-NEWLINE"
	out := b.WriteFile("img.png", raw)
	var receipt map[string]any
	if err := json.Unmarshal([]byte(out), &receipt); err != nil {
		t.Fatalf("write receipt: %v (%q)", err, out)
	}
	if receipt["trailing_newline_added"] != false {
		t.Errorf("binary write must not add a newline, receipt = %v", receipt)
	}
	if int(receipt["bytes"].(float64)) != len(raw) {
		t.Errorf("bytes = %v, want %d", receipt["bytes"], len(raw))
	}
	data, err := os.ReadFile(filepath.Join(b.FilesRoot(), "img.png"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != raw {
		t.Errorf("stored bytes %q differ from sent %q", string(data), raw)
	}

	// Uppercase suffix matches too (case-insensitive extension check).
	out = b.WriteFile("DB.SQLITE", "page-data")
	if strings.Contains(out, `"trailing_newline_added":true`) {
		t.Errorf("uppercase binary suffix must not be newline-normalized: %q", out)
	}
}

// TestReadonlyRejectsWrites asserts all write-side operations return the
// read-only error payload; reads still work.
func TestReadonlyRejectsWrites(t *testing.T) {
	dir := t.TempDir()
	// Pre-create the files sandbox and a readable file (readonly mode does
	// not create the files/ dir on New).
	filesDir := filepath.Join(dir, dataDirName, filesSubdir)
	if err := os.MkdirAll(filesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filesDir, "r.txt"), []byte("readable\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	b := New(dir, true, nil)
	for _, out := range []string{
		b.WriteFile("w.txt", "nope"),
		b.AppendFile("r.txt", "nope"),
		b.EditFile("r.txt", "readable", "changed", false, "", ""),
		b.DeleteFile("r.txt"),
		b.MakeDir("sub"),
		b.WriteModelManifest("models: []", "proposal"),
	} {
		mustError(t, out, "read-only")
	}
	if got := b.ReadFile("r.txt", ""); got != "readable\n" {
		t.Errorf("read in readonly mode = %q", got)
	}
}

// TestRoundTrip exercises write→append→read→stat→delete through the
// backend API on one file.
func TestRoundTrip(t *testing.T) {
	b := newBackend(t, nil)

	if out := b.WriteFile("dir/f.txt", "one"); !strings.Contains(out, `"ok":true`) {
		t.Fatalf("write: %s", out)
	}
	out := b.AppendFile("dir/f.txt", "two\n")
	var app map[string]any
	if err := json.Unmarshal([]byte(out), &app); err != nil {
		t.Fatalf("append: %v (%q)", err, out)
	}
	if app["appended_bytes"].(float64) != 4 {
		t.Errorf("appended_bytes = %v, want 4", app["appended_bytes"])
	}
	if got := b.ReadFile("dir/f.txt", ""); got != "one\ntwo\n" {
		t.Errorf("read = %q, want %q", got, "one\ntwo\n")
	}

	var stat map[string]any
	if err := json.Unmarshal([]byte(b.StatFile("dir/f.txt")), &stat); err != nil {
		t.Fatalf("stat: %v", err)
	}
	if stat["is_dir"].(bool) || int(stat["size"].(float64)) != 8 {
		t.Errorf("unexpected stat: %v", stat)
	}

	// read_file_range: char-based slice.
	var rr map[string]any
	if err := json.Unmarshal([]byte(b.ReadFileRange("dir/f.txt", 4, 3, "")), &rr); err != nil {
		t.Fatalf("range: %v", err)
	}
	if rr["content"] != "two" || rr["eof"] != false || rr["total"].(float64) != 8 {
		t.Errorf("unexpected range result: %v", rr)
	}

	// Delete files only: directories are refused.
	mustError(t, b.DeleteFile("dir"), "directory")
	if out := b.DeleteFile("dir/f.txt"); !strings.Contains(out, `"ok":true`) {
		t.Fatalf("delete: %s", out)
	}
	mustError(t, b.ReadFile("dir/f.txt", ""), "not found")
}

// TestModelManifestRoundTrip covers the manifest pair outside the files
// sandbox: proposal default, active selection, invalid-kind rejection.
func TestModelManifestRoundTrip(t *testing.T) {
	b := newBackend(t, nil)

	mustError(t, b.ReadModelManifest("bogus"), "manifest kind")
	mustError(t, b.ReadModelManifest("proposal"), "not found")

	if out := b.WriteModelManifest("models:\n  - a\n", ""); !strings.Contains(out, `"which":"proposal"`) {
		t.Fatalf("write default manifest: %s", out)
	}
	if got := b.ReadModelManifest("proposal"); !strings.HasPrefix(got, "models:") {
		t.Errorf("read proposal = %q", got)
	}
	if _, err := os.Stat(filepath.Join(b.DataDir(), stateDirName, "models.proposed.yaml")); err != nil {
		t.Errorf("proposal manifest not written under state/: %v", err)
	}

	// Kind matching is case-insensitive for the destination path; the
	// receipt echoes the caller's spelling.
	if out := b.WriteModelManifest("models:\n  - b\n", "ACTIVE"); !strings.Contains(out, `"ok":true`) {
		t.Fatalf("write active manifest: %s", out)
	}
	if _, err := os.Stat(filepath.Join(b.DataDir(), configDirName, "models.yaml")); err != nil {
		t.Errorf("active manifest not written under config/: %v", err)
	}
}
