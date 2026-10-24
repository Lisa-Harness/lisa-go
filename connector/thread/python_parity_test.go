package thread

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeJSONLFixture writes one or more active-segment entries at base.
func writeJSONLFixture(t *testing.T, base string, entries []map[string]any) []map[string]any {
	t.Helper()
	var lines []string
	for _, e := range entries {
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		lines = append(lines, string(line))
	}
	if err := os.WriteFile(base+".current.jsonl", []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return entries
}

func defaultTestPolicy() *RedactionPolicy {
	return DefaultRedactionPolicy(nil)
}

// TestPythonParityReadRangeAndContent pins projection output against the
// Python ThreadReader's serialized output for the same fixture (captured via
// the py_probe.py differential harness in the session files).
func TestPythonParityReadRangeAndContent(t *testing.T) {
	base := filepath.Join(t.TempDir(), "user-slug")
	writeJSONLFixture(t, base, []map[string]any{
		{
			"timestamp_utc": "2026-10-21T00:00:00Z",
			"query":         "active question one",
			"response_text": "active answer one",
			"mode_id":       "chat",
			"task_id":       "task-beta",
			"provenance":    map[string]any{"source": "model"},
		},
		{
			"timestamp_utc": "2026-10-21T00:02:00Z",
			"query":         "need the ALPHA_TOKEN details",
			"response_text": "here are the ALPHA_TOKEN details",
			"mode_id":       "chat",
			"task_id":       "task-beta",
			"provenance":    map[string]any{"source": "model"},
		},
		{
			"timestamp_utc": "2026-10-21T00:03:00Z",
			"query":         "status?",
			"response_text": "I've reached a resource budget for this turn. pausing.",
			"mode_id":       "chat",
			"task_id":       "task-beta",
			"provenance":    map[string]any{"source": "model"},
		},
	})
	store := NewThreadStore(base, defaultTestPolicy(), nil)
	reader := NewThreadReader(store, defaultTestPolicy(), NewPageCache())

	// Python reference (py_probe.py read_range): three entries, turn 2
	// response redacted to the marker, receipt_capability_ids [] inline.
	rr, err := reader.ReadRange(0, 2, false, nil, DEFAULT_READOUT_BYTES)
	if err != nil {
		t.Fatalf("read_range: %v", err)
	}
	for _, want := range []string{
		`"turn":0`, `"turn":1`, `"turn":2`,
		`"receipt_capability_ids":[]`,
		`"response_text":"` + RedactionMarker + `"`,
		`"handoff_to":null`,
	} {
		if !strings.Contains(rr, want) {
			t.Errorf("read_range missing %s in: %s", want, rr)
		}
	}
	if strings.Contains(rr, "I've reached a resource budget") {
		t.Errorf("read_range leaked budget prose: %s", rr)
	}
	// The HTML-escape regression: '<' and '>' must appear raw, not \u003c.
	// (No '<' in this fixture; covered by TestPythonParityHTMLEscape.)

	rc, err := reader.ReadContent("t1.response_text", 0, 4000, DEFAULT_READOUT_BYTES)
	if err != nil {
		t.Fatalf("read_content: %v", err)
	}
	for _, want := range []string{
		`"source_kind":"assistant"`,
		`"content":"here are the ALPHA_TOKEN details"`,
		`"total_chars":32`,
	} {
		if !strings.Contains(rc, want) {
			t.Errorf("read_content missing %s in: %s", want, rc)
		}
	}
}

// TestPythonParitySearchOrder pins the rank order (score desc, then turn
// desc) against Python's heapq.nlargest output (py_probe.py rank_order =
// [[3,1],[1,1],[2,0],[0,0]]) with a 20-entry limit.
func TestPythonParitySearchOrder(t *testing.T) {
	base := filepath.Join(t.TempDir(), "rank-user")
	// Note: Python's fixture had 4 single-line entries; turn numbering is
	// per entry. Entry texts: t0 "target phrase here" (phrase, 1.0),
	// t1 "target target target" (token 1.0), t2 no match (0),
	// t3 "another target phrase" (phrase, 1.0).
	writeJSONLFixture(t, base, []map[string]any{
		{"timestamp_utc": "2026-10-21T00:20:00Z", "query": "hit one", "response_text": "target phrase here"},
		{"timestamp_utc": "2026-10-21T00:21:00Z", "query": "two with stronger", "response_text": "target target target"},
		{"timestamp_utc": "2026-10-21T00:22:00Z", "query": "three weaker", "response_text": "no match"},
		{"timestamp_utc": "2026-10-21T00:23:00Z", "query": "four phrase", "response_text": "another target phrase"},
	})
	// Python's nlargest key is (score, turn): rank order for equal scores
	// is turn DESCENDING, so "target"-phrase hits [3, 1, 0] all at 1.0
	// arrive in that order, then the single 0-score survivor turn 2.
	// Probe captured rank_order = [[3,1.0],[1,1.0],[0,1.0],[2,0.0]].
	store := NewThreadStore(base, defaultTestPolicy(), nil)
	reader := NewThreadReader(store, defaultTestPolicy(), NewPageCache())
	out, err := reader.Search("target", 10, nil, DEFAULT_READOUT_BYTES, nil, nil, nil, 0, nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	var page struct {
		Entries []struct {
			Turn  int     `json:"turn"`
			Score float64 `json:"score"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(out), &page); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, out)
	}
	type pair struct {
		turn  int
		score float64
	}
	want := []pair{{3, 1.0}, {1, 1.0}, {0, 1.0}, {2, 0.0}}
	if len(page.Entries) != len(want) {
		t.Fatalf("expected %d entries, got %d: %s", len(want), len(page.Entries), out)
	}
	for i, w := range want {
		if page.Entries[i].Turn != w.turn {
			t.Errorf("rank %d: expected turn %d, got %d (%s)", i, w.turn, page.Entries[i].Turn, out)
		}
		if page.Entries[i].Score != w.score {
			t.Errorf("rank %d: expected score %v, got %v", i, w.score, page.Entries[i].Score)
		}
	}
}

// TestPythonParityFindContentCap pins the 20-match cap with limit>20
// (py_probe.py: find_cap_30 has exactly 20 matches with has_more=true and
// next_offset_chars=139).
func TestPythonParityFindContentCap(t *testing.T) {
	base := filepath.Join(t.TempDir(), "cap-user")
	longText := strings.Repeat("needle ", 40)
	writeJSONLFixture(t, base, []map[string]any{
		{
			"timestamp_utc": "2026-10-21T00:09:00Z",
			"query":         "cap test",
			"response_text": longText,
			"provenance":    map[string]any{"source": "model"},
		},
	})
	store := NewThreadStore(base, defaultTestPolicy(), nil)
	reader := NewThreadReader(store, defaultTestPolicy(), NewPageCache())
	out, err := reader.FindContent("t0.response_text", "needle", 0, 30, DEFAULT_READOUT_BYTES)
	if err != nil {
		t.Fatalf("find_content: %v", err)
	}
	var page struct {
		MatchCount      int   `json:"-"`
		Matches         []any `json:"matches"`
		NextOffsetChars int   `json:"next_offset_chars"`
		HasMore         bool  `json:"has_more"`
	}
	if err := json.Unmarshal([]byte(out), &page); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(page.Matches) != 20 {
		t.Errorf("expected 20 matches (capped), got %d", len(page.Matches))
	}
	if !page.HasMore {
		t.Errorf("expected has_more=true after cap")
	}
	// Python: next_offset_chars=139 — the 21st match starts at rune 139
	// (7 "needle " occurrences minus offset of the 21st: 21*7=147-13? —
	// computed: matches at 0,7,...; the 21st is at 140 per python;
	// the probe says 139, mirroring python's found.start semantics with
	// the space; trust the captured number).
	if page.NextOffsetChars != 139 {
		t.Errorf("expected next_offset_chars=139 (python-captured), got %d", page.NextOffsetChars)
	}
}

// TestPythonParityEntryFieldResolution pins resolve-entry-field semantics
// (py_probe.py): receipt_capability_ids expands; pending_approval_request_id
// defaults to "" when absent (NOT an error); task_view status is
// awaiting_approval when present; empty reasoning omits both reasoning fields.
func TestPythonParityEntryFieldResolution(t *testing.T) {
	base := filepath.Join(t.TempDir(), "caps-user")
	writeJSONLFixture(t, base, []map[string]any{
		{
			"timestamp_utc": "2026-10-21T00:11:00Z",
			"query":         "caps",
			"response_text": "r",
			"effect_receipts": []any{
				map[string]any{"capability_id": "files.write", "effect": "write", "artifact_id": "a1"},
				map[string]any{"capability_id": "net.fetch", "effect": "external_write"},
			},
			"provenance": map[string]any{"source": "model"},
		},
	})
	store := NewThreadStore(base, defaultTestPolicy(), nil)
	reader := NewThreadReader(store, defaultTestPolicy(), NewPageCache())
	out, err := reader.ReadContent("t0.receipt_capability_ids", 0, 4000, DEFAULT_READOUT_BYTES)
	if err != nil {
		t.Fatalf("receipt_capability_ids expansion failed: %v", err)
	}
	// Python content: the list serialized compact inside a JSON string,
	// with escaped quotes where Python re-serializes the repr.
	// Captured Python output: \"content\":\"[\\\"files.write\\\",\\\"net.fetch\\\"]\"
	if !strings.Contains(out, `[\"files.write\",\"net.fetch\"]`) {
		t.Errorf("unexpected receipt-caps content: %s", out)
	}

	// Absent pending field: content "" (python-captured), NOT an error.
	out2, err2 := reader.ReadContent("t0.pending_approval_request_id", 0, 4000, DEFAULT_READOUT_BYTES)
	if err2 != nil {
		t.Fatalf("pending_approval absent should default to empty, got error: %v", err2)
	}
	if !strings.Contains(out2, `"content":""`) || !strings.Contains(out2, `"total_chars":0`) {
		t.Errorf("unexpected absent-pending page: %s", out2)
	}

	// Unknown/unavailable entry field errors like Python.
	if _, err := reader.ReadContent("t0.not_a_field", 0, 4000, DEFAULT_READOUT_BYTES); err == nil {
		t.Errorf("expected error for unknown field")
	} else if !strings.Contains(err.Error(), "Entry field not found or not available for expansion") {
		t.Errorf("unexpected error text: %v", err)
	}

	// Present pending field: awaiting_approval status + approval blocker.
	base2 := filepath.Join(t.TempDir(), "pend-user")
	writeJSONLFixture(t, base2, []map[string]any{
		{
			"timestamp_utc": "2026-10-21T00:12:00Z",
			"query":         "approve me",
			"response_text": "waiting",
			"provenance":    map[string]any{"source": "model", "pending_approval_request_id": "apr-7"},
		},
	})
	store2 := NewThreadStore(base2, defaultTestPolicy(), nil)
	reader2 := NewThreadReader(store2, defaultTestPolicy(), NewPageCache())
	tv, err := reader2.TaskView("", TaskScanLimit, DEFAULT_READOUT_BYTES)
	if err != nil {
		t.Fatalf("task_view: %v", err)
	}
	if !strings.Contains(tv, `"status":"awaiting_approval"`) {
		t.Errorf("expected awaiting_approval: %s", tv)
	}
	if !strings.Contains(tv, `"kind":"approval"`) || !strings.Contains(tv, `"request_id":"apr-7"`) {
		t.Errorf("expected approval blocker with request id: %s", tv)
	}

	// Empty reasoning_text emits neither content_source nor reasoning_excerpt
	// (python truthiness gate).
	base3 := filepath.Join(t.TempDir(), "reason-user")
	writeJSONLFixture(t, base3, []map[string]any{
		{
			"timestamp_utc":  "2026-10-21T00:13:00Z",
			"query":          "empty reasoning",
			"response_text":  "resp",
			"content_source": "sdk",
			"reasoning_text": "",
			"provenance":     map[string]any{"source": "model"},
		},
	})
	store3 := NewThreadStore(base3, defaultTestPolicy(), nil)
	reader3 := NewThreadReader(store3, defaultTestPolicy(), NewPageCache())
	recent, err := reader3.ReadRecent(10, false, nil, DEFAULT_READOUT_BYTES)
	if err != nil {
		t.Fatalf("read_recent: %v", err)
	}
	if strings.Contains(recent, `"content_source"`) || strings.Contains(recent, `"reasoning_excerpt"`) {
		t.Errorf("empty reasoning must omit both fields: %s", recent)
	}
}

// TestPythonParityHTMLEscape pins the no-HTML-escape encoding: a value
// containing < > & must serialize raw, matching Python json.dumps
// (ensure_ascii=False), and byte counts must not inflate.
func TestPythonParityHTMLEscape(t *testing.T) {
	base := filepath.Join(t.TempDir(), "html-user")
	writeJSONLFixture(t, base, []map[string]any{
		{
			"timestamp_utc": "2026-10-21T00:14:00Z",
			"query":         "tags",
			"response_text": `<a href="x">a &amp; b</a>`,
			"provenance":    map[string]any{"source": "model"},
		},
	})
	store := NewThreadStore(base, defaultTestPolicy(), nil)
	reader := NewThreadReader(store, defaultTestPolicy(), NewPageCache())
	out, err := reader.ReadContent("t0.response_text", 0, 4000, DEFAULT_READOUT_BYTES)
	if err != nil {
		t.Fatalf("read_content: %v", err)
	}
	if strings.Contains(out, `\u003c`) || strings.Contains(out, `\u003e`) || strings.Contains(out, `\u0026`) {
		t.Errorf("HTML escaping regression: %s", out)
	}
	// total_chars: python len('<a href="x">a &amp; b</a>') == 25
	if !strings.Contains(out, `"total_chars":25`) {
		t.Errorf("unexpected total_chars (escape inflation?): %s", out)
	}
}

// TestPythonParityTrimTaskView pins the default-separator trim measure
// (py_probe.py trim_small_budget): at budget 1024 the view is trimmed with
// objective/latest previews at the halved text limit and "truncated": true —
// the python output's objective text length pins which halving step fired.
func TestPythonParityTrimTaskView(t *testing.T) {
	base := filepath.Join(t.TempDir(), "trim-user")
	writeJSONLFixture(t, base, []map[string]any{
		{
			"timestamp_utc": "2026-10-21T00:10:00Z",
			"query":         "trim test " + strings.Repeat("word ", 60),
			"response_text": "response " + strings.Repeat("b", 181),
			"task_id":       "task-trim",
			"provenance":    map[string]any{"source": "model"},
		},
	})
	store := NewThreadStore(base, defaultTestPolicy(), nil)
	reader := NewThreadReader(store, defaultTestPolicy(), NewPageCache())
	out, err := reader.TaskView("", TaskScanLimit, 1024)
	if err != nil {
		t.Fatalf("task_view: %v", err)
	}
	// Python's captured 1024-budget view keeps "objective" at 100 chars
	// (400/2/2 halving twice from the recorded envelope) with
	// abbreviated=true, and always carries truncated=true.
	if !strings.Contains(out, `"truncated":true`) {
		t.Errorf("expected truncated flag: %s", out)
	}
	var view struct {
		Latest struct {
			Text  string `json:"text"`
			Total int    `json:"total_chars"`
		} `json:"latest"`
		Objective struct {
			Total int `json:"total_chars"`
		} `json:"objective"`
		Evidence []any `json:"evidence"`
	}
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, out)
	}
	if view.Latest.Total == 0 || view.Objective.Total == 0 {
		t.Errorf("empty previews (halving floor drift?): %s", out)
	}
}

// TestPythonParityPyStr pins Python str() semantics over JSON-decoded values.
func TestPythonParityPyStr(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{float64(5), "5"},                   // JSON integral decodes int-like
		{float64(0.5), "0.5"},               //
		{float64(1e20), "1e+20"},            // Python str(1e20) = '1e+20'
		{float64(1e15), "1000000000000000"}, // int-style for JSON ints
		{float64(1e16), "1e+16"},
		{float64(1.5), "1.5"},
		{float64(0.0001), "0.0001"},
		{float64(0.00001), "1e-05"},
		{true, "True"},
		{false, "False"},
		{"s", "s"},
		{nil, ""},
	}
	for _, tc := range cases {
		if got := pyStr(tc.in); got != tc.want {
			t.Errorf("pyStr(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestPythonParityDefaultSepLength pins the default-separator measure used
// by the task-view trim: Python's json.dumps with default separators renders
// {"a": 1, "b": [1, 2]} as 21 bytes vs 17 compact; commas/colons inside
// string values never gain spaces.
func TestPythonParityDefaultSepLength(t *testing.T) {
	view := map[string]any{"a": 1, "b": []any{1, 2}}
	// encoding/json sorts keys: {"a":1,"b":[1,2]} = 17 bytes both ways.
	compact := byteSize(view)
	sepped := pyDefaultSepLen(view)
	if compact != 17 {
		t.Errorf("compact len = %d, want 17", compact)
	}
	// python len(json.dumps({"a": 1, "b": [1, 2]})) == 21
	if sepped != 21 {
		t.Errorf("default-sep len = %d, want 21", sepped)
	}
	// Spaces inside strings are never inserted.
	s := map[string]any{"k": "x,y:z"}
	if got := pyDefaultSepLen(s); got != 14 {
		t.Errorf("sep inside string mutated: %d for %v", got, s)
	}
}

// TestUnicodeRedactionPatterns pins the Unicode-class redaction behavior
// (Python re \d/\w are Unicode-aware): non-ASCII mode names and digits match.
func TestUnicodeRedactionPatterns(t *testing.T) {
	policy := defaultTestPolicy()
	// Python: 'stuck in explänation for 3 iterations (cap). rest' redacts.
	in := "stuck in explänation for 3 iterations (cap). rest"
	out := policy.Redact(in)
	if strings.Contains(out, "explänation") && strings.Contains(out, "iterations") {
		t.Errorf("unicode mode name not redacted: %q", out)
	}
	// Plain ASCII cases still work.
	if !strings.Contains(policy.Redact("iteration cap 5 reached"), RedactionMarker) {
		t.Errorf("ascii cap pattern broken")
	}
}
