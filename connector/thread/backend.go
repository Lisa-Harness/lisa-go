// backend.go is a faithful Go port of the read path of
// lisa/sdk/src/sdk/thread/_backend.py: segment-path derivation, sparse index
// parsing, zstd decompression of sealed segments, and the read operations
// loadRecent, loadRange, buildRecentExcerpt, and buildManifest. The write
// path stays in Python runtime.thread; this is the read-only contract shared
// by the MCP server and the host's context-assembly layer.
package thread

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// blockBytes is the block size for streaming reads of active and sealed segments.
const readBlockBytes = 65536

// manifestQueryPreviewChars is the query preview width in manifest rows.
const manifestQueryPreviewChars = 80

// SegmentPaths is every file path derived from a thread store base path.
type SegmentPaths struct {
	Base       string // <base>
	Current    string // <base>.current.jsonl
	IndexPath  string // <base>.index.jsonl
	Digest     string // <base>.digest.md
	Marker     string // <base>.digest.marker
	Dictionary string // <base>.dict.zstd
	Legacy     string // <base>.jsonl (pre-segmentation, for the rename guard)
}

func segmentPaths(base string) SegmentPaths {
	return SegmentPaths{
		Base:       base,
		Current:    base + ".current.jsonl",
		IndexPath:  base + ".index.jsonl",
		Digest:     base + ".digest.md",
		Marker:     base + ".digest.marker",
		Dictionary: base + ".dict.zstd",
		Legacy:     base + ".jsonl",
	}
}

// SegmentRecord is one line of <base>.index.jsonl describing a sealed segment.
type SegmentRecord struct {
	SegID    string `json:"seg_id"`
	TurnLo   int    `json:"turn_lo"`
	TurnHi   int    `json:"turn_hi"`
	TsLo     string `json:"ts_lo"`
	TsHi     string `json:"ts_hi"`
	BytesRaw int    `json:"bytes_raw"`
	BytesZst int    `json:"bytes_zst"`
}

func sealedSegmentPath(base, segID string) string {
	return fmt.Sprintf("%s.%s.jsonl.zst", base, segID)
}

// readIndex parses the sparse segment index. Returns nil when absent/unreadable.
func readIndex(indexPath string) []SegmentRecord {
	data, err := os.ReadFile(indexPath)
	if err != nil {
		return nil
	}
	var records []SegmentRecord
	for _, line := range strings.Split(string(data), "\n") {
		stripped := strings.TrimSpace(line)
		if stripped == "" {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(stripped), &raw); err != nil || raw == nil {
			continue
		}
		segID, ok1 := raw["seg_id"].(string)
		turnLo, ok2 := jsonInt(raw["turn_lo"])
		turnHi, ok3 := jsonInt(raw["turn_hi"])
		tsLo, ok4 := raw["ts_lo"].(string)
		tsHi, ok5 := raw["ts_hi"].(string)
		bytesRaw, ok6 := jsonInt(raw["bytes_raw"])
		bytesZst, ok7 := jsonInt(raw["bytes_zst"])
		if !(ok1 && ok2 && ok3 && ok4 && ok5 && ok6 && ok7) {
			continue
		}
		records = append(records, SegmentRecord{
			SegID: segID, TurnLo: turnLo, TurnHi: turnHi,
			TsLo: tsLo, TsHi: tsHi, BytesRaw: bytesRaw, BytesZst: bytesZst,
		})
	}
	return records
}

// jsonInt converts a JSON-number-decoded value to int (int(raw[...])).
func jsonInt(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		return int(t), true
	case int:
		return t, true
	case int64:
		return int(t), true
	}
	return 0, false
}

// parseEntry parses one JSONL line; nil for malformed or non-dict content
// (mirrors _parse_line: the single seam bounding how many bytes a read parses).
func parseEntry(line []byte) map[string]any {
	var decoded map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(line), &decoded); err != nil {
		return nil
	}
	return decoded
}

// activeLines iterates non-empty lines of the active segment without parsing.
func activeLines(currentPath string) func(yield func([]byte) bool) {
	return func(yield func([]byte) bool) {
		f, err := os.Open(currentPath)
		if err != nil {
			return
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, readBlockBytes), 1024*1024*16)
		for sc.Scan() {
			line := sc.Bytes()
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			if !yield(bytes.Clone(line)) {
				return
			}
		}
	}
}

