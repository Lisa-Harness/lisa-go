package memory

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"modernc.org/sqlite"
)

// sqliteSnapshot is one point-in-time copy of the source database held in a
// temporary file, serialized and removed when closed. The SQLite backup API
// (modernc.org/sqlite's backuper driver interface) copies the live file
// while the store is open — writers are not blocked and the dump reflects
// one moment (parity: source.backup(snapshot) via sqlite3.Connection.backup).
type sqliteSnapshot struct {
	db       *sql.DB
	tempPath string
}

// openSnapshot copies dbPath into a temp-file snapshot via the SQLite
// backup API and returns an open handle to it.
func openSnapshot(dbPath string) (*sqliteSnapshot, error) {
	dir := filepath.Dir(dbPath)
	f, err := os.CreateTemp(dir, "lisa-memory-snapshot-*.db")
	if err != nil {
		return nil, fmt.Errorf("create snapshot temp file: %w", err)
	}
	name := f.Name()
	f.Close()
	if err := os.Remove(name); err != nil {
		return nil, fmt.Errorf("prepare snapshot temp path: %w", err)
	}

	source := fmt.Sprintf("file:%s?mode=ro", filepath.ToSlash(dbPath))
	src, err := sql.Open("sqlite", source)
	if err != nil {
		return nil, fmt.Errorf("open source for snapshot: %w", err)
	}
	src.SetMaxOpenConns(1)
	if err := backupToFile(src, name); err != nil {
		src.Close()
		os.Remove(name)
		return nil, err
	}
	src.Close()

	dst, err := sql.Open("sqlite", name)
	if err != nil {
		os.Remove(name)
		return nil, fmt.Errorf("open snapshot copy: %w", err)
	}
	dst.SetMaxOpenConns(1)
	return &sqliteSnapshot{db: dst, tempPath: name}, nil
}

// backupToFile drives the driver's online-backup capability (modernc.org/
// sqlite's backuper pattern: NewBackup(uri) + Step(-1) + Finish) over one
// raw source connection.
func backupToFile(source *sql.DB, destPath string) error {
	conn, err := source.Conn(context.Background())
	if err != nil {
		return fmt.Errorf("snapshot source conn: %w", err)
	}
	defer conn.Close()
	return conn.Raw(func(driverConn any) error {
		type backuper interface {
			NewBackup(destUri string) (*sqlite.Backup, error)
		}
		bck, err := driverConn.(backuper).NewBackup("file:" + filepath.ToSlash(destPath))
		if err != nil {
			return fmt.Errorf("snapshot backup init: %w", err)
		}
		for more := true; more; {
			more, err = bck.Step(-1)
			if err != nil {
				bck.Finish()
				return fmt.Errorf("snapshot backup step: %w", err)
			}
		}
		return bck.Finish()
	})
}

func (s *sqliteSnapshot) query(query string, args ...any) (*snapshotRows, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	return &snapshotRows{rows: rows}, nil
}

func (s *sqliteSnapshot) queryRow(query string, dest ...any) error {
	return s.db.QueryRow(query, dest...).Scan(dest...)
}

func (s *sqliteSnapshot) close() error {
	if s.db != nil {
		s.db.Close()
	}
	if s.tempPath != "" {
		os.Remove(s.tempPath)
	}
	return nil
}

// snapshotRows adapts database/sql.Rows behind a small interface so the
// dump code stays readable.
type snapshotRows struct {
	rows *sql.Rows
}

func (r *snapshotRows) next() bool             { return r.rows.Next() }
func (r *snapshotRows) scan(dest ...any) error { return r.rows.Scan(dest...) }
func (r *snapshotRows) err() error             { return r.rows.Err() }
func (r *snapshotRows) close() error           { return r.rows.Close() }
func (r *snapshotRows) columns() ([]string, error) {
	return r.rows.Columns()
}

// values reads the current row with SQLite value semantics (int64, float64,
// string, []byte, nil) via database/sql's default conversions.
func (r *snapshotRows) values() ([]any, error) {
	cols, err := r.columns()
	if err != nil {
		return nil, err
	}
	ptrs := make([]any, len(cols))
	for i := range ptrs {
		var v any
		ptrs[i] = &v
	}
	if err := r.rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	out := make([]any, len(ptrs))
	for i, p := range ptrs {
		out[i] = *(p.(*any))
	}
	return out, nil
}
