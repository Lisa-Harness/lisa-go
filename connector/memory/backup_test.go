package memory

import (
	"context"
	"database/sql"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// stubProvider hashes content into a deterministic flat vector (parity with
// the Python _StubEmbeddingProvider that hashes to a 4-d vector).
type stubProvider struct{ dim int }

func (p stubProvider) Embed(_ context.Context, texts []string) ([][]float64, error) {
	out := make([][]float64, 0, len(texts))
	for _, text := range texts {
		vec := make([]float64, p.dim)
		for i := range vec {
			vec[i] = float64((i + len(text)) % 7)
		}
		out = append(out, vec)
	}
	return out, nil
}

// brokenProvider always fails (parity with the Python _BrokenProvider).
type brokenProvider struct{}

func (brokenProvider) Embed(_ context.Context, _ []string) ([][]float64, error) {
	return nil, errBroken
}

type errBrokenType struct{}

func (errBrokenType) Error() string { return "embedding service down" }

var errBroken error = errBrokenType{}

func newSemanticStore(t *testing.T, dim int, p EmbeddingProvider) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "memory.db"), WithEmbeddingProvider(p, dim))
	if err != nil {
		t.Fatalf("new semantic store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestSemanticRecallFallsBackOnProviderError locks the best-effort sidecar
// invariant: an embedding failure never fails the write and never blocks
// lexical recall (parity: test_embedding_failure_does_not_block_write).
func TestSemanticRecallFallsBackOnProviderError(t *testing.T) {
	t.Parallel()
	s, err := New(filepath.Join(t.TempDir(), "memory.db"), WithEmbeddingProvider(brokenProvider{}, 4))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer s.Close()

	rec, err := s.RecordMemory(context.Background(), write("survives embedding failure", nil))
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if rec.Content != "survives embedding failure" {
		t.Errorf("content = %q", rec.Content)
	}
	// Lexical recall still works.
	result, err := s.Recall(context.Background(), "survives", "adam", RecallOptions{Limit: 1})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if !contains(result, "survives embedding failure") {
		t.Errorf("recall = %q", result)
	}
}

// TestSemanticRecallUsesEmbeddedPool exercises the KNN path with a working
// provider (sqlite-vec parity: when embedded rows exist, the semantic pool
// drives the candidate set and the zero-overlap lexical filter relaxes).
func TestSemanticRecallUsesEmbeddedPool(t *testing.T) {
	t.Parallel()
	s := newSemanticStore(t, 8, stubProvider{dim: 8})
	ctx := context.Background()
	mustRecord(t, s, write("we chose postgres for analytics", nil))
	mustRecord(t, s, write("the weather is sunny today", nil))

	result, err := s.Recall(ctx, "postgres analytics database", "adam", RecallOptions{Limit: 2})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if !contains(result, "we chose postgres for analytics") {
		t.Errorf("semantic recall missed the postgres note: %q", result)
	}
	// Both records surface through the semantic pool (no zero-overlap
	// filtering), but the postgres note must rank first only if ranking
	// agrees; with a stub hash both have equal distance, so assert
	// presence of both rather than strict ordering (matches the Python
	// test, which asserts presence only).
	if !contains(result, "the weather is sunny today") {
		t.Errorf("semantic pool dropped the sunny note: %q", result)
	}
}

// TestEmbeddingDimensionMismatchSkipsSidecar: a provider returning the wrong
// width must not corrupt the sidecar or fail the write.
func TestEmbeddingDimensionMismatchSkipsSidecar(t *testing.T) {
	t.Parallel()
	s, err := New(filepath.Join(t.TempDir(), "memory.db"), WithEmbeddingProvider(stubProvider{dim: 3}, 8))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer s.Close()
	if _, err := s.RecordMemory(context.Background(), write("dimension mismatch probe", nil)); err != nil {
		t.Fatalf("record: %v", err)
	}
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM memory_vec").Scan(&count); err != nil {
		t.Fatalf("count vec: %v", err)
	}
	if count != 0 {
		t.Errorf("memory_vec rows = %d, want 0 (mismatched vectors skipped)", count)
	}
}

// TestEmbeddingStorageIsFloat32Blob locks the sidecar storage format: a
// big-endian float32 BLOB per vector (sqlite-vec's storage precision), so a
// 1536-dim embedding is 6 KiB — not the ~19 KiB a JSON-text encoding would
// be — and values round-trip through float32 precision.
func TestEmbeddingStorageIsFloat32Blob(t *testing.T) {
	t.Parallel()
	s := newSemanticStore(t, 8, stubProvider{dim: 8})
	rec := mustRecord(t, s, write("blob format probe", nil))

	var blob []byte
	var dimension int
	if err := s.db.QueryRow(
		"SELECT embedding, dimension FROM memory_vec WHERE memory_id = ?", rec.ID).
		Scan(&blob, &dimension); err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	if dimension != 8 {
		t.Errorf("dimension = %d, want 8", dimension)
	}
	if len(blob) != 8*4 {
		t.Errorf("blob = %d bytes, want 32 (8 dims x float32)", len(blob))
	}
	vec, err := decodeVector(blob)
	if err != nil {
		t.Fatalf("decode blob: %v", err)
	}
	if len(vec) != 8 {
		t.Fatalf("decoded len = %d, want 8", len(vec))
	}
	// float32 round-trip: deterministic stub values within float32 precision.
	for i, x := range vec {
		want := float64((i + len("blob format probe")) % 7)
		if math.Abs(x-want) > 1e-6 {
			t.Errorf("vec[%d] = %v, want %v", i, x, want)
		}
	}
}

// TestEncodeDecodeVectorPrecision pins the float32 quantization behavior:
// values survive within float32's ~7 significant digits (relative error),
// which is exactly the precision sqlite-vec stores. Malformed blobs error.
func TestEncodeDecodeVectorPrecision(t *testing.T) {
	t.Parallel()
	in := []float64{0.25, -1.5, 3.14159265, 12345.678}
	out, err := decodeVector(encodeVector(in))
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	for i, x := range out {
		// float32 keeps ~7 significant digits: |diff| <= 1e-6 * max(|v|,1).
		tol := 1e-6 * math.Max(math.Abs(in[i]), 1)
		if math.Abs(x-in[i]) > 4*tol {
			t.Errorf("vec[%d] = %v, want ~%v (float32)", i, x, in[i])
		}
	}
	if _, err := decodeVector([]byte{1, 2, 3}); err == nil {
		t.Error("expected error for non-multiple-of-4 blob")
	}
}

// TestLexicalOnlyStoreWithoutProvider: no provider → no sidecar table, and
// recall works lexically (parity: the sqlite-vec-absent default).
func TestLexicalOnlyStoreWithoutProvider(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if s.vec {
		t.Error("sidecar present without provider")
	}
	mustRecord(t, s, write("plain lexical note", nil))
	result, err := s.Recall(context.Background(), "plain lexical", "adam", RecallOptions{Limit: 1})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if !contains(result, "plain lexical note") {
		t.Errorf("recall = %q", result)
	}
}

// TestSemanticPoolExcludesExpired: the KNN pool applies the same expiry
// filter as the lexical pool.
func TestSemanticPoolExcludesExpired(t *testing.T) {
	t.Parallel()
	s := newSemanticStore(t, 8, stubProvider{dim: 8})
	ctx := context.Background()
	rec := mustRecord(t, s, write("postgres decision expiring", nil))
	if _, err := s.db.Exec("UPDATE memories SET expires_at = '2000-01-01T00:00:00+00:00' WHERE id = ?", rec.ID); err != nil {
		t.Fatalf("expire: %v", err)
	}
	result, err := s.Recall(ctx, "postgres decision expiring", "adam", RecallOptions{Limit: 2})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if result != "" {
		t.Errorf("semantic pool surfaced expired row: %q", result)
	}
}

// TestSemanticPoolExcludesRetired: the KNN pool applies the same status
// filter as the lexical pool.
func TestSemanticPoolExcludesRetired(t *testing.T) {
	t.Parallel()
	s := newSemanticStore(t, 8, stubProvider{dim: 8})
	ctx := context.Background()
	rec := mustRecord(t, s, write("postgres retired probe", nil))
	if _, err := s.RetireMemory(ctx, rec.ID, "adam", ListOptions{}); err != nil {
		t.Fatalf("retire: %v", err)
	}
	result, err := s.Recall(ctx, "postgres retired probe", "adam", RecallOptions{Limit: 2})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if result != "" {
		t.Errorf("semantic pool surfaced retired row: %q", result)
	}
}

// TestRecallRelaxesOverlapWhenSemantic drives the isSemantic branch: a
// query with zero lexical overlap still surfaces semantically-near rows.
func TestRecallRelaxesOverlapWhenSemantic(t *testing.T) {
	t.Parallel()
	s := newSemanticStore(t, 8, stubProvider{dim: 8})
	mustRecord(t, s, write("stored semantic content", nil))
	// Query shares no token with the content; the semantic pool must still
	// return the row (lexical zero-overlap filter disabled).
	result, err := s.Recall(context.Background(), "zzz no overlap", "adam", RecallOptions{Limit: 2})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if !contains(result, "stored semantic content") {
		t.Errorf("semantic pool should surface content despite zero overlap: %q", result)
	}
}

// ── Backup tests (parity with sdk/memory/tests/test_backup.py) ────────────

var backupNameFormat = regexp.MustCompile(`^memory-\d{8}T\d{6}(-\d+)?\.sql$`)

func TestBackupSkipsWhenDBMissing(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	target, err := BackupMemoryDB(dataDir, BackupOptions{})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if target != "" {
		t.Errorf("backup target = %q, want empty when DB missing", target)
	}
}

func TestBackupWritesAndPrunes(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "data", "memory.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Create a store with two rows, then close it so the backup has data.
	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if _, err := s.RecordMemory(context.Background(), write("backup row one", nil)); err != nil {
		t.Fatalf("record: %v", err)
	}
	if _, err := s.RecordMemory(context.Background(), write("backup row two", nil)); err != nil {
		t.Fatalf("record: %v", err)
	}
	s.Close()

	target, err := BackupMemoryDB(dataDir, BackupOptions{Keep: 2})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if target == "" {
		t.Fatal("expected a backup target path")
	}
	if !backupNameFormat.MatchString(filepath.Base(target)) {
		t.Errorf("backup name %q does not match %v", filepath.Base(target), backupNameFormat)
	}

	// Two more backups; keep=2 must prune to exactly the newest two.
	time.Sleep(1100 * time.Millisecond) // timestamp resolution is 1s
	if _, err := BackupMemoryDB(dataDir, BackupOptions{Keep: 2}); err != nil {
		t.Fatalf("backup 2: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := BackupMemoryDB(dataDir, BackupOptions{Keep: 2}); err != nil {
		t.Fatalf("backup 3: %v", err)
	}
	backupDir := DefaultBackupDir(dataDir)
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatalf("read backup dir: %v", err)
	}
	if len(entries) != 2 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("backup dir has %d files, want 2 (keep=2): %v", len(entries), names)
	}
	for _, e := range entries {
		if !backupNameFormat.MatchString(e.Name()) {
			t.Errorf("stale backup %q not pruned", e.Name())
		}
	}
}

