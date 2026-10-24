package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestGoMemoryCrossCheck is invoked by the session crosscheck driver with
// LISA_MEMORY_CROSSCHECK_FILE (the py_probe.py-captured outputs fixture) —
// the Python probe builds a fresh SQLite DB with the real
// sdk.memory.store.SqliteMemoryStore and this test replays the identical
// store operations in Go against the SAME .db file. Skips silently in
// normal test runs (env not set).
//
// Canonicalization: JSON objects marshal with sorted keys in both
// languages (Go encoding/json sorts map keys; Python json.dumps sort_keys).
// Timestamp-bearing values are normalized because the two runs execute at
// different wall times; everything else must match byte-for-byte after
// canonicalization.
func TestGoMemoryCrossCheck(t *testing.T) {
	file := os.Getenv("LISA_MEMORY_CROSSCHECK_FILE")
	if file == "" {
		t.Skip("crosscheck env not set")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read crosscheck file: %v", err)
	}
	var pyOutputs map[string]string
	if err := json.Unmarshal(raw, &pyOutputs); err != nil {
		t.Fatalf("unmarshal crosscheck file: %v", err)
	}
	fixtureRaw, ok := pyOutputs["fixture"]
	if !ok {
		t.Fatalf("crosscheck fixture case missing")
	}
	var fixture struct {
		IDs             []int64 `json:"ids"`
		PreRecallDBPath string  `json:"pre_recall_db_path"`
	}
	if err := json.Unmarshal([]byte(fixtureRaw), &fixture); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	if _, err := os.Stat(fixture.PreRecallDBPath); err != nil {
		t.Fatalf("fixture db missing: %v", err)
	}

	// Open the Python-built pre-recall snapshot directly in Go. Schema
	// parity is the point: the Go store must accept it as-is
	// (user_version=2, no recreate), and the replay then re-runs the same
	// incremental sequence the Python probe captured (state-observing
	// cases before the recall that bumps access counters). The file lives
	// in the probe's temp dir; the replay works on a copy so reruns stay
	// deterministic.
	dbCopy := filepath.Join(t.TempDir(), "memory.db")
	if err := copyFile(fixture.PreRecallDBPath, dbCopy); err != nil {
		t.Fatalf("copy fixture db: %v", err)
	}
	store, err := New(dbCopy)
	if err != nil {
		t.Fatalf("open python-built db in go: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	r2, r6 := fixture.IDs[1], fixture.IDs[5]

	// Replay order matters: list/get cases observe access_count=0 (no
	// recall ran yet, matching the Python capture order), then the recall
	// cases run, then access_tracking observes the bump. Ordered slice,
	// not a map, to keep the replay deterministic.
	cases := []struct {
		name string
		fn   func() (string, error)
	}{
		{"get_record", func() (string, error) {
			rec, err := store.GetMemory(ctx, r2, "adam", ListOptions{})
			if err != nil {
				return "", err
			}
			if rec == nil {
				return "", fmt.Errorf("record %d not found", r2)
			}
			return marshalJSON(rec), nil
		}},
		{"list_default", func() (string, error) {
			recs, err := store.ListMemories(ctx, "adam", ListOptions{})
			return marshalJSON(recs), err
		}},
		{"list_channel", func() (string, error) {
			recs, err := store.ListMemories(ctx, "adam", ListOptions{SurfaceID: "chan"})
			return marshalJSON(recs), err
		}},
		{"list_status_none", func() (string, error) {
			recs, err := store.ListMemories(ctx, "adam", ListOptions{Status: StatusNoFilter})
			return marshalJSON(recs), err
		}},
		{"list_kind_semantic", func() (string, error) {
			recs, err := store.ListMemories(ctx, "adam", ListOptions{MemoryKind: "semantic"})
			return marshalJSON(recs), err
		}},
		{"list_nadia", func() (string, error) {
			recs, err := store.ListMemories(ctx, "nadia", ListOptions{})
			return marshalJSON(recs), err
		}},
		{"count_active", func() (string, error) {
			n, err := store.CountMemories(ctx, "adam", ListOptions{})
			return fmt.Sprintf("%d", n), err
		}},
		{"count_status_none", func() (string, error) {
			n, err := store.CountMemories(ctx, "adam", ListOptions{Status: StatusNoFilter})
			return fmt.Sprintf("%d", n), err
		}},
		{"count_channel", func() (string, error) {
			n, err := store.CountMemories(ctx, "adam", ListOptions{SurfaceID: "chan"})
			return fmt.Sprintf("%d", n), err
		}},
		{"recall_postgres", func() (string, error) {
			return store.Recall(ctx, "chose postgresql control", "adam", RecallOptions{Limit: 5})
		}},
		{"recall_records_json", func() (string, error) {
			recs, err := store.RecallRecords(ctx, "postgres", "adam", RecallOptions{Limit: 5})
			return marshalJSON(recs), err
		}},
		{"recall_no_match", func() (string, error) {
			return store.Recall(ctx, "zzz no match", "adam", RecallOptions{Limit: 2})
		}},
		{"recall_channel_isolated", func() (string, error) {
			return store.Recall(ctx, "postgres", "adam", RecallOptions{Limit: 5, SurfaceID: "chan"})
		}},
		{"recall_nadia", func() (string, error) {
			return store.Recall(ctx, "postgres", "nadia", RecallOptions{Limit: 5})
		}},
		{"recall_excludes_retired_by_scope", func() (string, error) {
			return store.Recall(ctx, "retired postgres", "adam", RecallOptions{Limit: 5})
		}},
		{"access_tracking", func() (string, error) {
			rec, err := store.GetMemory(ctx, r2, "adam", ListOptions{})
			if err != nil || rec == nil {
				return "", err
			}
			return marshalJSON(map[string]any{
				"access_count":     rec.AccessCount,
				"last_accessed_at": rec.LastAccessedAt,
			}), nil
		}},
		{"retired_record", func() (string, error) {
			rec, err := store.GetMemory(ctx, r6, "adam", ListOptions{SurfaceID: "chan"})
			if err != nil {
				return "", err
			}
			if rec == nil {
				return "", fmt.Errorf("retired record %d not found", r6)
			}
			return marshalJSON(rec), nil
		}},
	}

	for _, tc := range cases {
		pyOut, ok := pyOutputs[tc.name]
		if !ok {
			t.Errorf("crosscheck: python output %q missing", tc.name)
			continue
		}
		goOut, err := tc.fn()
		if err != nil {
			t.Errorf("crosscheck %s: go error %v", tc.name, err)
			continue
		}
		pyC := canon(pyOut)
		goC := canon(goOut)
		if pyC != goC {
			t.Errorf("crosscheck %s: STRUCTURE MISMATCH\npython: %s\ngo:     %s", tc.name, pyC, goC)
		}
	}
}

// canon canonicalizes a JSON payload: decode → re-marshal (sorted keys), with
// wall-clock timestamps normalized (the two runs execute at different times;
// null/non-null state is preserved, values replace with a sentinel). Both
// UTC suffix spellings normalize: Python isoformat "+00:00" and Go "Z".
var timestampValuePattern = regexp.MustCompile(
	`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|\+00:00)`)

func canon(s string) string {
	var v any
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &v); err != nil {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return s
	}
	return timestampValuePattern.ReplaceAllString(string(b), "<timestamp>")
}

func marshalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
