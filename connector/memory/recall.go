package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// RecallOptions carries the recall call's optional parameters. Defaults
// mirror the Python recall(**kwargs) coercion: empty user/workspace fall
// back to "default", scope to "personal", limit to 5, recall_pool to 40,
// status to "active". Use StatusNoFilter to include all statuses (Python
// status=None); the MCP layer maps an explicit JSON null to that sentinel.
type RecallOptions struct {
	WorkspaceID string
	MemoryScope string
	SurfaceID   string
	Limit       int
	RecallPool  int
	MemoryKind  string
	Status      *string // nil → "active" default
}

// recallPoolDefault mirrors the Python recall kwargs default.
const recallPoolDefault = 40

// Recall renders the ranked recall as the bullet-list prose the Python
// recall() returns ("- <content>" lines joined by newline). Empty string
// when nothing matches.
func (s *Store) Recall(ctx context.Context, query, userSlug string, opts RecallOptions) (string, error) {
	records, err := s.RecallRecords(ctx, query, userSlug, opts)
	if err != nil || len(records) == 0 {
		return "", err
	}
	lines := make([]string, 0, len(records))
	for _, r := range records {
		lines = append(lines, "- "+r.Content)
	}
	return strings.Join(lines, "\n"), nil
}

// RecallRecords ranks and returns the matching memory records (no prose
// rendering). Same ranking and opportunistic access tracking as Recall, but
// returns the structured records so the MCP search_memory tool can serialize
// them as a JSON array (the recall_memory operation json.loads the result;
// the prose output is not JSON-parseable).
func (s *Store) RecallRecords(ctx context.Context, query, userSlug string, opts RecallOptions) ([]MemoryRecord, error) {
	// The semantic candidate pool's embedding call runs OUTSIDE the store
	// mutex (parity: the Python store awaits the provider without holding
	// anything a second caller needs), so a slow endpoint cannot stall
	// other memory tools for the provider timeout. The DB work re-acquires
	// the lock.
	queryVec, semPreErr := s.embedQuery(ctx, query)
	if semPreErr != nil {
		// Python: semantic recall failure falls back to lexical (logged).
		s.log.Warn("semantic recall failed; falling back to lexical", "err", semPreErr)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil, ErrStoreClosed
	}
	return s.recallRecordsLocked(ctx, query, userSlug, opts, queryVec)
}

// embedQuery resolves the recall query's embedding outside the store
// mutex. Returns (nil, nil) when semantic recall is not wired or the query
// is blank — both mean "use the lexical pool".
func (s *Store) embedQuery(ctx context.Context, query string) ([]float64, error) {
	if s.embeddingProvider == nil || !s.vec || strings.TrimSpace(query) == "" {
		return nil, nil
	}
	vecs, err := s.embeddingProvider.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 || len(vecs[0]) == 0 {
		return nil, nil
	}
	qvec := vecs[0]
	if len(qvec) != s.embeddingDim {
		return nil, fmt.Errorf("embedding dimension mismatch: got %d want %d", len(qvec), s.embeddingDim)
	}
	return qvec, nil
}

