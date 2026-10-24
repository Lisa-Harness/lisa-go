package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
)

// RecordMemory stores a new memory snippet scoped to the provided user
// identity and surface, returning the persisted record. The embedding write
// is best-effort and never blocks or fails the write.
func (s *Store) RecordMemory(ctx context.Context, req MemoryWriteRequest) (MemoryRecord, error) {
	s.mu.Lock()
	rec, err := s.recordMemoryLocked(ctx, req)
	s.mu.Unlock()
	if err != nil {
		return MemoryRecord{}, err
	}
	// Opportunistic embedding write: best-effort, never blocks the memory
	// record. The provider call runs OUTSIDE the store mutex (parity: the
	// Python store awaits the embedding without holding anything a second
	// caller needs), so a slow/stalled endpoint cannot head-of-line block
	// other tool calls. An embedding failure (provider down, dimension
	// mismatch) logs and continues — the row is already durable in
	// memories; semantic recall simply won't find it until the embedding
	// is backfilled.
	s.writeEmbedding(ctx, rec.ID, req.Content)
	return rec, nil
}

// writeEmbedding persists an embedding for content into the memory_vec
// sidecar table WITHOUT holding the store mutex during the provider call.
// Best-effort: no-op when no provider is wired. Failures are logged and
// swallowed so a memory write never fails because of the sidecar.
func (s *Store) writeEmbedding(ctx context.Context, memoryID int64, content string) {
	if s.embeddingProvider == nil || !s.vec {
		return
	}
	vecs, err := s.embeddingProvider.Embed(ctx, []string{content})
	if err != nil {
		s.log.Warn("embedding write failed", "memory_id", memoryID, "err", err)
		return
	}
	if len(vecs) == 0 || len(vecs[0]) == 0 {
		return
	}
	vec := vecs[0]
	if len(vec) != s.embeddingDim {
		s.log.Warn("embedding dimension mismatch; skipping sidecar write",
			"memory_id", memoryID, "got", len(vec), "want", s.embeddingDim)
		return
	}
	b, err := json.Marshal(vec)
	if err != nil {
		s.log.Warn("embedding marshal failed", "memory_id", memoryID, "err", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO memory_vec (memory_id, embedding, dimension) VALUES (?, ?, ?)`,
		memoryID, string(b), len(vec)); err != nil {
		s.log.Warn("embedding write failed", "memory_id", memoryID, "err", err)
	}
}

// RecordMemoryLocked is the insert + read-back half of RecordMemory; the
// caller must hold s.mu.
func (s *Store) recordMemoryLocked(ctx context.Context, req MemoryWriteRequest) (MemoryRecord, error) {
	if s.db == nil {
		return MemoryRecord{}, ErrStoreClosed
	}
	// Defaults mirror the pydantic model for fields absent from JSON.
	if req.WorkspaceID == "" {
		req.WorkspaceID = "default"
	}
	if req.UserSlug == "" {
		req.UserSlug = "default"
	}
	if req.MemoryScope == "" {
		req.MemoryScope = "personal"
	}
	if req.AgentID == "" {
		req.AgentID = "core"
	}
	if req.MemoryTier == "" {
		req.MemoryTier = "daily"
	}
	if req.MemoryKind == "" {
		req.MemoryKind = "episodic"
	}
	if req.Confidence == 0 {
		req.Confidence = 1.0
	}
	if req.ContentJSON == nil {
		req.ContentJSON = map[string]any{}
	}
	if req.ItemIDs == nil {
		req.ItemIDs = []int64{}
	}

	eid := entryID()
	createdAt := nowISO()
	// Unknown/misspelled tiers fall back to the daily TTL (parity: Python's
	// default_ttl_days.get(tier, DEFAULT_TTL_DAYS["daily"])).
	ttl, ok := DefaultTTLDays[req.MemoryTier]
	if !ok {
		ttl = DefaultTTLDays["daily"]
	}
	expiry := expiresAt(ttl)

	var supersedes any
	if req.SupersedesID != nil {
		supersedes = *req.SupersedesID
	}
	itemJSON, err := json.Marshal(req.ItemIDs)
	if err != nil {
		return MemoryRecord{}, fmt.Errorf("marshal item_ids: %w", err)
	}
	contentJSON, err := json.Marshal(req.ContentJSON)
	if err != nil {
		return MemoryRecord{}, fmt.Errorf("marshal content_json: %w", err)
	}

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO memories (
			entry_id, user_slug, workspace_id, memory_scope, surface_id, agent_id,
			operation_id, instruction_id, memory_tier, memory_kind, status,
			supersedes_id, item_ids, confidence, merge_count, content,
			content_json, salience, created_at, expires_at, valid_until
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'active', ?, ?, ?, 0, ?, ?, ?, ?, ?, ?)`,
		eid, req.UserSlug, req.WorkspaceID, req.MemoryScope, req.SurfaceID,
		req.AgentID, req.OperationID, req.InstructionID, req.MemoryTier,
		req.MemoryKind, supersedes, string(itemJSON), req.Confidence,
		req.Content, string(contentJSON), req.Salience, createdAt, expiry,
		req.ValidUntil)
	if err != nil {
		return MemoryRecord{}, fmt.Errorf("insert memory: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return MemoryRecord{}, fmt.Errorf("memory insert id: %w", err)
	}
	rec, err := s.getMemoryLocked(ctx, id, req.UserSlug, req.WorkspaceID, req.MemoryScope, req.SurfaceID)
	if err != nil {
		return MemoryRecord{}, err
	}
	if rec == nil {
		return MemoryRecord{}, errors.New("failed to persist memory record")
	}
	return *rec, nil
}

// ListOptions carries the ListMemories / GetMemory / DeleteMemory /
// RetireMemory / UpdateSalience / CountMemories scope and filters.
//
// Status filter semantics (parity with the Python store signatures
// `status: str | None = "active"`): nil means the store default "active" —
// retired/superseded records drop out of the common listing automatically.
// Use StatusNoFilter to include all statuses (consolidation and admin
// views); the MCP layer maps an explicit JSON null to that sentinel.
// MemoryKind "" likewise means no kind filter.
type ListOptions struct {
	WorkspaceID string
	MemoryScope string
	SurfaceID   string
	Limit       int
	MemoryKind  string
	Status      *string // nil → "active" default
}

// StatusNoFilter disables the status filter entirely (Python status=None).
var StatusNoFilter = new(string) // points at "" — deliberately non-nil

// ListMemories lists memories for a surface, optionally filtered by kind and
// status, most recent first. status defaults to "active" so retired/superseded
// records drop out of the common listing automatically; pass StatusNoFilter
// to include all statuses (used by consolidation and admin views).
func (s *Store) ListMemories(ctx context.Context, userSlug string, opts ListOptions) ([]MemoryRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil, ErrStoreClosed
	}
	workspaceID := defaultIfEmpty(opts.WorkspaceID, "default")
	memoryScope := defaultIfEmpty(opts.MemoryScope, "personal")
	limit := opts.Limit
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+recordCols+` FROM memories
		WHERE user_slug = ? AND workspace_id = ? AND memory_scope = ? AND surface_id = ?
			AND (? IS NULL OR memory_kind = ?)
			AND (? IS NULL OR status = ?)
		ORDER BY id DESC
		LIMIT ?`,
		userSlug, workspaceID, memoryScope, opts.SurfaceID,
		kindArg(opts.MemoryKind), kindArg(opts.MemoryKind),
		statusArg(statusOrDefault(opts.Status)), statusArg(statusOrDefault(opts.Status)), limit)
	if err != nil {
		return nil, fmt.Errorf("list memories: %w", err)
	}
	return scanRecords(rows)
}

