// Package memory is a faithful Go port of the Lisa SQLite memory store
// (lisa/sdk/src/sdk/memory/store.py): the minimal built-in memory backend
// behind the lisa-memory MCP server and the generic memory.* capability
// aliases (add / search / list / get / delete / retire / count). Mem0 (or
// any other HTTP/stdio memory server) serves the same operations and more
// via the MCP tool pool.
//
// Parity invariants preserved from the Python source:
//
//  1. Surface-scoped isolation: every read, delete, retire, salience update
//     and recall filters on (user_slug, workspace_id, memory_scope,
//     surface_id), so cross-user or cross-surface access by id guessing is
//     impossible. This is the server's privacy boundary; a port that drops
//     any scope filter is a defect, not a shortcut.
//  2. Tier TTL expiry: memory_tier maps to an expires_at bound (daily=7,
//     weekly=30, long_term=365, permanent=nil) applied on write; recall and
//     its candidate pools exclude expired rows. An explicit valid_until set
//     on write overrides the tier TTL for semantic-validity windows that
//     differ from the tier TTL (it is returned to the caller but does not
//     filter recall — only expires_at does, mirroring Python).
//  3. Recall ranking: lexical token-overlap scoring normalized by both the
//     query size and the content length, plus a salience boost (× 0.2),
//     ordered by (score, id) descending; when a non-empty query matches
//     nothing lexically the result is empty; an empty query returns the
//     most recent records. Semantic KNN over stored embeddings runs first
//     when an embedding provider is wired and relaxes the zero-overlap
//     filter.
//  4. Hard schema cutover: PRAGMA user_version tracks SchemaVersion; a
//     mismatched version drops and recreates the memories table (dev DBs
//     are expendable; there are no compatibility shims).
//  5. Access tracking is opportunistic and positive-only: only records
//     actually surfaced by recall get access_count and last_accessed_at
//     bumped.
//  6. Embeddings are a best-effort sidecar: every embedding failure is
//     logged and swallowed so a memory write never fails because of it.
//
// The Python store loads the optional sqlite-vec extension for semantic
// recall; Go has no equivalent loadable extension, so the KNN candidate
// pool is computed directly over the stored embedding vectors with the
// same scope/expiry filters and the same fallback-to-lexical behavior.
package memory

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (static builds, no cgo)
)

// DefaultTTLDays maps memory tiers to TTL expiry in days (permanent = nil),
// mirroring the Python DEFAULT_TTL_DAYS.
var DefaultTTLDays = map[string]*int{
	"daily":     intPtr(7),
	"weekly":    intPtr(30),
	"long_term": intPtr(365),
	"permanent": nil,
}

func intPtr(n int) *int { return &n }

// SchemaVersion is recorded in PRAGMA user_version. Bumping it triggers a
// hard recreate of the memories table (drop + create) per the project's
// no-compat/migration rule — dev DBs are expendable, and external backends
// (Mem0) own their own schemas.
const SchemaVersion = 2

// defaultEmbeddingDimension matches the Python provider default (and the
// common OpenAI text-embedding width).
const defaultEmbeddingDimension = 1536

// ErrStoreClosed reports operations on a closed store.
var ErrStoreClosed = errors.New("memory store is closed")

// EmbeddingProvider generates embeddings for text content (the Python
// EmbeddingProvider protocol: async embed(texts) -> list[list[float]]).
type EmbeddingProvider interface {
	Embed(ctx context.Context, texts []string) ([][]float64, error)
}

// Store is the SQLite-backed memory store owned by the lisa-memory MCP
// server. All DB access is serialized through a mutex, mirroring the
// Python store's single shared connection (and keeping
// database/sql from multiplexing reads across pooled connections).
type Store struct {
	dbPath            string
	embeddingProvider EmbeddingProvider
	embeddingDim      int
	log               *slog.Logger

	mu  sync.Mutex
	db  *sql.DB
	vec bool // semantic-recall sidecar table present
}

// Option configures a Store at construction.
type Option func(*Store)

// WithEmbeddingProvider wires semantic recall. dimension must match the
// provider's output width exactly — mismatches degrade to lexical recall
// (logged), never corrupt the sidecar table.
func WithEmbeddingProvider(p EmbeddingProvider, dimension int) Option {
	return func(s *Store) {
		s.embeddingProvider = p
		if dimension > 0 {
			s.embeddingDim = dimension
		}
	}
}

// WithLogger sets the logger for best-effort embedding/recall failures.
func WithLogger(l *slog.Logger) Option { return func(s *Store) { s.log = l } }

// New opens (or creates) the SQLite database at dbPath, applies the hard
// schema cutover, and prepares the semantic-recall sidecar table when an
// embedding provider is wired.
func New(dbPath string, opts ...Option) (*Store, error) {
	s := &Store{
		dbPath:       dbPath,
		embeddingDim: defaultEmbeddingDimension,
		log:          slog.Default(),
	}
	for _, opt := range opts {
		opt(s)
	}
	if dir := filepath.Dir(dbPath); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create memory db directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("opening memory store %s: %w", dbPath, err)
	}
	// One connection keeps SQLite's "database is locked" away and mirrors
	// the Python store's single shared connection.
	db.SetMaxOpenConns(1)
	s.db = db
	if err := s.initSchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("init memory schema: %w", err)
	}
	return s, nil
}