func (s *Store) recallRecordsLocked(ctx context.Context, query, userSlug string, opts RecallOptions, queryVec []float64) ([]MemoryRecord, error) {
	userSlug = strings.TrimSpace(userSlug)
	if userSlug == "" {
		userSlug = "default"
	}
	workspaceID := defaultIfEmpty(opts.WorkspaceID, "default")
	memoryScope := defaultIfEmpty(opts.MemoryScope, "personal")
	surfaceID := strings.TrimSpace(opts.SurfaceID)
	limit := opts.Limit
	if limit <= 0 {
		limit = 5
	}
	recallPool := opts.RecallPool
	if recallPool <= 0 {
		recallPool = recallPoolDefault
	}
	status := statusOrDefault(opts.Status)

	// Tokenize like Python: set of whitespace tokens, lowercased.
	tokens := map[string]bool{}
	for _, tok := range strings.Fields(query) {
		tokens[strings.ToLower(tok)] = true
	}
	nowISOStr := nowISO()

	// Semantic pool first (when wired); its presence is a ranker signal
	// even when the query tokens do not overlap the content. The query
	// embedding was resolved outside the lock (embedQuery); when the
	// provider failed there, the pool is skipped and the lexical pool
	// ranks instead.
	isSemantic := false
	var poolRows []MemoryRecord
	if queryVec != nil {
		rows, err := s.semanticPoolLocked(ctx, queryVec, userSlug, workspaceID, memoryScope, surfaceID, opts.MemoryKind, status, nowISOStr, recallPool)
		if err != nil {
			// Python: semantic recall failure falls back to lexical
			// (logged); the DB-side failure takes the same fallback.
			s.log.Warn("semantic recall failed; falling back to lexical", "err", err)
			rows = nil
		}
		poolRows = rows
	}
	if len(poolRows) > 0 {
		isSemantic = true
	} else {
		var err error
		poolRows, err = s.lexicalPoolLocked(userSlug, workspaceID, memoryScope, surfaceID, opts.MemoryKind, status, nowISOStr, recallPool)
		if err != nil {
			return nil, err
		}
	}
	if len(poolRows) == 0 {
		return []MemoryRecord{}, nil
	}

	type rankedRecord struct {
		score  float64
		record MemoryRecord
	}
	ranked := make([]rankedRecord, 0, len(poolRows))
	for _, rec := range poolRows {
		// Haystack mirrors Python: content + ensure_ascii JSON of
		// content_json, lowercased. Go's json.Marshal escapes <, >, & by
		// default and escapes non-ASCII identically enough for ranking —
		// byte-exactness does not matter, only token overlap does.
		haystackBytes, err := json.Marshal(rec.ContentJSON)
		if err != nil {
			haystackBytes = []byte("{}")
		}
		haystackLower := strings.ToLower(rec.Content + " " + string(haystackBytes))
		var lexical float64
		if len(tokens) > 0 {
			haystackTokens := map[string]bool{}
			for _, tok := range strings.Fields(haystackLower) {
				haystackTokens[tok] = true
			}
			overlap := 0
			for t := range tokens {
				if haystackTokens[t] {
					overlap++
				}
			}
			if !isSemantic && overlap == 0 {
				continue
			}
			// Normalize by both query size and content length to avoid
			// rewarding short content with a single matching keyword.
			contentWordCount := len(haystackTokens)
			if contentWordCount < 1 {
				contentWordCount = 1
			}
			lexical = float64(overlap) / float64(len(tokens)) * (1.0 - 1.0/float64(contentWordCount))
		} else {
			lexical = 1.0
		}
		salienceBoost := math.Max(rec.Salience, 0.0) * 0.2
		ranked = append(ranked, rankedRecord{lexical + salienceBoost, rec})
	}
	if len(ranked) == 0 {
		return []MemoryRecord{}, nil
	}

	// (score, id) descending; the id tiebreak is part of the Python key.
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].record.ID > ranked[j].record.ID
	})
	topN := limit
	if topN < 1 {
		topN = 1
	}
	if topN > 8 {
		topN = 8
	}
	if len(ranked) < topN {
		topN = len(ranked)
	}
	top := ranked[:topN]

	// Opportunistic access tracking: bump access_count and
	// last_accessed_at for the records we actually surfaced. Piggybacks on
	// the recall call — no separate maintenance pass needed. Only positive
	// matches reach this point (zero-overlap candidates are filtered
	// above), so the logged count reflects genuine salience, not recency
	// noise.
	topIDs := make([]int64, 0, len(top))
	for _, r := range top {
		topIDs = append(topIDs, r.record.ID)
	}
	if err := s.bumpAccessLocked(ctx, topIDs, nowISOStr); err != nil {
		return nil, fmt.Errorf("access-tracking update: %w", err)
	}
	records := make([]MemoryRecord, 0, len(top))
	for _, r := range top {
		records = append(records, r.record)
	}
	return records, nil
}