// GetMemory fetches a single memory record by id, scoped to the caller's
// surface. Returns nil when no row matches — the caller decides whether to
// treat that as "not found". Surface-scope filters (user_slug, workspace_id,
// memory_scope, surface_id) are applied so a caller cannot read across user
// or surface boundaries by guessing an id.
func (s *Store) GetMemory(ctx context.Context, memoryID int64, userSlug string, opts ListOptions) (*MemoryRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil, ErrStoreClosed
	}
	return s.getMemoryLocked(ctx, memoryID, userSlug,
		defaultIfEmpty(opts.WorkspaceID, "default"),
		defaultIfEmpty(opts.MemoryScope, "personal"), opts.SurfaceID)
}

func (s *Store) getMemoryLocked(ctx context.Context, memoryID int64, userSlug, workspaceID, memoryScope, surfaceID string) (*MemoryRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+recordCols+` FROM memories
		WHERE id = ? AND user_slug = ? AND workspace_id = ?
			AND memory_scope = ? AND surface_id = ?`,
		memoryID, userSlug, workspaceID, memoryScope, surfaceID)
	if err != nil {
		return nil, fmt.Errorf("get memory: %w", err)
	}
	recs, err := scanRecords(rows)
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, nil
	}
	return &recs[0], nil
}

// DeleteMemory deletes a single memory record by id, scoped to the caller's
// surface. Returns false when no matching record existed (wrong id or wrong
// scope). Surface-scope filters prevent cross-user / cross-surface deletion
// by id guessing.
func (s *Store) DeleteMemory(ctx context.Context, memoryID int64, userSlug string, opts ListOptions) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return false, ErrStoreClosed
	}
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM memories
		WHERE id = ? AND user_slug = ? AND workspace_id = ?
			AND memory_scope = ? AND surface_id = ?`,
		memoryID, userSlug,
		defaultIfEmpty(opts.WorkspaceID, "default"),
		defaultIfEmpty(opts.MemoryScope, "personal"), opts.SurfaceID)
	if err != nil {
		return false, fmt.Errorf("delete memory: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("delete memory rows affected: %w", err)
	}
	return n > 0, nil
}

// RetireMemory marks a memory record retired, scoped to the caller's
// surface. Retirement (not deletion) preserves the audit trail — the record
// stays queryable with status=nil but drops out of the default
// status="active" recall pool. Used by consolidation to retire episodic
// originals after a semantic successor is written. Returns false when no
// active record matched (re-retiring an already-retired record is a no-op
// via the status='active' guard).
func (s *Store) RetireMemory(ctx context.Context, memoryID int64, userSlug string, opts ListOptions) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return false, ErrStoreClosed
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE memories SET status = 'retired'
		WHERE id = ? AND user_slug = ? AND workspace_id = ?
			AND memory_scope = ? AND surface_id = ? AND status = 'active'`,
		memoryID, userSlug,
		defaultIfEmpty(opts.WorkspaceID, "default"),
		defaultIfEmpty(opts.MemoryScope, "personal"), opts.SurfaceID)
	if err != nil {
		return false, fmt.Errorf("retire memory: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("retire memory rows affected: %w", err)
	}
	return n > 0, nil
}

