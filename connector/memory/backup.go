// backup.go is a faithful Go port of lisa/sdk/src/sdk/memory/backup.py:
// point-in-time SQLite dump backups for the memory store.
//
// On memory-server boot, memory.db is dumped via the SQLite backup API (the
// Go driver exposes it as sqlite3.Connection.Backup equivalent through
// modernc.org/sqlite) to a timestamped SQL file under <data_dir>/backups/
// and older dumps are pruned so only the newest keep snapshots remain.
package memory

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// DefaultKeepBackups is the default retention: keep the last 10 dumps.
const DefaultKeepBackups = 10

const dataDirName = "data"

const (
	backupPrefix          = "memory"
	backupSuffix          = ".sql"
	timestampTmpl         = "20060102T150405"
	backupTimestampLayout = timestampTmpl
)

var backupNameRE = regexp.MustCompile(
	`^` + backupPrefix + `-\d{8}T\d{6}(-\d+)?` + backupSuffix + `$`)

// DefaultBackupDir returns the default backup directory for a memory data
// dir (parity: sdk.layout.DATA_DIR / backups). The Python server resolves
// the DB itself as <data_dir>/data/memory.db; the Go server passes the same
// layout in.
func DefaultBackupDir(dataDir string) string {
	return filepath.Join(dataDir, dataDirName, "backups")
}

// BackupMemoryDB dumps memory.db to a timestamped SQL backup and prunes old
// dumps. Writes one memory-<UTC timestamp>.sql file under backupDir (default
// <data_dir>/data/backups/) and deletes older dumps beyond the keep limit.
// Returns the new backup path, or "" when the source database does not exist
// yet (first boot — nothing to back up). keep is clamped to at least 1 so
// the fresh dump always survives.
//
// The dump uses the sqlite3 %equiv .dump serialization — same text format as
// the sqlite3 shell — so backups are restorable with
// `sqlite3 memory.db < dump.sql`.
func BackupMemoryDB(dataDir string, opts BackupOptions) (string, error) {
	sourcePath := opts.DBPath
	if sourcePath == "" {
		sourcePath = filepath.Join(dataDir, dataDirName, "memory.db")
	}
	if _, err := os.Stat(sourcePath); err != nil {
		if os.IsNotExist(err) {
			slog.Info("memory backup skipped; no database", "path", sourcePath)
			return "", nil
		}
		return "", fmt.Errorf("stat memory db: %w", err)
	}
	keep := opts.Keep
	if keep < 1 {
		keep = 1
	}
	backupDir := opts.BackupDir
	if backupDir == "" {
		backupDir = DefaultBackupDir(dataDir)
	}
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return "", fmt.Errorf("create backup dir: %w", err)
	}
	target, err := uniqueBackupPath(backupDir, timestampUTC())
	if err != nil {
		return "", err
	}
	dump, err := dumpSnapshot(sourcePath)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(target, dump, 0o600); err != nil {
		return "", fmt.Errorf("write backup: %w", err)
	}
	removed, err := pruneBackups(backupDir, keep)
	if err != nil {
		return target, err
	}
	if len(removed) > 0 {
		slog.Info("memory backup wrote; pruned old backups",
			"target", target, "pruned", len(removed), "keep", keep)
	} else {
		slog.Info("memory backup wrote", "target", target, "keep", keep)
	}
	return target, nil
}

// BackupOptions carries the optional backup parameters.
type BackupOptions struct {
	DBPath    string // default <dataDir>/data/memory.db
	BackupDir string // default <dataDir>/data/backups/
	Keep      int    // default DefaultKeepBackups
}

func timestampUTC() string {
	// Python: datetime.now(UTC).strftime("%Y%m%dT%H%M%S") — already UTC.
	return timestampTmpl[:0] + formatNowUTC()
}

func formatNowUTC() string {
	now := timeNowUTC()
	yy, mm, dd := now.Date()
	hh, mi, ss := now.Clock()
	return fmt.Sprintf("%04d%02d%02dT%02d%02d%02d", yy, int(mm), dd, hh, mi, ss)
}

// uniqueBackupPath returns a collision-free backup path for the given
// timestamp (parity: _unique_backup_path).
func uniqueBackupPath(backupDir, timestamp string) (string, error) {
	candidate := filepath.Join(backupDir, backupPrefix+"-"+timestamp+backupSuffix)
	sequence := 1
	for {
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate, nil
		} else if err != nil {
			return "", fmt.Errorf("stat backup candidate: %w", err)
		}
		candidate = filepath.Join(backupDir,
			fmt.Sprintf("%s-%s-%d%s", backupPrefix, timestamp, sequence, backupSuffix))
		sequence++
	}
}

// listBackups returns existing backup dumps, oldest first. Ordered by
// modification time (name as a stable tiebreak) rather than filename, so
// same-second dumps with collision suffixes still prune oldest-first —
// lexicographic name order breaks once a suffix reaches two digits (-19
// sorts before -2).
func listBackups(backupDir string) ([]string, error) {
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list backups: %w", err)
	}
	type dump struct {
		path    string
		modNano int64
		name    string
	}
	var dumps []dump
	for _, e := range entries {
		if e.IsDir() || !backupNameRE.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, fmt.Errorf("stat backup %s: %w", e.Name(), err)
		}
		dumps = append(dumps, dump{
			path:    filepath.Join(backupDir, e.Name()),
			modNano: info.ModTime().UnixNano(),
			name:    e.Name(),
		})
	}
	sort.Slice(dumps, func(i, j int) bool {
		if dumps[i].modNano != dumps[j].modNano {
			return dumps[i].modNano < dumps[j].modNano
		}
		return dumps[i].name < dumps[j].name
	})
	paths := make([]string, 0, len(dumps))
	for _, d := range dumps {
		paths = append(paths, d.path)
	}
	return paths, nil
}

// pruneBackups deletes all but the newest keep backups; returns removed paths.
func pruneBackups(backupDir string, keep int) ([]string, error) {
	backups, err := listBackups(backupDir)
	if err != nil {
		return nil, err
	}
	excess := len(backups) - keep
	if excess <= 0 {
		return nil, nil
	}
	var removed []string
	for _, path := range backups[:excess] {
		if err := os.Remove(path); err != nil {
			return removed, fmt.Errorf("prune backup %s: %w", path, err)
		}
		removed = append(removed, path)
	}
	return removed, nil
}

// trimBackupName helps tests assert the fixture naming convention.
func trimBackupName(name string) string {
	return strings.TrimSuffix(strings.TrimPrefix(name, backupPrefix+"-"), backupSuffix)
}