// semanticPoolLocked is the KNN candidate pool over the stored-embedding
// sidecar, re-ranked by the caller. qvec is the caller-provided query
// embedding resolved outside the store mutex. Returns an empty list (no
// error) when qvec is nil — the caller falls back to the lexical pool.
// Python delegates distance ordering to sqlite-vec MATCH KNN ?; here the
// nearest-by-L2 candidates within the scope/expiry filters are ordered in
// Go, then truncated to the pool width.
func (s *Store) semanticPoolLocked(ctx context.Context, qvec []float64, userSlug, workspaceID, memoryScope, surfaceID, memoryKind string, status *string, nowISOStr string, recallPool int) ([]MemoryRecord, error) {
	if len(qvec) == 0 {
		return nil, nil
	}

	status = statusOrDefault(status)
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id, v.embedding FROM memories m
		JOIN memory_vec v ON v.memory_id = m.id
		WHERE m.user_slug = ? AND m.workspace_id = ? AND m.memory_scope = ? AND m.surface_id = ?
			AND (? IS NULL OR m.memory_kind = ?)
			AND (? IS NULL OR m.status = ?)
			AND (m.expires_at IS NULL OR m.expires_at >= ?)`,
		userSlug, workspaceID, memoryScope, surfaceID,
		kindArg(memoryKind), kindArg(memoryKind),
		statusArg(status), statusArg(status), nowISOStr)
	if err != nil {
		return nil, fmt.Errorf("semantic pool: %w", err)
	}
	type candidate struct {
		id   int64
		dist float64
	}
	var candidates []candidate
	for rows.Next() {
		var id int64
		var embJSON string
		if err := rows.Scan(&id, &embJSON); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan semantic pool row: %w", err)
		}
		var vec []float64
		if err := json.Unmarshal([]byte(embJSON), &vec); err != nil || len(vec) != len(qvec) {
			continue
		}
		candidates = append(candidates, candidate{id, l2Squared(qvec, vec)})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("semantic pool rows: %w", err)
	}
	rows.Close()

	sort.Slice(candidates, func(i, j int) bool { return candidates[i].dist < candidates[j].dist })
	if len(candidates) > recallPool {
		candidates = candidates[:recallPool]
	}
	// Fetch full records for the candidate ids (parity: SELECT m.* over the
	// JOIN), preserving MATCH order.
	records := make([]MemoryRecord, 0, len(candidates))
	for _, c := range candidates {
		rec, err := s.getMemoryLocked(ctx, c.id, userSlug, workspaceID, memoryScope, surfaceID)
		if err != nil {
			return nil, err
		}
		if rec != nil {
			records = append(records, *rec)
		}
	}
	return records, nil
}

func l2Squared(a, b []float64) float64 {
	sum := 0.0
	for i := range a {
		d := a[i] - b[i]
		sum += d * d
	}
	return sum
}

// lexicalPoolLocked is the recent-rows candidate pool for lexical recall
// ranking (expired rows excluded; status defaulting already applied).
func (s *Store) lexicalPoolLocked(userSlug, workspaceID, memoryScope, surfaceID, memoryKind string, status *string, nowISOStr string, recallPool int) ([]MemoryRecord, error) {
	rows, err := s.db.Query(`
		SELECT `+recordCols+` FROM memories
		WHERE user_slug = ? AND workspace_id = ? AND memory_scope = ?
			AND surface_id = ? AND (expires_at IS NULL OR expires_at >= ?)
			AND (? IS NULL OR memory_kind = ?)
			AND (? IS NULL OR status = ?)
		ORDER BY id DESC
		LIMIT ?`,
		userSlug, workspaceID, memoryScope, surfaceID, nowISOStr,
		kindArg(memoryKind), kindArg(memoryKind),
		statusArg(status), statusArg(status), recallPool)
	if err != nil {
		return nil, fmt.Errorf("lexical pool: %w", err)
	}
	return scanRecords(rows)
}

func (s *Store) bumpAccessLocked(ctx context.Context, ids []int64, nowISOStr string) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)+1)
	args = append(args, nowISOStr)
	for _, id := range ids {
		args = append(args, id)
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE memories
		SET access_count = access_count + 1, last_accessed_at = ?
		WHERE id IN (`+placeholders+`)`, args...)
	if err != nil {
		return fmt.Errorf("bump access: %w", err)
	}
	return nil
}
