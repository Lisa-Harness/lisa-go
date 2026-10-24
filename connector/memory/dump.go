package memory

import (
	"fmt"
	"strings"
	"time"
)

func timeNowUTC() time.Time { return time.Now().UTC() }

// dumpSnapshot writes a consistent .dump SQL snapshot of dbPath, returning
// the SQL text (parity: sdk/memory/backup.py _dump_snapshot).
//
// The live database is first copied into an in-memory snapshot via the
// SQLite backup API so the dump reflects one point in time even if the
// source store is mid-write. The snapshot is then serialized in the
// sqlite3 .dump CLI format: BEGIN TRANSACTION; schema statements; INSERT
// statements per row; COMMIT; ending with PRAGMA user_version restoration.
// Backups are restorable with `sqlite3 memory.db < dump.sql`.
func dumpSnapshot(dbPath string) ([]byte, error) {
	snapshot, err := openSnapshot(dbPath)
	if err != nil {
		return nil, err
	}
	defer snapshot.close()

	var sb strings.Builder
	sb.WriteString("BEGIN TRANSACTION;\n")
	if err := dumpSchema(snapshot, &sb); err != nil {
		return nil, err
	}
	if err := dumpTables(snapshot, &sb); err != nil {
		return nil, err
	}
	if err := dumpUserVersion(snapshot, &sb); err != nil {
		return nil, err
	}
	sb.WriteString("COMMIT;\n")
	return []byte(sb.String()), nil
}

// dumpSchema emits CREATE statements from sqlite_master for every
// application table and index (excluding sqlite_* internals).
func dumpSchema(snapshot *sqliteSnapshot, sb *strings.Builder) error {
	rows, err := snapshot.query(
		`SELECT type, name, sql FROM sqlite_master
		 WHERE name NOT LIKE 'sqlite_%' AND sql IS NOT NULL
		 ORDER BY CASE type WHEN 'table' THEN 1 WHEN 'index' THEN 2 ELSE 3 END, rowid`)
	if err != nil {
		return fmt.Errorf("read schema: %w", err)
	}
	defer rows.close()
	for rows.next() {
		var kind, name, sqlText string
		if err := rows.scan(&kind, &name, &sqlText); err != nil {
			return fmt.Errorf("scan schema row: %w", err)
		}
		if strings.EqualFold(kind, "view") || strings.EqualFold(kind, "trigger") {
			continue
		}
		sb.WriteString(strings.TrimRight(sqlText, ";") + ";\n")
	}
	return rows.err()
}

// dumpTables emits INSERT statements for every table with rows, in
// sqlite_master order (skip virtual/internal tables).
func dumpTables(snapshot *sqliteSnapshot, sb *strings.Builder) error {
	rows, err := snapshot.query(
		`SELECT name FROM sqlite_master
		 WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
		 ORDER BY rowid`)
	if err != nil {
		return fmt.Errorf("list tables: %w", err)
	}
	var tables []string
	for rows.next() {
		var name string
		if err := rows.scan(&name); err != nil {
			rows.close()
			return fmt.Errorf("scan table name: %w", err)
		}
		tables = append(tables, name)
	}
	if err := rows.err(); err != nil {
		rows.close()
		return fmt.Errorf("iterate tables: %w", err)
	}
	rows.close()

	for _, table := range tables {
		// Virtual tables (would appear from vec0 in the Python world) list
		// their name differently; a defensive skip keeps dumps portable.
		if strings.ContainsAny(table, "()") {
			continue
		}
		if err := dumpTableRows(snapshot, table, sb); err != nil {
			return err
		}
	}
	return nil
}

// dumpTableRows serializes one table's rows as .dump-format INSERTs. Row
// values preserve NULL, numeric, and text affinity; BLOBs render as X'hex'.
func dumpTableRows(snapshot *sqliteSnapshot, table string, sb *strings.Builder) error {
	// Quote identifiers per the sqlite3 shell convention.
	qt := quoteIdent(table)
	rows, err := snapshot.query(fmt.Sprintf("SELECT * FROM %s", qt))
	if err != nil {
		return fmt.Errorf("read rows of %s: %w", table, err)
	}
	defer rows.close()
	cols, err := rows.columns()
	if err != nil {
		return fmt.Errorf("read columns of %s: %w", table, err)
	}
	colList := make([]string, len(cols))
	for i, c := range cols {
		colList[i] = quoteIdent(c)
	}
	prefix := fmt.Sprintf("INSERT INTO %s(%s) VALUES(", qt, strings.Join(colList, ","))
	for rows.next() {
		vals, err := rows.values()
		if err != nil {
			return fmt.Errorf("scan row of %s: %w", table, err)
		}
		sb.WriteString(prefix)
		for i, v := range vals {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(sqlLiteral(v))
		}
		sb.WriteString(");\n")
	}
	if err := rows.err(); err != nil {
		return fmt.Errorf("iterate rows of %s: %w", table, err)
	}
	return nil
}

// sqlLiteral renders one SQLite value as a .dump SQL literal.
func sqlLiteral(v any) string {
	switch t := v.(type) {
	case nil:
		return "NULL"
	case bool:
		if t {
			return "1"
		}
		return "0"
	case []byte:
		var hex strings.Builder
		hex.WriteString("X'")
		for _, b := range t {
			fmt.Fprintf(&hex, "%02x", b)
		}
		hex.WriteString("'")
		return hex.String()
	case string:
		return "'" + strings.ReplaceAll(t, "'", "''") + "'"
	default:
		return fmt.Sprintf("%v", t)
	}
}

// dumpUserVersion restores PRAGMA user_version so a restored dump recreates
// the same schema version behavior (parity: iterdump includes it in newer
// Python; explicit here so the cutover guard sees the right version).
func dumpUserVersion(snapshot *sqliteSnapshot, sb *strings.Builder) error {
	var userVersion int
	if err := snapshot.queryRow("PRAGMA user_version", &userVersion); err != nil {
		return fmt.Errorf("read user_version for dump: %w", err)
	}
	if userVersion != 0 {
		fmt.Fprintf(sb, "PRAGMA user_version = %d;\n", userVersion)
	}
	return nil
}

// quoteIdent quotes a SQLite identifier with double quotes per the shell.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