func TestBackupDumpIsRestorableSQL(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "data", "memory.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	rec, err := s.RecordMemory(context.Background(), write("restorable content", nil))
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	s.Close()

	target, err := BackupMemoryDB(dataDir, BackupOptions{})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read dump: %v", err)
	}
	sqlText := string(raw)
	for _, want := range []string{
		"BEGIN TRANSACTION;",
		"CREATE TABLE memories",
		`INSERT INTO "memories"`,
		"COMMIT;",
		"PRAGMA user_version = 2;",
	} {
		if !strings.Contains(sqlText, want) {
			t.Errorf("dump missing %q", want)
		}
	}
	// The dumped literal must carry the exact row content.
	if !strings.Contains(sqlText, "restorable content") {
		t.Errorf("dump missing row content; id was %d", rec.ID)
	}
	// The dump must actually restore: execute it into a fresh unmanaged DB
	// (parity: `sqlite3 memory.db < dump.sql`) and read the row back.
	restoredDB, err := sql.Open("sqlite", filepath.Join(dataDir, "restore-out.db"))
	if err != nil {
		t.Fatalf("open restore target: %v", err)
	}
	defer restoredDB.Close()
	if _, err := restoredDB.Exec(sqlText); err != nil {
		t.Fatalf("restore dump: %v", err)
	}
	var content string
	if err := restoredDB.QueryRow("SELECT content FROM memories WHERE id = ?", rec.ID).Scan(&content); err != nil {
		t.Fatalf("read restored row: %v", err)
	}
	if content != "restorable content" {
		t.Errorf("restored content = %q", content)
	}
}

func TestBackupQuotesSingleQuotesInContent(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "data", "memory.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if _, err := s.RecordMemory(context.Background(), write("it's quoted", nil)); err != nil {
		t.Fatalf("record: %v", err)
	}
	s.Close()
	target, err := BackupMemoryDB(dataDir, BackupOptions{})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read dump: %v", err)
	}
	if !strings.Contains(string(raw), `'it''s quoted'`) {
		t.Errorf("dump did not escape single quote: %q", raw)
	}
}

func TestDefaultKeepBackups(t *testing.T) {
	t.Parallel()
	if DefaultKeepBackups != 10 {
		t.Errorf("DefaultKeepBackups = %d, want 10", DefaultKeepBackups)
	}
}