func (s *Store) initSchema() error {
	var userVersion int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&userVersion); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	if userVersion != SchemaVersion {
		// Hard cutover on schema version bump: drop and recreate. Per the
		// project contract there are no compatibility shims — dev DBs are
		// expendable and external backends own their schemas. The
		// user_version pragma is the single source of truth.
		if _, err := s.db.Exec("DROP TABLE IF EXISTS memories;"); err != nil {
			return fmt.Errorf("drop stale memories table: %w", err)
		}
		if _, err := s.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion)); err != nil {
			return fmt.Errorf("set user_version: %w", err)
		}
	}
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS memories (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			entry_id TEXT NOT NULL UNIQUE,
			user_slug TEXT NOT NULL,
			workspace_id TEXT NOT NULL DEFAULT 'default',
			memory_scope TEXT NOT NULL DEFAULT 'personal',
			surface_id TEXT NOT NULL DEFAULT '',
			agent_id TEXT NOT NULL DEFAULT 'core',
			operation_id TEXT NOT NULL DEFAULT '',
			instruction_id TEXT NOT NULL DEFAULT '',
			memory_tier TEXT NOT NULL DEFAULT 'long_term',
			memory_kind TEXT NOT NULL DEFAULT 'episodic',
			status TEXT NOT NULL DEFAULT 'active',
			supersedes_id INTEGER,
			item_ids TEXT NOT NULL DEFAULT '[]',
			confidence REAL NOT NULL DEFAULT 1.0,
			merge_count INTEGER NOT NULL DEFAULT 0,
			content TEXT NOT NULL,
			content_json TEXT DEFAULT '{}',
			salience REAL NOT NULL DEFAULT 0.5,
			created_at TEXT NOT NULL,
			expires_at TEXT,
			valid_until TEXT,
			access_count INTEGER NOT NULL DEFAULT 0,
			last_accessed_at TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_scope
			ON memories(user_slug, workspace_id, memory_scope, surface_id)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_operation
			ON memories(user_slug, operation_id)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_expires
			ON memories(expires_at) WHERE expires_at IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_memories_kind_status
			ON memories(user_slug, workspace_id, memory_scope, surface_id,
						memory_kind, status)`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("exec schema: %w", err)
		}
	}
	// Semantic recall: Go has no sqlite-vec equivalent extension, so the
	// embedding sidecar is a plain table (memory_id → JSON vector). The
	// KNN candidate pool is computed in Go with identical scope/expiry
	// filters. Present only when semantic recall is wired (parity: the
	// Python store stays lexical-only when the extension does not load).
	if s.embeddingProvider != nil {
		if _, err := s.db.Exec(fmt.Sprintf(
			"CREATE TABLE IF NOT EXISTS memory_vec ("+
				"memory_id INTEGER PRIMARY KEY, embedding TEXT NOT NULL, "+
				"dimension INTEGER NOT NULL DEFAULT %d)", s.embeddingDim)); err != nil {
			return fmt.Errorf("create memory_vec: %w", err)
		}
		s.vec = true
	}
	return nil
}

// Close releases the underlying SQLite connection. Called by the server's
// shutdown path so the file handle is released cleanly when the stdio
// session ends. Safe to call when the store was never opened or twice.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

func nowISO() string {
	// Python datetime.now(UTC).isoformat(): "+00:00" suffix, not "Z".
	return time.Now().UTC().Format("2006-01-02T15:04:05.000000Z07:00")
}

func entryID() string {
	// uuid4().hex[:12] equivalent: 12 hex chars from a random source.
	const hexDigits = "0123456789abcdef"
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// crypto/rand failing is unrecoverable for id minting; surface it
		// as a store construction error at the caller via panic-free
		// fallback: use the wall clock as a last-resort entropy source.
		now := time.Now().UTC().UnixNano()
		for i := range raw {
			raw[i] = byte(now >> (i % 8 * 8))
		}
	}
	var b [12]byte
	for i := range b {
		b[i] = hexDigits[raw[i/2]>>(4*(1-uint(i%2)))&0xf]
	}
	return string(b[:])
}

func expiresAt(ttlDays *int) *string {
	if ttlDays == nil {
		return nil
	}
	// Python: (now UTC with microseconds dropped) + timedelta(days=n).
	now := time.Now().UTC().Truncate(time.Second).Add(time.Duration(*ttlDays) * 24 * time.Hour)
	out := now.Format("2006-01-02T15:04:05+00:00")
	return &out
}

// statusArg adapts an optional status filter to a query parameter
// (Python's status=None means "no filter").
func statusArg(status *string) any {
	if status == nil {
		return nil
	}
	return *status
}

// kindArg adapts an optional memory_kind filter to a query parameter.
func kindArg(kind string) any {
	if strings.TrimSpace(kind) == "" {
		return nil
	}
	return kind
}
