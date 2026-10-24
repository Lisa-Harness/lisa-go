package memory

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// MemoryWriteRequest is the canonical write shape (kernel MemoryWriteRequest).
// Field defaults mirror the pydantic model so omitted JSON fields keep the
// Python-observed values; RecordMemory applies the same fallback defaults
// store-side.
type MemoryWriteRequest struct {
	WorkspaceID    string         `json:"workspace_id"`
	UserID         string         `json:"user_id"`
	UserSlug       string         `json:"user_slug"`
	MemoryScope    string         `json:"memory_scope"`
	SurfaceID      string         `json:"surface_id"`
	SourceUserID   string         `json:"source_user_id"`
	SourceUserSlug string         `json:"source_user_slug"`
	AgentID        string         `json:"agent_id"`
	OperationID    string         `json:"operation_id"`
	InstructionID  string         `json:"instruction_id"`
	MemoryTier     string         `json:"memory_tier"`
	Content        string         `json:"content"`
	ContentJSON    map[string]any `json:"content_json"`
	Salience       float64        `json:"salience"`
	MemoryKind     string         `json:"memory_kind"`
	Confidence     float64        `json:"confidence"`
	ItemIDs        []int64        `json:"item_ids"`
	SupersedesID   *int64         `json:"supersedes_id"`
	ValidUntil     *string        `json:"valid_until"`
}

// MemoryRecord is the canonical read shape (kernel MemoryRecord). The four
// derived fields Python's row mapper fills (user_id, source_user_id and
// source_user_slug from user_slug; valid_until from expires_at when unset)
// are computed in rowToRecord — same transforms, same order. The DB's
// expires_at column is NOT exposed: the pydantic MemoryRecord has no such
// field, and validity surfaces through valid_until only (crosscheck-locked
// against the Python model_dump shape).
type MemoryRecord struct {
	ID             int64          `json:"id"`
	WorkspaceID    string         `json:"workspace_id"`
	UserID         string         `json:"user_id"`
	UserSlug       string         `json:"user_slug"`
	MemoryScope    string         `json:"memory_scope"`
	SurfaceID      string         `json:"surface_id"`
	SourceUserID   string         `json:"source_user_id"`
	SourceUserSlug string         `json:"source_user_slug"`
	AgentID        string         `json:"agent_id"`
	OperationID    string         `json:"operation_id"`
	InstructionID  string         `json:"instruction_id"`
	MemoryTier     string         `json:"memory_tier"`
	Content        string         `json:"content"`
	ContentJSON    map[string]any `json:"content_json"`
	Salience       float64        `json:"salience"`
	CreatedAt      string         `json:"created_at"`
	MemoryKind     string         `json:"memory_kind"`
	Confidence     float64        `json:"confidence"`
	ItemIDs        []int64        `json:"item_ids"`
	SupersedesID   *int64         `json:"supersedes_id"`
	ValidUntil     *string        `json:"valid_until"`
	LastAccessedAt *string        `json:"last_accessed_at"`
	AccessCount    int64          `json:"access_count"`
	Status         string         `json:"status"`
}

// recordCols is the SELECT column list shared by every record query,
// matching the Python `SELECT *` column order that _row_to_record indexes.
const recordCols = `id, entry_id, user_slug, workspace_id, memory_scope, surface_id,
	agent_id, operation_id, instruction_id, memory_tier, memory_kind, status,
	supersedes_id, item_ids, confidence, merge_count, content, content_json,
	salience, created_at, expires_at, valid_until, access_count, last_accessed_at`

