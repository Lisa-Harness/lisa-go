package conformance

import "testing"

func storagePair(t *testing.T, extra ...string) (*impl, *impl) {
	t.Helper()
	py := referencePython(t)
	bin := buildServer(t, "lisa-storage")
	ref := launch(t, "python", []string{py, "-m", "mcp_server.storage.server"}, extra...)
	port := launch(t, "go", []string{bin}, extra...)
	return ref, port
}

func TestStorageToolSurface(t *testing.T) {
	ref, port := storagePair(t)
	compareSurface(t, ref, port)
}

func TestStorageFileLifecycle(t *testing.T) {
	ref, port := storagePair(t)
	runDifferential(t, ref, port, []step{
		{"write", "write_file", map[string]any{"path": "notes/a.txt", "content": "hello\nhello again"}},
		{"read", "read_file", map[string]any{"path": "notes/a.txt"}},
		{"append", "append_file", map[string]any{"path": "notes/a.txt", "content": "tail\n"}},
		{"range", "read_file_range", map[string]any{"path": "notes/a.txt", "start": 2, "length": 5}},
		{"range past eof", "read_file_range", map[string]any{"path": "notes/a.txt", "start": 1000, "length": 5}},
		{"edit ambiguous", "edit_file", map[string]any{"path": "notes/a.txt", "old_string": "hello", "new_string": "bye"}},
		{"edit absent", "edit_file", map[string]any{"path": "notes/a.txt", "old_string": "nope", "new_string": "x"}},
		{"edit stale hash", "edit_file", map[string]any{"path": "notes/a.txt", "old_string": "tail", "new_string": "x", "expected_sha256": "deadbeef"}},
		{"edit unique", "edit_file", map[string]any{"path": "notes/a.txt", "old_string": "hello again", "new_string": "bye"}},
		{"edit replace all", "edit_file", map[string]any{"path": "notes/a.txt", "old_string": "e", "new_string": "E", "replace_all": true}},
		{"read edited", "read_file", map[string]any{"path": "notes/a.txt"}},
		{"stat", "stat_file", map[string]any{"path": "notes/a.txt"}},
		{"stat dir", "stat_file", map[string]any{"path": "notes"}},
		{"make dir", "make_dir", map[string]any{"path": "empty/nested"}},
		{"list all", "list_files", map[string]any{}},
		{"list prefix", "list_files", map[string]any{"prefix": "notes"}},
		{"delete dir", "delete_file", map[string]any{"path": "notes"}},
		{"delete", "delete_file", map[string]any{"path": "notes/a.txt"}},
		{"read deleted", "read_file", map[string]any{"path": "notes/a.txt"}},
		{"delete missing", "delete_file", map[string]any{"path": "notes/a.txt"}},
	})
}

func TestStorageUnicode(t *testing.T) {
	ref, port := storagePair(t)
	runDifferential(t, ref, port, []step{
		{"write", "write_file", map[string]any{"path": "u.txt", "content": "héllo 世界 🌍 <tag> & done"}},
		{"range multibyte", "read_file_range", map[string]any{"path": "u.txt", "start": 6, "length": 4}},
		{"read", "read_file", map[string]any{"path": "u.txt"}},
		{"edit multibyte", "edit_file", map[string]any{"path": "u.txt", "old_string": "世界", "new_string": "world"}},
		{"stat", "stat_file", map[string]any{"path": "u.txt"}},
	})
}

func TestStorageSandbox(t *testing.T) {
	ref, port := storagePair(t)
	runDifferential(t, ref, port, []step{
		{"traversal read", "read_file", map[string]any{"path": "../escape.txt"}},
		{"traversal write", "write_file", map[string]any{"path": "a/../../escape.txt", "content": "x"}},
		{"absolute read", "read_file", map[string]any{"path": "/etc/passwd"}},
		{"absolute write", "write_file", map[string]any{"path": "/tmp/escape.txt", "content": "x"}},
		{"traversal list", "list_files", map[string]any{"prefix": "../"}},
		{"empty path", "read_file", map[string]any{"path": ""}},
	})
}

func TestStorageReadonly(t *testing.T) {
	ref, port := storagePair(t, "--readonly")
	runDifferential(t, ref, port, []step{
		{"write", "write_file", map[string]any{"path": "a.txt", "content": "x"}},
		{"append", "append_file", map[string]any{"path": "a.txt", "content": "x"}},
		{"edit", "edit_file", map[string]any{"path": "a.txt", "old_string": "x", "new_string": "y"}},
		{"delete", "delete_file", map[string]any{"path": "a.txt"}},
		{"make dir", "make_dir", map[string]any{"path": "d"}},
		{"manifest write", "write_model_manifest", map[string]any{"content": "models: []\n"}},
		{"list", "list_files", map[string]any{}},
	})
}

func TestStorageDenyGlobs(t *testing.T) {
	ref, port := storagePair(t, "--deny-glob", "*.secret", "--deny-glob", "private/*")
	runDifferential(t, ref, port, []step{
		{"write allowed", "write_file", map[string]any{"path": "ok.txt", "content": "fine"}},
		{"write denied ext", "write_file", map[string]any{"path": "k.secret", "content": "x"}},
		{"write denied case", "write_file", map[string]any{"path": "K.SECRET", "content": "x"}},
		{"write denied dir", "write_file", map[string]any{"path": "private/x.txt", "content": "x"}},
		{"read denied", "read_file", map[string]any{"path": "k.secret"}},
		{"stat denied", "stat_file", map[string]any{"path": "private/x.txt"}},
		{"list denied prefix", "list_files", map[string]any{"prefix": "private"}},
		{"list all", "list_files", map[string]any{}},
	})
}

func TestStorageModelManifests(t *testing.T) {
	ref, port := storagePair(t)
	runDifferential(t, ref, port, []step{
		{"read missing proposal", "read_model_manifest", map[string]any{}},
		{"read invalid kind", "read_model_manifest", map[string]any{"which": "bogus"}},
		{"read invalid kind quoted", "read_model_manifest", map[string]any{"which": `it's "x"\n`}},
		{"write proposal", "write_model_manifest", map[string]any{"content": "models:\n  - id: a\n"}},
		{"read proposal", "read_model_manifest", map[string]any{"which": "proposal"}},
		{"write active", "write_model_manifest", map[string]any{"content": "models: []\n", "which": "active"}},
		{"read active", "read_model_manifest", map[string]any{"which": "active"}},
		{"write invalid kind", "write_model_manifest", map[string]any{"content": "x", "which": "bogus"}},
	})
}