// UpdateSalience updates a record's salience, clamped to [0,1] and scoped to
// the caller's surface. Hook for decay-aware dynamic salience (the
// consolidation slice boosts the semantic successor's salience). Returns
// false when no row matched.
func (s *Store) UpdateSalience(ctx context.Context, memoryID int64, salience float64, userSlug string, opts ListOptions) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return false, ErrStoreClosed
	}
	clamped := math.Max(0.0, math.Min(1.0, salience))
	res, err := s.db.ExecContext(ctx, `
		UPDATE memories SET salience = ?
		WHERE id = ? AND user_slug = ? AND workspace_id = ?
			AND memory_scope = ? AND surface_id = ?`,
		clamped, memoryID, userSlug,
		defaultIfEmpty(opts.WorkspaceID, "default"),
		defaultIfEmpty(opts.MemoryScope, "personal"), opts.SurfaceID)
	if err != nil {
		return false, fmt.Errorf("update salience: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("update salience rows affected: %w", err)
	}
	return n > 0, nil
}

// CountMemories counts memories for a surface, optionally filtered by kind
// and status. Used by the consolidation slice to compute episodic pressure
// without pulling rows. A nil status means "no filter" — the MCP layer
// defaults to "active" to match ListMemories.
func (s *Store) CountMemories(ctx context.Context, userSlug string, opts ListOptions) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return 0, ErrStoreClosed
	}
	count, err := s.countMemoriesLocked(ctx, userSlug,
		defaultIfEmpty(opts.WorkspaceID, "default"),
		defaultIfEmpty(opts.MemoryScope, "personal"), opts.SurfaceID,
		opts.MemoryKind, opts.Status)
	if err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) countMemoriesLocked(ctx context.Context, userSlug, workspaceID, memoryScope, surfaceID, memoryKind string, status *string) (int, error) {
	status = statusOrDefault(status)
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM memories
		WHERE user_slug = ? AND workspace_id = ? AND memory_scope = ? AND surface_id = ?
			AND (? IS NULL OR memory_kind = ?)
			AND (? IS NULL OR status = ?)`,
		userSlug, workspaceID, memoryScope, surfaceID,
		kindArg(memoryKind), kindArg(memoryKind),
		statusArg(status), statusArg(status)).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count memories: %w", err)
	}
	return count, nil
}

func defaultIfEmpty(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return strings.TrimSpace(v)
}

// statusOrDefault applies the store-level "active" default: nil → "active",
// StatusNoFilter ("" pointer) → nil (no filter), any other value → itself.
func statusOrDefault(status *string) *string {
	if status == nil {
		active := "active"
		return &active
	}
	if *status == "" {
		return nil
	}
	return status
}