// rowToRecord maps one memories row to a MemoryRecord via the same
// transformations as the Python _row_to_record:
//
//   - JSON-string columns (content_json, item_ids) decode with fallbacks
//     ("{}" / "[]") on malformed payloads;
//   - source_user_id / source_user_slug / user_id derive from user_slug
//     (there is no DB column for them);
//   - valid_until falls back to expires_at (the tier TTL) when unset;
//   - a missing required field surfaces as a scan error instead of
//     silently producing a nil field.
func rowToRecord(scan func(dest ...any) error) (MemoryRecord, error) {
	var (
		rec          MemoryRecord
		entryID      string
		supersedesID sql.NullInt64
		itemIDsRaw   sql.NullString
		contentJSON  sql.NullString
		expiresAt    sql.NullString
		validUntil   sql.NullString
		lastAccessed sql.NullString
	)
	err := scan(
		&rec.ID, &entryID, &rec.UserSlug, &rec.WorkspaceID, &rec.MemoryScope,
		&rec.SurfaceID, &rec.AgentID, &rec.OperationID, &rec.InstructionID,
		&rec.MemoryTier, &rec.MemoryKind, &rec.Status, &supersedesID,
		&itemIDsRaw, &rec.Confidence, new(int), &rec.Content, &contentJSON,
		&rec.Salience, &rec.CreatedAt, &expiresAt, &validUntil,
		&rec.AccessCount, &lastAccessed,
	)
	if err != nil {
		return MemoryRecord{}, fmt.Errorf("scan memory row: %w", err)
	}
	// JSON-string columns → typed fields (malformed falls back, as Python).
	if rec.ContentJSON, err = decodeJSONObject(contentJSON); err != nil {
		return MemoryRecord{}, err
	}
	if rec.ItemIDs, err = decodeInt64List(itemIDsRaw); err != nil {
		return MemoryRecord{}, err
	}
	if supersedesID.Valid {
		v := supersedesID.Int64
		rec.SupersedesID = &v
	}
	// No DB column for source_user_* — derive from user_slug.
	rec.SourceUserID = rec.UserSlug
	rec.SourceUserSlug = rec.UserSlug
	rec.UserID = rec.UserSlug
	// TTL expiry is the default validity bound. An explicit valid_until
	// (set on write) overrides it for semantic-validity windows that differ
	// from the tier TTL.
	if validUntil.Valid {
		v := validUntil.String
		rec.ValidUntil = &v
	} else if expiresAt.Valid {
		v := expiresAt.String
		rec.ValidUntil = &v
	}
	if lastAccessed.Valid {
		v := lastAccessed.String
		rec.LastAccessedAt = &v
	}
	return rec, nil
}

func scanRecords(rows *sql.Rows) ([]MemoryRecord, error) {
	defer rows.Close()
	var records []MemoryRecord
	for rows.Next() {
		rec, err := rowToRecord(rows.Scan)
		if err != nil {
			return nil, err
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate memory rows: %w", err)
	}
	if records == nil {
		// Python returns [] (never None) from its list/record paths; a JSON
		// empty list, not null (crosscheck-locked).
		records = []MemoryRecord{}
	}
	return records, nil
}

// decodeJSONObject decodes a content_json column. Mirrors the Python mapper:
// NULL/"" become {} and a malformed JSON string degrades to {} rather than
// failing the read (the column was written by the store itself).
func decodeJSONObject(raw sql.NullString) (map[string]any, error) {
	if !raw.Valid || raw.String == "" {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw.String), &out); err != nil || out == nil {
		return map[string]any{}, nil
	}
	return out, nil
}

// decodeInt64List decodes an item_ids column: NULL/"" → []; malformed → [].
func decodeInt64List(raw sql.NullString) ([]int64, error) {
	if !raw.Valid || raw.String == "" {
		return []int64{}, nil
	}
	var out []int64
	if err := json.Unmarshal([]byte(raw.String), &out); err != nil || out == nil {
		return []int64{}, nil
	}
	return out, nil
}

// recordJSON serializes a MemoryRecord the way the MCP layer returns it
// (Python: model_dump(mode="json")). content_json is re-marshaled from the
// typed map, so key order follows Go's map iteration — consumers parse JSON
// and never depend on map key order.
func recordJSON(rec MemoryRecord) ([]byte, error) {
	return json.Marshal(rec)
}