// reverseActiveLines iterates non-empty active-segment lines newest-first,
// reading blocks from the end (mirrors _reverse_active_lines).
func reverseActiveLines(currentPath string) func(yield func([]byte) bool) {
	return func(yield func([]byte) bool) {
		f, err := os.Open(currentPath)
		if err != nil {
			return
		}
		defer f.Close()
		pos, err := f.Seek(0, io.SeekEnd)
		if err != nil {
			return
		}
		var pending []byte // the first (incomplete) piece of the block below
		for pos > 0 {
			size := int64(readBlockBytes)
			if pos < size {
				size = pos
			}
			pos -= size
			if _, err := f.Seek(pos, io.SeekStart); err != nil {
				return
			}
			block := make([]byte, size)
			if _, err := io.ReadFull(f, block); err != nil {
				return
			}
			pieces := bytes.Split(block, []byte("\n"))
			if len(pieces) == 1 {
				pending = append(block, pending...)
				continue
			}
			line := append(bytes.Clone(pieces[len(pieces)-1]), pending...)
			if len(bytes.TrimSpace(line)) > 0 {
				if !yield(line) {
					return
				}
			}
			for i := len(pieces) - 2; i >= 1; i-- {
				if len(bytes.TrimSpace(pieces[i])) > 0 {
					if !yield(pieces[i]) {
						return
					}
				}
			}
			pending = bytes.Clone(pieces[0])
		}
		if len(bytes.TrimSpace(pending)) > 0 {
			yield(pending)
		}
	}
}

// newestEntries parses newest-first lines until limit valid entries are
// found; returns them oldest-first (mirrors _newest_entries).
func newestEntries(lines func(func([]byte) bool), limit int) []map[string]any {
	var entries []map[string]any
	for line := range lines {
		if entry := parseEntry(line); entry != nil {
			entries = append(entries, entry)
			if len(entries) >= limit {
				break
			}
		}
	}
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	return entries
}

func readActiveEntries(currentPath string) []map[string]any {
	var entries []map[string]any
	for line := range activeLines(currentPath) {
		if entry := parseEntry(line); entry != nil {
			entries = append(entries, entry)
		}
	}
	return entries
}

// ── zstd decompression (read-only — compression stays in the host) ────

