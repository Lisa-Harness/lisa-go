package storage

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestEditFile(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		before       string
		old          string
		replacement  string
		all          bool
		after        string
		replacements int
		firstLine    int
	}{
		{"one region", "# Tracker\n\n| A1 | Open |\n| A2 | Closed |\n", "| A1 | Open |", "| A1 | Closed |", false,
			"# Tracker\n\n| A1 | Closed |\n| A2 | Closed |\n", 1, 3},
		{"delete", "keep\nremove me\nkeep\n", "remove me\n", "", false, "keep\nkeep\n", 1, 2},
		{"no final newline", "one\ntwo", "two", "three", false, "one\nthree", 1, 2},
		{"CRLF", "one\r\ntwo\r\n", "two", "three", false, "one\r\nthree\r\n", 1, 2},
		{"replace all", "same\nmiddle\nsame\n", "same", "other", true, "other\nmiddle\nother\n", 2, 1},
		{"nonoverlapping", "aaaaa", "aa", "b", true, "bba", 2, 1},
		{"unicode", "préface\ncafé\nfin", "café", "茶", false, "préface\n茶\nfin", 1, 2},
		{"empty result", "one", "one", "", false, "", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := newBackend(t, nil)
			target := filepath.Join(b.FilesRoot(), "doc.md")
			if err := os.WriteFile(target, []byte(tc.before), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(target, 0o644); err != nil {
				t.Fatal(err)
			}
			var receipt struct {
				OK           bool   `json:"ok"`
				Path         string `json:"path"`
				Replacements int    `json:"replacements"`
				FirstLine    int    `json:"first_line"`
				BytesBefore  int    `json:"bytes_before"`
				BytesAfter   int    `json:"bytes_after"`
				SHA256       string `json:"sha256"`
			}
			out := b.EditFile("doc.md", tc.old, tc.replacement, tc.all, "", "utf-8")
			if err := json.Unmarshal([]byte(out), &receipt); err != nil {
				t.Fatalf("receipt: %v (%s)", err, out)
			}
			wantHash := fmt.Sprintf("%x", sha256.Sum256([]byte(tc.after)))
			if !receipt.OK || receipt.Path != "doc.md" || receipt.Replacements != tc.replacements ||
				receipt.FirstLine != tc.firstLine || receipt.BytesBefore != len(tc.before) ||
				receipt.BytesAfter != len(tc.after) || receipt.SHA256 != wantHash {
				t.Fatalf("unexpected receipt: %s", out)
			}
			got, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.after {
				t.Errorf("stored bytes = %q, want %q", got, tc.after)
			}
			info, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			if runtime.GOOS != "windows" && info.Mode().Perm() != 0o644 {
				t.Errorf("mode = %o, want 644", info.Mode().Perm())
			}
			entries, err := os.ReadDir(b.FilesRoot())
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != "doc.md" {
				t.Errorf("temporary files remain: %v", entries)
			}
		})
	}
}

func TestEditFileRejectsWithoutWriting(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, path, before, old, replacement, encoding, want string
	}{
		{"ambiguous", "doc.md", "same\nmiddle\nsame\n", "same", "other", "", "occurs 2 times"},
		{"absent", "doc.md", "one\n", "absent", "two", "", "not found"},
		{"CRLF mismatch", "doc.md", "one\r\ntwo\r\n", "one\ntwo\n", "changed\n", "", "uses CRLF"},
		{"LF mismatch", "doc.md", "one\ntwo\n", "one\r\ntwo\r\n", "changed\n", "", "uses LF"},
		{"empty fragment", "doc.md", "one", "", "two", "", "must not be empty"},
		{"no op", "doc.md", "one", "one", "one", "", "would change nothing"},
		{"invalid file", "doc.md", "\xff", "one", "two", "", "file is not valid text"},
		{"invalid replacement", "doc.md", "one", "one", "\xff", "", "new content is not valid text"},
		{"encoding", "doc.md", "one", "one", "two", "latin-1", "unsupported encoding"},
		{"missing file", "missing.md", "one", "one", "two", "", "file not found"},
		{"directory", ".", "one", "one", "two", "", "targets a directory"},
		{"traversal", "../doc.md", "one", "one", "two", "", "path rejected"},
		{"absolute", "/doc.md", "one", "one", "two", "", "path rejected"},
		{"denied", "secret.DB", "one", "one", "two", "", "denied by policy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := newBackend(t, []string{"*.db"})
			target := filepath.Join(b.FilesRoot(), "doc.md")
			if err := os.WriteFile(target, []byte(tc.before), 0o644); err != nil {
				t.Fatal(err)
			}
			out := b.EditFile(tc.path, tc.old, tc.replacement, false, "", tc.encoding)
			mustError(t, out, tc.want)
			var details struct {
				Lines  []int  `json:"lines"`
				Bytes  int    `json:"bytes"`
				SHA256 string `json:"sha256"`
			}
			if err := json.Unmarshal([]byte(out), &details); err != nil {
				t.Fatal(err)
			}
			if tc.name == "ambiguous" && !reflect.DeepEqual(details.Lines, []int{1, 3}) {
				t.Errorf("ambiguity lines = %v, want [1 3]", details.Lines)
			}
			if tc.name == "absent" && details.Bytes != len(tc.before) {
				t.Errorf("missing match bytes = %d, want %d", details.Bytes, len(tc.before))
			}
			if tc.name == "ambiguous" || tc.name == "absent" {
				if want := fmt.Sprintf("%x", sha256.Sum256([]byte(tc.before))); details.SHA256 != want {
					t.Errorf("error hash = %q, want %q", details.SHA256, want)
				}
			}
			got, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.before {
				t.Fatalf("rejected edit changed file to %q", got)
			}
			entries, err := os.ReadDir(b.FilesRoot())
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Errorf("rejected edit created files: %v", entries)
			}
		})
	}
}

