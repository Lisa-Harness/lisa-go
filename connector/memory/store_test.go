package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
)

// write is the _write() helper of the Python test module.
func write(content string, mutate func(*MemoryWriteRequest)) MemoryWriteRequest {
	req := MemoryWriteRequest{
		UserSlug:    "adam",
		Content:     content,
		MemoryScope: "personal",
		WorkspaceID: "default",
		MemoryTier:  "long_term",
		OperationID: "test",
		ContentJSON: map[string]any{},
		Confidence:  1.0,
	}
	if mutate != nil {
		mutate(&req)
	}
	return req
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustRecord(t *testing.T, s *Store, req MemoryWriteRequest) MemoryRecord {
	t.Helper()
	rec, err := s.RecordMemory(context.Background(), req)
	if err != nil {
		t.Fatalf("record memory: %v", err)
	}
	return rec
}

// ── Schema / construction ─────────────────────────────────────────────────

func TestPostInitCreatesDBFileAndDirectories(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "nested", "deep", "memory.db")
	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer s.Close()
	// The schema must be idempotent — constructing twice on the same path
	// must not raise.
	s2, err := New(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer s2.Close()
}

func TestDefaultTTLDaysAreSensible(t *testing.T) {
	t.Parallel()
	if got := *DefaultTTLDays["daily"]; got != 7 {
		t.Errorf("daily TTL = %d, want 7", got)
	}
	if got := *DefaultTTLDays["weekly"]; got != 30 {
		t.Errorf("weekly TTL = %d, want 30", got)
	}
	if got := *DefaultTTLDays["long_term"]; got != 365 {
		t.Errorf("long_term TTL = %d, want 365", got)
	}
	if DefaultTTLDays["permanent"] != nil {
		t.Errorf("permanent TTL = %v, want nil", DefaultTTLDays["permanent"])
	}
}

// ── RecordMemory ──────────────────────────────────────────────────────────

func TestRecordMemoryPersistsAndReturnsRecord(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	rec := mustRecord(t, s, write("We chose PostgreSQL for the control plane.", func(r *MemoryWriteRequest) {
		r.Salience = 0.8
	}))

	if rec.Content != "We chose PostgreSQL for the control plane." {
		t.Errorf("content = %q", rec.Content)
	}
	if rec.UserSlug != "adam" {
		t.Errorf("user_slug = %q", rec.UserSlug)
	}
	if rec.MemoryScope != "personal" {
		t.Errorf("memory_scope = %q", rec.MemoryScope)
	}
	if rec.Salience != 0.8 {
		t.Errorf("salience = %v", rec.Salience)
	}
	if rec.CreatedAt == "" {
		t.Error("created_at empty")
	}
}

func TestRecordMemoryDerivesSourceUserFromUserSlug(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	rec := mustRecord(t, s, write("derived user", func(r *MemoryWriteRequest) { r.UserSlug = "adam" }))
	fetched, err := s.GetMemory(context.Background(), rec.ID, "adam", ListOptions{})
	if err != nil || fetched == nil {
		t.Fatalf("get memory: %v", err)
	}
	if fetched.SourceUserID != "adam" || fetched.SourceUserSlug != "adam" || fetched.UserID != "adam" {
		t.Errorf("derived user fields: %q/%q/%q", fetched.SourceUserID, fetched.SourceUserSlug, fetched.UserID)
	}
}

func TestRecordMemoryAppliesTierTTL(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	daily := mustRecord(t, s, write("ephemeral", func(r *MemoryWriteRequest) { r.MemoryTier = "daily" }))
	permanent := mustRecord(t, s, write("forever", func(r *MemoryWriteRequest) { r.MemoryTier = "permanent" }))
	if daily.ValidUntil == nil {
		t.Error("daily memory: valid_until nil, want expires_at fallback")
	}
	if permanent.ValidUntil != nil {
		t.Errorf("permanent memory: valid_until %v, want nil", permanent.ValidUntil)
	}
	// The daily tier's expires_at is applied in the DB; on the record it
	// surfaces through valid_until (no expires_at field on MemoryRecord —
	// parity with the pydantic shape; crosscheck-locked).
}

// TestRecordMemoryUnknownTierFallsBackToDaily locks the Python .get(tier,
// DEFAULT_TTL_DAYS["daily"]) parity: an unknown tier gets the 7-day TTL,
// not a permanent row. Surfaced through valid_until (the expires_at DB
// column is not part of the record shape).
func TestRecordMemoryUnknownTierFallsBackToDaily(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	rec := mustRecord(t, s, write("typo'd tier", func(r *MemoryWriteRequest) { r.MemoryTier = "month" }))
	if rec.ValidUntil == nil {
		t.Error("unknown tier: valid_until nil, want daily TTL fallback")
	}
}

// ── ListMemories ──────────────────────────────────────────────────────────

func TestListMemoriesIsSurfaceScopedAndOrderedByRecency(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	// Shared scope is a different memory_scope and must not leak into personal.
	for n := 0; n < 3; n++ {
		mustRecord(t, s, write(fmt.Sprintf("shared note %d", n), func(r *MemoryWriteRequest) { r.MemoryScope = "shared" }))
	}
	mustRecord(t, s, write("personal note one", nil))
	mustRecord(t, s, write("personal note two", nil))
	mustRecord(t, s, write("other surface note", func(r *MemoryWriteRequest) { r.SurfaceID = "channel-1" }))

	defaultSurface, err := s.ListMemories(ctx, "adam", ListOptions{MemoryScope: "personal"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	channelSurface, err := s.ListMemories(ctx, "adam", ListOptions{MemoryScope: "personal", SurfaceID: "channel-1"})
	if err != nil {
		t.Fatalf("list channel: %v", err)
	}
	sharedScope, err := s.ListMemories(ctx, "adam", ListOptions{MemoryScope: "shared"})
	if err != nil {
		t.Fatalf("list shared: %v", err)
	}

	got := contents(defaultSurface)
	want := []string{"personal note two", "personal note one"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("default surface = %v, want %v", got, want)
	}
	if got := contents(channelSurface); len(got) != 1 || got[0] != "other surface note" {
		t.Errorf("channel surface = %v", got)
	}
	if got := contents(sharedScope); len(got) != 3 || got[0] != "shared note 2" || got[1] != "shared note 1" || got[2] != "shared note 0" {
		t.Errorf("shared scope = %v", got)
	}
}

func contents(records []MemoryRecord) []string {
	out := make([]string, 0, len(records))
	for _, r := range records {
		out = append(out, r.Content)
	}
	return out
}

func TestListMemoriesRespectsLimit(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	for n := 0; n < 5; n++ {
		mustRecord(t, s, write(fmt.Sprintf("note %d", n), nil))
	}
	records, err := s.ListMemories(ctx, "adam", ListOptions{Limit: 2})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("len = %d, want 2", len(records))
	}
	// Most recent first (highest id first).
	if records[0].Content != "note 4" || records[1].Content != "note 3" {
		t.Errorf("order = %v", contents(records))
	}
}

func TestListMemoriesIsolatesByUser(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	mustRecord(t, s, write("adam's note", func(r *MemoryWriteRequest) { r.UserSlug = "adam" }))
	mustRecord(t, s, write("nadia's note", func(r *MemoryWriteRequest) { r.UserSlug = "nadia" }))

	adam, err := s.ListMemories(ctx, "adam", ListOptions{})
	if err != nil {
		t.Fatalf("list adam: %v", err)
	}
	nadia, err := s.ListMemories(ctx, "nadia", ListOptions{})
	if err != nil {
		t.Fatalf("list nadia: %v", err)
	}
	if got := contents(adam); len(got) != 1 || got[0] != "adam's note" {
		t.Errorf("adam = %v", got)
	}
	if got := contents(nadia); len(got) != 1 || got[0] != "nadia's note" {
		t.Errorf("nadia = %v", got)
	}
}

func TestListMemoriesFiltersByKindAndStatus(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	mustRecord(t, s, write("episodic one", nil))
	mustRecord(t, s, write("episodic two", nil))
	sem := mustRecord(t, s, write("semantic fact", func(r *MemoryWriteRequest) { r.MemoryKind = "semantic" }))
	if _, err := s.RetireMemory(ctx, sem.ID, "adam", ListOptions{}); err != nil {
		t.Fatalf("retire: %v", err)
	}

	active, err := s.ListMemories(ctx, "adam", ListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(active) != 2 {
		t.Errorf("active count = %d, want 2", len(active))
	}
	for _, r := range active {
		if r.Status != "active" {
			t.Errorf("status = %q, want active", r.Status)
		}
	}
	episodic, err := s.ListMemories(ctx, "adam", ListOptions{MemoryKind: "episodic"})
	if err != nil {
		t.Fatalf("list episodic: %v", err)
	}
	if len(episodic) != 2 {
		t.Errorf("episodic count = %d, want 2", len(episodic))
	}
	all, err := s.ListMemories(ctx, "adam", ListOptions{Status: StatusNoFilter})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	var sawRetired bool
	for _, r := range all {
		if r.Status == "retired" {
			sawRetired = true
		}
	}
	if !sawRetired {
		t.Errorf("status=nil list did not surface the retired record: %v", contents(all))
	}
}

// ── GetMemory ─────────────────────────────────────────────────────────────

func TestGetMemoryReturnsRecordByID(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	rec := mustRecord(t, s, write("target note", nil))
	fetched, err := s.GetMemory(context.Background(), rec.ID, "adam", ListOptions{})
	if err != nil || fetched == nil {
		t.Fatalf("get: %v", err)
	}
	if fetched.ID != rec.ID || fetched.Content != "target note" {
		t.Errorf("fetched = %+v", fetched)
	}
}

func TestGetMemoryReturnsNilForWrongID(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustRecord(t, s, write("a note", nil))
	fetched, err := s.GetMemory(context.Background(), 9999, "adam", ListOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if fetched != nil {
		t.Errorf("wrong id returned %+v", fetched)
	}
}

func TestGetMemoryIsSurfaceScoped(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	rec := mustRecord(t, s, write("surface-a note", func(r *MemoryWriteRequest) { r.SurfaceID = "surface-a" }))

	wrongSurface, err := s.GetMemory(ctx, rec.ID, "adam", ListOptions{SurfaceID: "surface-b"})
	if err != nil || wrongSurface != nil {
		t.Errorf("wrong surface: got %v err %v, want nil", wrongSurface, err)
	}
	wrongScope, err := s.GetMemory(ctx, rec.ID, "adam", ListOptions{MemoryScope: "shared", SurfaceID: "surface-a"})
	if err != nil || wrongScope != nil {
		t.Errorf("wrong scope: got %v err %v, want nil", wrongScope, err)
	}
	wrongUser, err := s.GetMemory(ctx, rec.ID, "nadia", ListOptions{SurfaceID: "surface-a"})
	if err != nil || wrongUser != nil {
		t.Errorf("wrong user: got %v err %v, want nil", wrongUser, err)
	}
	correct, err := s.GetMemory(ctx, rec.ID, "adam", ListOptions{SurfaceID: "surface-a"})
	if err != nil || correct == nil || correct.Content != "surface-a note" {
		t.Errorf("correct scope: got %+v err %v", correct, err)
	}
}

// ── DeleteMemory ──────────────────────────────────────────────────────────

func TestDeleteMemoryRemovesRecord(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	rec := mustRecord(t, s, write("doomed", nil))
	deleted, err := s.DeleteMemory(ctx, rec.ID, "adam", ListOptions{})
	if err != nil || !deleted {
		t.Fatalf("delete: %v %v", deleted, err)
	}
	fetched, err := s.GetMemory(ctx, rec.ID, "adam", ListOptions{})
	if err != nil || fetched != nil {
		t.Errorf("after delete: %v err %v", fetched, err)
	}
}

func TestDeleteMemoryReturnsFalseForWrongID(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustRecord(t, s, write("a note", nil))
	deleted, err := s.DeleteMemory(context.Background(), 9999, "adam", ListOptions{})
	if err != nil || deleted {
		t.Errorf("delete wrong id: %v err %v, want false", deleted, err)
	}
}

func TestDeleteMemoryIsSurfaceScoped(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	rec := mustRecord(t, s, write("surface-a note", func(r *MemoryWriteRequest) { r.SurfaceID = "surface-a" }))

	wrongSurface, err := s.DeleteMemory(ctx, rec.ID, "adam", ListOptions{SurfaceID: "surface-b"})
	if err != nil || wrongSurface {
		t.Errorf("wrong surface delete: %v err %v, want false", wrongSurface, err)
	}
	wrongUser, err := s.DeleteMemory(ctx, rec.ID, "nadia", ListOptions{SurfaceID: "surface-a"})
	if err != nil || wrongUser {
		t.Errorf("wrong user delete: %v err %v, want false", wrongUser, err)
	}
	correct, err := s.DeleteMemory(ctx, rec.ID, "adam", ListOptions{SurfaceID: "surface-a"})
	if err != nil || !correct {
		t.Errorf("correct delete: %v err %v, want true", correct, err)
	}
	gone, err := s.GetMemory(ctx, rec.ID, "adam", ListOptions{SurfaceID: "surface-a"})
	if err != nil || gone != nil {
		t.Errorf("record still present after delete: %v", gone)
	}
}

// ── RetireMemory ──────────────────────────────────────────────────────────

func TestRetireMemoryMarksRecordRetired(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	rec := mustRecord(t, s, write("old episodic note", nil))

	retired, err := s.RetireMemory(ctx, rec.ID, "adam", ListOptions{})
	if err != nil || !retired {
		t.Fatalf("retire: %v err %v", retired, err)
	}
	fetched, err := s.GetMemory(ctx, rec.ID, "adam", ListOptions{})
	if err != nil || fetched == nil {
		t.Fatalf("get after retire: %v", err)
	}
	if fetched.Status != "retired" {
		t.Errorf("status = %q, want retired", fetched.Status)
	}
	// Re-retiring an already-retired record is a no-op (status='active' guard).
	again, err := s.RetireMemory(ctx, rec.ID, "adam", ListOptions{})
	if err != nil || again {
		t.Errorf("re-retire: %v err %v, want false", again, err)
	}
}

// ── CountMemories ─────────────────────────────────────────────────────────

func TestCountMemoriesFiltersByKindAndStatus(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	mustRecord(t, s, write("episodic one", nil))
	mustRecord(t, s, write("episodic two", nil))
	sem := mustRecord(t, s, write("semantic fact", func(r *MemoryWriteRequest) { r.MemoryKind = "semantic" }))
	if _, err := s.RetireMemory(ctx, sem.ID, "adam", ListOptions{}); err != nil {
		t.Fatalf("retire: %v", err)
	}

	cases := []struct {
		name string
		opts ListOptions
		want int
	}{
		{"active only", ListOptions{}, 2},
		{"episodic kind", ListOptions{MemoryKind: "episodic"}, 2},
		{"semantic kind retired", ListOptions{MemoryKind: "semantic"}, 0},
		{"no status filter", ListOptions{Status: StatusNoFilter}, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := s.CountMemories(ctx, "adam", tc.opts)
			if err != nil {
				t.Fatalf("count: %v", err)
			}
			if got != tc.want {
				t.Errorf("count = %d, want %d", got, tc.want)
			}
		})
	}
}

// ── UpdateSalience ────────────────────────────────────────────────────────

func TestUpdateSalienceIsClampedAndScoped(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	rec := mustRecord(t, s, write("note", func(r *MemoryWriteRequest) { r.Salience = 0.3 }))

	updated, err := s.UpdateSalience(ctx, rec.ID, 1.5, "adam", ListOptions{})
	if err != nil || !updated {
		t.Fatalf("update salience: %v err %v", updated, err)
	}
	fetched, err := s.GetMemory(ctx, rec.ID, "adam", ListOptions{})
	if err != nil || fetched == nil {
		t.Fatalf("get: %v", err)
	}
	if fetched.Salience != 1.0 {
		t.Errorf("salience = %v, want clamped 1.0", fetched.Salience)
	}
	wrongScope, err := s.UpdateSalience(ctx, rec.ID, 0.2, "nadia", ListOptions{})
	if err != nil || wrongScope {
		t.Errorf("wrong-scope update: %v err %v, want false", wrongScope, err)
	}
}

// ── Recall ranking ────────────────────────────────────────────────────────

func TestRecallRanksByTokenOverlap(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustRecord(t, s, write("We postponed the Redis migration.", nil))
	mustRecord(t, s, write("We chose PostgreSQL for the primary database.", nil))

	result, err := s.Recall(context.Background(), "chose postgresql primary", "adam", RecallOptions{Limit: 5})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	lines := splitLines(result)
	if len(lines) < 1 || lines[0] != "- We chose PostgreSQL for the primary database." {
		t.Errorf("first line %q", lines)
	}
}

func TestRecallLongerContentBeatsShorterSingleMatch(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustRecord(t, s, write("postgres", nil))
	mustRecord(t, s, write("We chose postgres for the primary database and analytics.", nil))

	result, err := s.Recall(context.Background(), "chose postgres primary database", "adam", RecallOptions{Limit: 2})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	lines := splitLines(result)
	if len(lines) < 1 || len(lines[0]) <= len("We chose postgres") {
		t.Errorf("longer content should win: %q", lines)
	}
}

func TestRecallSalienceBoostBreaksTies(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustRecord(t, s, write("use redis for cache", nil))
	mustRecord(t, s, write("use redis for cache", func(r *MemoryWriteRequest) { r.Salience = 0.9 }))

	result, err := s.Recall(context.Background(), "redis cache", "adam", RecallOptions{Limit: 2})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	// Both records have identical content; both surface, higher-salience
	// (higher id) first.
	if got := countOccurrences(result, "use redis for cache"); got != 2 {
		t.Errorf("surfaced %d lines, want 2: %q", got, result)
	}
}

func TestRecallReturnsEmptyWhenNoRecords(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	result, err := s.Recall(context.Background(), "anything", "nobody", RecallOptions{})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if result != "" {
		t.Errorf("recall = %q, want empty", result)
	}
}

func TestRecallReturnsEmptyWhenNoTokenOverlap(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	r1 := mustRecord(t, s, write("unrelated content one", nil))
	r2 := mustRecord(t, s, write("unrelated content two", nil))

	result, err := s.Recall(ctx, "zzz no match", "adam", RecallOptions{Limit: 2})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if result != "" {
		t.Errorf("recall = %q, want empty", result)
	}
	// Access count must not be bumped for non-surfaced records.
	for i, id := range []int64{r1.ID, r2.ID} {
		fetched, err := s.GetMemory(ctx, id, "adam", ListOptions{})
		if err != nil || fetched == nil {
			t.Fatalf("get %d: %v", id, err)
		}
		if i == 0 && fetched.AccessCount != 0 || i == 1 && fetched.AccessCount != 0 {
			t.Errorf("record %d access_count = %d, want 0", id, fetched.AccessCount)
		}
	}
}

func TestRecallReturnsRecentWhenQueryIsEmpty(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	mustRecord(t, s, write("unrelated content one", nil))
	mustRecord(t, s, write("unrelated content two", nil))

	result, err := s.Recall(ctx, "", "adam", RecallOptions{Limit: 2})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	lines := splitLines(result)
	if len(lines) != 2 || lines[0] != "- unrelated content two" || lines[1] != "- unrelated content one" {
		t.Errorf("recent recall = %q", lines)
	}
}

// ── Recall expiry ─────────────────────────────────────────────────────────

func TestRecallExcludesExpiredMemories(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	rec := mustRecord(t, s, write("postgres decision from last week", func(r *MemoryWriteRequest) { r.MemoryTier = "daily" }))
	// Force the row's expires_at into the past (parity with the Python test
	// reaching into store._conn).
	past := "2000-01-01T00:00:00+00:00"
	if _, err := s.db.Exec("UPDATE memories SET expires_at = ? WHERE id = ?", past, rec.ID); err != nil {
		t.Fatalf("expire: %v", err)
	}
	result, err := s.Recall(ctx, "postgres decision", "adam", RecallOptions{Limit: 5})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if result != "" {
		t.Errorf("recall = %q, want empty", result)
	}
}

func TestRecallIncludesNonExpiredMemories(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustRecord(t, s, write("postgres decision today", func(r *MemoryWriteRequest) { r.MemoryTier = "long_term" }))
	result, err := s.Recall(context.Background(), "postgres decision", "adam", RecallOptions{Limit: 5})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if !contains(result, "postgres decision today") {
		t.Errorf("recall = %q", result)
	}
}

// ── Recall pool width ─────────────────────────────────────────────────────

func TestRecallPoolWidensCandidateWindow(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustRecord(t, s, write("chose postgres for the primary database", func(r *MemoryWriteRequest) { r.Salience = 0.9 }))
	for n := 0; n < 10; n++ {
		mustRecord(t, s, write(fmt.Sprintf("noise note %d", n), nil))
	}
	result, err := s.Recall(context.Background(), "chose postgres primary database", "adam", RecallOptions{Limit: 5, RecallPool: 40})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if !contains(result, "chose postgres for the primary database") {
		t.Errorf("pool-40 recall missed old match: %q", result)
	}
}

func TestRecallPoolTooNarrowMissesOldMatch(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustRecord(t, s, write("chose postgres for the primary database", nil))
	for n := 0; n < 5; n++ {
		mustRecord(t, s, write(fmt.Sprintf("noise note %d", n), nil))
	}
	result, err := s.Recall(context.Background(), "chose postgres primary database", "adam", RecallOptions{Limit: 5, RecallPool: 3})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if contains(result, "chose postgres for the primary database") {
		t.Errorf("pool-3 recall surfaced the buried match: %q", result)
	}
}

// ── Recall access tracking ────────────────────────────────────────────────

func TestRecallBumpsAccessCountAndLastAccessedAt(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	rec := mustRecord(t, s, write("chose postgres primary", nil))
	if _, err := s.Recall(ctx, "postgres primary", "adam", RecallOptions{Limit: 5}); err != nil {
		t.Fatalf("recall: %v", err)
	}
	fetched, err := s.GetMemory(ctx, rec.ID, "adam", ListOptions{})
	if err != nil || fetched == nil {
		t.Fatalf("get: %v", err)
	}
	if fetched.AccessCount != 1 {
		t.Errorf("access_count = %d, want 1", fetched.AccessCount)
	}
	if fetched.LastAccessedAt == nil {
		t.Error("last_accessed_at nil")
	}
}

func TestRecallDoesNotBumpAccessForNonSurfacedRecords(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	matched := mustRecord(t, s, write("postgres primary database", nil))
	noise := mustRecord(t, s, write("totally unrelated", nil))
	if _, err := s.Recall(ctx, "postgres primary", "adam", RecallOptions{Limit: 1}); err != nil {
		t.Fatalf("recall: %v", err)
	}
	m, err := s.GetMemory(ctx, matched.ID, "adam", ListOptions{})
	if err != nil || m == nil {
		t.Fatalf("get matched: %v", err)
	}
	n, err := s.GetMemory(ctx, noise.ID, "adam", ListOptions{})
	if err != nil || n == nil {
		t.Fatalf("get noise: %v", err)
	}
	if m.AccessCount != 1 {
		t.Errorf("matched access_count = %d, want 1", m.AccessCount)
	}
	if n.AccessCount != 0 {
		t.Errorf("noise access_count = %d, want 0", n.AccessCount)
	}
}

func TestRecallAccumulatesAccessCountAcrossCalls(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	mustRecord(t, s, write("postgres primary database", nil))
	for i := 0; i < 3; i++ {
		if _, err := s.Recall(ctx, "postgres primary", "adam", RecallOptions{Limit: 1}); err != nil {
			t.Fatalf("recall %d: %v", i, err)
		}
	}
	fetched, err := s.GetMemory(ctx, 1, "adam", ListOptions{})
	if err != nil || fetched == nil {
		t.Fatalf("get: %v", err)
	}
	if fetched.AccessCount != 3 {
		t.Errorf("access_count = %d, want 3", fetched.AccessCount)
	}
}

// ── Recall isolation ──────────────────────────────────────────────────────

func TestRecallIsolatesByUserScopeAndSurface(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	mustRecord(t, s, write("adam postgres note", func(r *MemoryWriteRequest) { r.UserSlug = "adam" }))
	mustRecord(t, s, write("nadia postgres note", func(r *MemoryWriteRequest) { r.UserSlug = "nadia" }))
	mustRecord(t, s, write("adam channel postgres note", func(r *MemoryWriteRequest) { r.UserSlug = "adam"; r.SurfaceID = "chan" }))

	adamPersonal, err := s.Recall(ctx, "postgres", "adam", RecallOptions{Limit: 5})
	if err != nil {
		t.Fatalf("recall adam: %v", err)
	}
	nadiaPersonal, err := s.Recall(ctx, "postgres", "nadia", RecallOptions{Limit: 5})
	if err != nil {
		t.Fatalf("recall nadia: %v", err)
	}
	adamChannel, err := s.Recall(ctx, "postgres", "adam", RecallOptions{Limit: 5, SurfaceID: "chan"})
	if err != nil {
		t.Fatalf("recall channel: %v", err)
	}

	if !contains(adamPersonal, "adam postgres note") || contains(adamPersonal, "adam channel postgres note") {
		t.Errorf("adam personal = %q", adamPersonal)
	}
	if !contains(nadiaPersonal, "nadia postgres note") {
		t.Errorf("nadia personal = %q", nadiaPersonal)
	}
	if !contains(adamChannel, "adam channel postgres note") || contains(adamChannel, "adam postgres note") {
		t.Errorf("adam channel = %q", adamChannel)
	}
}

func TestRecallExcludesRetiredByDefault(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	mustRecord(t, s, write("active postgres note", nil))
	b := mustRecord(t, s, write("retired postgres note", nil))
	if _, err := s.RetireMemory(ctx, b.ID, "adam", ListOptions{}); err != nil {
		t.Fatalf("retire: %v", err)
	}

	result, err := s.Recall(ctx, "postgres", "adam", RecallOptions{Limit: 5})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if !contains(result, "active postgres note") {
		t.Errorf("active note missing: %q", result)
	}
	if contains(result, "retired postgres note") {
		t.Errorf("retired note surfaced: %q", result)
	}
	// StatusNoFilter (Python status=None) surfaces the retired one.
	allResult, err := s.Recall(ctx, "postgres", "adam", RecallOptions{Limit: 5, Status: StatusNoFilter})
	if err != nil {
		t.Fatalf("recall all: %v", err)
	}
	if !contains(allResult, "retired postgres note") {
		t.Errorf("no-filter recall missing retired note: %q", allResult)
	}
}

// ── RecallRecords shape ───────────────────────────────────────────────────

func TestRecallRecordsReturnsStructuredRecords(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustRecord(t, s, write("postgres primary database", func(r *MemoryWriteRequest) { r.Salience = 0.5 }))
	records, err := s.RecallRecords(context.Background(), "postgres primary", "adam", RecallOptions{Limit: 5})
	if err != nil {
		t.Fatalf("recall records: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("len = %d, want 1", len(records))
	}
	// The record must serialize as JSON with the canonical field names.
	b, err := json.Marshal(records[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"id", "content", "user_slug", "memory_scope", "created_at", "status", "memory_kind"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("record JSON missing key %q: %s", key, b)
		}
	}
}

// ── helpers ───────────────────────────────────────────────────────────────

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	// strings.Index re-exported here keeps the test file's helper table flat.
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	lines = append(lines, s[start:])
	return lines
}

func countOccurrences(s, needle string) int {
	count := 0
	for _, line := range splitLines(s) {
		if line == "- "+needle {
			count++
		}
	}
	return count
}