func loadZstdDecoder(dictPath string, src io.Reader) (io.ReadCloser, error) {
	var dict []byte
	if data, err := os.ReadFile(dictPath); err == nil {
		dict = data
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	var dec *zstd.Decoder
	var err error
	if dict != nil {
		dec, err = zstd.NewReader(src, zstd.WithDecoderDicts(dict))
	} else {
		dec, err = zstd.NewReader(src)
	}
	if err != nil {
		return nil, err
	}
	return dec.IOReadCloser(), nil
}

// sealedLines streams the non-empty lines of a sealed segment through a
// bounded decoder. A missing or undecodable segment ends the stream after
// the lines already decoded (mirrors _sealed_lines).
func sealedLines(sealedPath, dictPath string, logger *slog.Logger) func(yield func([]byte) bool) {
	return func(yield func([]byte) bool) {
		f, err := os.Open(sealedPath)
		if err != nil {
			return
		}
		defer f.Close()
		rc, err := loadZstdDecoder(dictPath, f)
		if err != nil {
			if logger != nil {
				logger.Warn("thread: failed to decompress sealed segment", "path", sealedPath, "err", err)
			}
			return
		}
		defer rc.Close()
		sc := bufio.NewScanner(rc)
		sc.Buffer(make([]byte, readBlockBytes), 1024*1024*16)
		for sc.Scan() {
			line := sc.Bytes()
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			if !yield(bytes.Clone(line)) {
				return
			}
		}
		if err := sc.Err(); err != nil && logger != nil {
			logger.Warn("thread: failed to decompress sealed segment", "path", sealedPath, "err", err)
		}
	}
}

// ── ThreadStore ────────────────────────────────────────────────────────

// ThreadStore is read-only access to a lossless segmented thread store
// keyed by a base path (thread_dir / <user_slug>).
type ThreadStore struct {
	base      string
	paths     SegmentPaths
	redaction *RedactionPolicy
	logger    *slog.Logger
}

// NewThreadStore builds a read-only ThreadStore over base.
func NewThreadStore(base string, redaction *RedactionPolicy, logger *slog.Logger) *ThreadStore {
	return &ThreadStore{base: base, paths: segmentPaths(base), redaction: redaction, logger: logger}
}

// BasePath is the store base path.
func (s *ThreadStore) BasePath() string { return s.base }

// Paths is the derived segment-path set.
func (s *ThreadStore) Paths() SegmentPaths { return s.paths }

// LoadRecent loads up to limit recent thread entries, oldest-first in the
// tail. limit <= 0 returns every active entry. Returns [] when no store.
func (s *ThreadStore) LoadRecent(limit int) []map[string]any {
	if limit <= 0 {
		return readActiveEntries(s.paths.Current)
	}
	active := newestEntries(reverseActiveLines(s.paths.Current), limit)
	if len(active) >= limit {
		return active
	}
	records := readIndex(s.paths.IndexPath)
	if len(records) == 0 {
		return active
	}
	sealedPath := sealedSegmentPath(s.paths.Base, records[len(records)-1].SegID)
	return append(s.sealedTail(sealedPath, limit-len(active)), active...)
}

// sealedTail returns the newest needed valid entries of one sealed segment,
// oldest-first (mirrors _sealed_tail).
func (s *ThreadStore) sealedTail(sealedPath string, needed int) []map[string]any {
	keep := maxInt(needed*2, 16)
	for {
		window := make([][]byte, 0, keep)
		total := 0
		for line := range sealedLines(sealedPath, s.paths.Dictionary, s.logger) {
			if len(window) == keep {
				copy(window, window[1:])
				window = window[:keep-1]
			}
			window = append(window, line)
			total++
		}
		// newest-first view of the window
		rev := make([][]byte, len(window))
		for i, l := range window {
			rev[len(window)-1-i] = l
		}
		entries := newestEntries(sliceIter(rev), needed)
		if len(entries) >= needed || total <= keep {
			return entries
		}
		keep *= 4
	}
}

func sliceIter(items [][]byte) func(func([]byte) bool) {
	return func(yield func([]byte) bool) {
		for _, item := range items {
			if !yield(item) {
				return
			}
		}
	}
}

// NextTurnIndex returns the 0-based global turn index the next appended
// entry will receive: sealed turn spans plus the active entry count.
func (s *ThreadStore) NextTurnIndex() int {
	records := readIndex(s.paths.IndexPath)
	spans := 0
	for _, r := range records {
		spans += r.TurnHi - r.TurnLo + 1
	}
	return spans + countActiveEntries(s.paths.Current)
}

func countActiveEntries(currentPath string) int {
	count := 0
	for line := range activeLines(currentPath) {
		if parseEntry(line) != nil {
			count++
		}
	}
	return count
}

// LoadRange loads entries whose global turn index falls in [start, end].
func (s *ThreadStore) LoadRange(startTurn, endTurn int) []map[string]any {
	var out []map[string]any
	for _, entry := range s.IterRange(startTurn, endTurn) {
		out = append(out, entry.Entry)
	}
	return out
}

// LoadTurns loads the entries at specific global turns in one pass,
// omitting absent turns.
func (s *ThreadStore) LoadTurns(turns []int) map[int]map[string]any {
	wanted := map[int]bool{}
	lo, hi := 0, 0
	first := true
	for _, t := range turns {
		if t < 0 {
			continue
		}
		wanted[t] = true
		if first || t < lo {
			lo = t
		}
		if first || t > hi {
			hi = t
		}
		first = false
	}
	out := map[int]map[string]any{}
	if len(wanted) == 0 {
		return out
	}
	for _, te := range s.iter(lo, hi, wanted) {
		out[te.Turn] = te.Entry
	}
	return out
}

// TurnEntry is one (turn, entry) pair yielded by IterRange.
type TurnEntry struct {
	Turn  int
	Entry map[string]any
}

// IterRange streams entries with their stable global ordinals in
// [startTurn, endTurn] without building a range list.
func (s *ThreadStore) IterRange(startTurn, endTurn int) []TurnEntry {
	return s.iter(startTurn, endTurn, nil)
}

func (s *ThreadStore) iter(startTurn, endTurn int, wanted map[int]bool) []TurnEntry {
	if startTurn > endTurn {
		return nil
	}
	var out []TurnEntry
	records := readIndex(s.paths.IndexPath)
	stop := false
	for _, record := range records {
		if stop {
			break
		}
		if record.TurnHi < startTurn || record.TurnLo > endTurn {
			continue
		}
		sealedPath := sealedSegmentPath(s.paths.Base, record.SegID)
		turn := record.TurnLo
		for line := range sealedLines(sealedPath, s.paths.Dictionary, s.logger) {
			if turn > endTurn {
				stop = true
				break
			}
			entry := parseEntry(line)
			if entry == nil {
				continue
			}
			if turn >= startTurn && (wanted == nil || wanted[turn]) {
				out = append(out, TurnEntry{turn, entry})
			}
			turn++
		}
	}
	activeLo := 0
	for _, r := range records {
		activeLo += r.TurnHi - r.TurnLo + 1
	}
	turn := activeLo
	for line := range activeLines(s.paths.Current) {
		if turn > endTurn {
			break
		}
		entry := parseEntry(line)
		if entry == nil {
			continue
		}
		if turn >= startTurn && (wanted == nil || wanted[turn]) {
			out = append(out, TurnEntry{turn, entry})
		}
		turn++
	}
	return out
}

// BuildRecentExcerpt builds a short human-readable excerpt of recent thread
// entries. Returns "" when there are no entries. The MCP server passes
// isHostCommand=nil (keep all entries); the host wires the command filter.
func (s *ThreadStore) BuildRecentExcerpt(limit int, isHostCommand func(string) bool) string {
	rawLimit := maxInt(limit, minInt(limit*4, 200))
	entries := s.LoadRecent(rawLimit)
	if len(entries) == 0 {
		return ""
	}
	lines := []string{"Recorded thread context:"}
	emitted := 0
	for i := len(entries) - 1; i >= 0 && emitted < limit; i-- {
		entry := entries[i]
		query := strings.TrimSpace(strField(entry, "query"))
		response := strings.TrimSpace(strField(entry, "response_text"))
		if query == "" && response == "" {
			continue
		}
		if isHostCommand != nil && isHostCommand(query) {
			continue
		}
		if s.redaction != nil {
			query = s.redaction.Redact(query)
			response = s.redaction.Redact(response)
		}
		body := response
		if body == "" {
			body = query
		}
		if prov := getMap(entry, "provenance"); prov != nil {
			// prov.get("source", "observed"): the default applies when the
			// KEY is absent; a present null renders "None" (Python str()).
			source := pyGetStrDefault(prov, "source", "observed")
			model := pyGetStrDefault(prov, "model_id", "unknown")
			body = fmt.Sprintf("%s [system_provenance: %s, model_id: %s]", body, source, model)
		}
		timestamp := pyStr(defaultStr(entry["timestamp_utc"], "unknown"))
		if s.redaction != nil {
			timestamp = s.redaction.Redact(timestamp)
			body = s.redaction.Redact(body)
		}
		shown := query
		if shown == "" {
			shown = "unknown"
		}
		lines = append(lines, fmt.Sprintf("- [recorded at %s] %s: %s", timestamp, shown, body))
		emitted++
	}
	if emitted == 0 {
		return ""
	}
	// Reverse the entry lines back to oldest-first; header and footer anchor.
	rest := lines[1:]
	for i, j := 0, len(rest)-1; i < j; i, j = i+1, j-1 {
		rest[i], rest[j] = rest[j], rest[i]
	}
	footer := "These are recorded past exchanges, not the active request; " +
		"they do not establish current task status."
	return lines[0] + "\n" + strings.Join(rest, "\n") + "\n\n" + footer
}

// BuildManifest builds a compact structured manifest of recent thread
// entries (the lossless "table of contents"). Returns "" when there are no
// entries.
func (s *ThreadStore) BuildManifest(limit int) string {
	entries := s.LoadRecent(limit)
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Thread Manifest (Recorded History)\n\n")
	b.WriteString("| Turn | Recorded At (UTC) | Mode | Query | Writes | Handoff |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- |\n")
	total := len(entries)
	// Python: str(stored_turn) if isinstance(stored_turn, int) — exact
	// int type check, so a float turn value falls to the ordinal form.
	for offset, entry := range entries {
		var turn string
		if stored, ok := entry["turn"]; ok {
			if n, ok2 := jsonInt(stored); ok2 {
				turn = fmt.Sprintf("%d", n)
			}
		}
		if turn == "" {
			turn = fmt.Sprintf("%d/%d", offset+1, total)
		}
		modeID := strField(entry, "mode_id")
		if modeID == "" {
			modeID = "-"
		}
		query := strings.TrimSpace(strField(entry, "query"))
		if s.redaction != nil {
			query = s.redaction.Redact(query)
		}
		timestamp := pyStr(defaultStr(entry["timestamp_utc"], "unknown"))
		if s.redaction != nil {
			timestamp = s.redaction.Redact(timestamp)
		}
		timestamp = strings.ReplaceAll(timestamp, "|", "\\|")
		if utf8Len(query) > manifestQueryPreviewChars {
			query = headChars(query, manifestQueryPreviewChars-3) + "…"
		}
		query = strings.ReplaceAll(query, "|", "\\|")
		var writeCaps []string
		seen := map[string]bool{}
		for _, r := range getList(entry, "effect_receipts") {
			rm, ok := r.(map[string]any)
			if !ok {
				continue
			}
			effect := pyStr(defaultStr(rm["effect"], ""))
			if effect != "write" && effect != "external_write" {
				continue
			}
			cap := pyStr(defaultStr(rm["capability_id"], ""))
			if !seen[cap] {
				seen[cap] = true
				writeCaps = append(writeCaps, cap)
			}
		}
		sortedCaps := make([]string, len(writeCaps))
		copy(sortedCaps, writeCaps)
		sortStrings(sortedCaps)
		writes := strings.Join(sortedCaps, ", ")
		if writes == "" {
			writes = "-"
		}
		handoff := strField(entry, "handoff_to")
		if handoff == "" {
			handoff = strField(entry, "transitioned_to")
		}
		if handoff == "" {
			handoff = "-"
		}
		rowMarker := ""
		if offset == total-1 {
			rowMarker = " ← latest recorded"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s%s |\n",
			turn, timestamp, modeID, query, writes, handoff, rowMarker)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func utf8Len(s string) int { return len([]rune(s)) }

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