func TestEditFileHashGuard(t *testing.T) {
	t.Parallel()
	b := newBackend(t, nil)
	if out := b.WriteFile("doc.md", "one\n"); !strings.Contains(out, `"ok":true`) {
		t.Fatal(out)
	}
	var stat struct {
		SHA256 string `json:"sha256"`
	}
	if err := json.Unmarshal([]byte(b.StatFile("doc.md")), &stat); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("%x", sha256.Sum256([]byte("one\n"))); stat.SHA256 != want {
		t.Fatalf("stat hash = %q, want %q", stat.SHA256, want)
	}
	stale := stat.SHA256
	for _, tc := range []struct{ old, replacement, encoding string }{
		{"one", "two", "UTF8"},
		{"two", "three", "utf_8"},
	} {
		out := b.EditFile("doc.md", tc.old, tc.replacement, false, " "+strings.ToUpper(stat.SHA256)+" ", tc.encoding)
		var receipt struct {
			OK     bool   `json:"ok"`
			SHA256 string `json:"sha256"`
		}
		if err := json.Unmarshal([]byte(out), &receipt); err != nil {
			t.Fatal(err)
		}
		if !receipt.OK || receipt.SHA256 == stat.SHA256 {
			t.Fatalf("guarded edit: %s", out)
		}
		if err := json.Unmarshal([]byte(b.StatFile("doc.md")), &stat); err != nil {
			t.Fatal(err)
		}
		if receipt.SHA256 != stat.SHA256 {
			t.Fatalf("edit hash %q differs from stat %q", receipt.SHA256, stat.SHA256)
		}
	}
	out := b.EditFile("doc.md", "three", "four", false, stale, "")
	mustError(t, out, "sha256 mismatch")
	var rejected struct {
		SHA256 string `json:"sha256"`
	}
	if err := json.Unmarshal([]byte(out), &rejected); err != nil {
		t.Fatal(err)
	}
	if rejected.SHA256 != stat.SHA256 || b.ReadFile("doc.md", "") != "three\n" {
		t.Fatalf("stale edit changed file or reported wrong hash: %s", out)
	}
	if err := json.Unmarshal([]byte(b.StatFile(".")), &stat); err != nil {
		t.Fatal(err)
	}
	if stat.SHA256 != "" {
		t.Errorf("directory hash = %q, want empty", stat.SHA256)
	}
}

func TestEditFileAmbiguityLinesCapped(t *testing.T) {
	t.Parallel()
	b := newBackend(t, nil)
	if out := b.WriteFile("doc.md", strings.Repeat("same\n", 12)); !strings.Contains(out, `"ok":true`) {
		t.Fatal(out)
	}
	out := b.EditFile("doc.md", "same", "other", false, "", "")
	mustError(t, out, "occurs 12 times")
	var details struct {
		Lines []int `json:"lines"`
	}
	if err := json.Unmarshal([]byte(out), &details); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(details.Lines, []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}) {
		t.Errorf("lines = %v, want first ten lines", details.Lines)
	}
}

func TestAtomicReplaceFailureCleansUp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(target, "original")
	if err := os.WriteFile(original, []byte("unchanged"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := atomicReplace(target, []byte("replacement")); err == nil {
		t.Fatal("expected replacement of a nonempty directory to fail")
	}
	got, err := os.ReadFile(original)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "unchanged" {
		t.Errorf("failed edit changed original: %q", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "target" {
		t.Errorf("failed edit left temporary files: %v", entries)
	}
}

func TestEditFileRejectsSymlinkEscape(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		// Creating symlinks requires privileges not available on all Windows hosts.
		return
	}
	b := newBackend(t, nil)
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("unchanged"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(b.FilesRoot(), "link.md")); err != nil {
		t.Fatal(err)
	}
	mustError(t, b.EditFile("link.md", "unchanged", "changed", false, "", ""), "path rejected")
	got, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "unchanged" {
		t.Fatalf("edit escaped sandbox: %q", got)
	}
}
