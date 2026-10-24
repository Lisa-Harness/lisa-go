// readout.go is a faithful Go port of lisa/sdk/src/sdk/thread/_readout.py:
// bounded, restorable model-facing views of the lossless thread archive,
// plus the deterministic search of _search.py and the resolved-ref window
// reads of _results.py (read_result_window / load_result_object). The
// raw-vs-projected split is a security invariant: load_range / load_recent
// return raw entries; every projection here is redacted via the policy.
package thread

import (
	"container/list"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// DEFAULT_READOUT_BYTES mirrors _readout.DEFAULT_READOUT_BYTES.
const DEFAULT_READOUT_BYTES = 16384

// MAX_READOUT_BYTES mirrors _readout.MAX_READOUT_BYTES.
const MAX_READOUT_BYTES = 32768

const (
	taskScanLimit      = 200
	inlineBytes        = 600
	headPreviewChars   = 120
	tailPreviewChars   = 60
	cursorReserveBytes = 256
	reasoningChars     = 800
	maxSearchResults   = 20
	maxRecentEntries   = 50
	maxReferenceChars  = 128
)

// rangeCursorPattern mirrors _readout._RANGE_CURSOR; searchCursorPattern
// mirrors _SEARCH_CURSOR; referencePattern mirrors _REFERENCE.
var (
	rangeCursorPattern  = regexp.MustCompile(`^(range|recent)\.(\d{1,20})\.(\d{1,20})\.(\d{1,20})\.(-?\d{1,20})$`)
	searchCursorPattern = regexp.MustCompile(`^search\.(-?\d{1,20})\.(\d{1,20})\.([a-f0-9]{16})$`)
	referencePattern    = regexp.MustCompile(`^t(\d{1,20})(?:\.m(\d{1,20}))?(?:\.c(\d{1,20}))?(?:\.([a-z_]+))?$`)
	// resultReferencePattern mirrors _results._RESULT_REFERENCE.
	resultReferencePattern = regexp.MustCompile(`^r[a-f0-9]{64}$`)
)

// entryFields mirrors _readout._ENTRY_FIELDS.
var entryFields = []string{"mode_id", "query", "response_text", "handoff_to", "transitioned_to"}

// messageFields mirrors _readout._MESSAGE_FIELDS.
var messageFields = []string{"role", "name", "tool_call_id", "id", "content", "tool_calls", "is_error"}

func storedResultSource() map[string]any {
	return map[string]any{
		"scope":          "stored_tool_result",
		"source_kind":    "tool",
		"timestamp_utc":  nil,
		"interpretation": "Stored tool output; its time and relationship to the active request are unknown.",
	}
}

func readoutBudget(requested int) (int, error) {
	if requested <= 0 {
		return 0, errors.New("budget_bytes must be positive")
	}
	return maxInt(DEFAULT_READOUT_BYTES, minInt(requested, MAX_READOUT_BYTES)), nil
}

func newPage() map[string]any {
	return map[string]any{
		"scope":          "recorded_history",
		"interpretation": recordedHistoryLabel,
		"entries":        []any{},
		"next_cursor":    nil,
		"has_more":       false,
	}
}

func finishPage(page map[string]any, cursor any) string {
	page["next_cursor"] = cursor
	page["has_more"] = cursor != nil
	return encode(page)
}

func pageEntries(page map[string]any) []any { return page["entries"].([]any) }

func fits(page map[string]any, budget int) bool {
	return byteSize(page)+cursorReserveBytes <= budget
}

// ── Search primitives (_search.py) ────────────────────────────────────────

const (
	excerptChars = 240
	contextChars = 80
	maxMatches   = 3
)

// SearchFilters mirrors _search.SearchFilters.
type SearchFilters struct {
	ToolName string
	Path     string
	TaskID   string
}

func (f SearchFilters) acceptsTool(name string, arguments any) bool {
	if f.ToolName != "" && name != f.ToolName {
		if idx := strings.LastIndex(name, "."); idx < 0 || name[idx+1:] != f.ToolName {
			return false
		}
	}
	if f.Path == "" {
		return true
	}
	parsed := arguments
	if s, ok := arguments.(string); ok {
		var decoded any
		if err := json.Unmarshal([]byte(s), &decoded); err != nil {
			return false
		}
		parsed = decoded
	}
	for value := range allStrings(parsed) {
		if strings.Contains(value, f.Path) {
			return true
		}
	}
	return false
}

func allStrings(value any) func(func(string) bool) {
	return func(yield func(string) bool) {
		var walk func(v any) bool
		walk = func(v any) bool {
			switch t := v.(type) {
			case string:
				return yield(t)
			case map[string]any:
				for _, item := range t {
					if !walk(item) {
						return false
					}
				}
			case []any:
				for _, item := range t {
					if !walk(item) {
						return false
					}
				}
			}
			return true
		}
		walk(value)
	}
}

// entryTaskID mirrors _search.entry_task_id.
func entryTaskID(entry map[string]any) string {
	if s := strField(entry, "task_id"); s != "" {
		return s
	}
	return pyStr(defaultStr(getMap(entry, "provenance")["task_id"], ""))
}

// RedactValue mirrors _search.redact_value: recursive read-time redaction.
func RedactValue(value any, redaction *RedactionPolicy) any {
	switch t := value.(type) {
	case string:
		if redaction != nil {
			return redaction.Redact(t)
		}
		return t
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = RedactValue(item, redaction)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for key, item := range t {
			out[key] = RedactValue(item, redaction)
		}
		return out
	}
	return value
}

func permittedText(value any, redaction *RedactionPolicy) string {
	permitted := RedactValue(value, redaction)
	if s, ok := permitted.(string); ok {
		return s
	}
	return encode(permitted)
}

type searchMatch struct {
	Ref              string  `json:"ref"`
	ToolName         string  `json:"tool_name"`
	OffsetChars      int     `json:"offset_chars"`
	MatchOffsetChars int     `json:"match_offset_chars"`
	Excerpt          string  `json:"excerpt"`
	Score            float64 `json:"score"`
}

type scoredEntry struct {
	Turn    int
	Score   float64
	Matches []searchMatch
}

// regexpFindCaseInsensitive mirrors re.search(re.escape(term), text, re.I)
// over rune offsets.
func regexpFindCaseInsensitive(text, term string) (start int, ok bool) {
	idx := strings.Index(strings.ToLower(text), strings.ToLower(term))
	if idx < 0 {
		return 0, false
	}
	return utf8.RuneCountInString(text[:idx]), true
}

func matchValue(text, query, ref, tool string) *searchMatch {
	if text == "" {
		return nil
	}
	seen := map[string]bool{}
	var terms []string
	for _, term := range strings.Fields(query) {
		if !seen[term] {
			seen[term] = true
			terms = append(terms, term)
		}
	}
	var hits []int
	for _, term := range terms {
		if start, ok := regexpFindCaseInsensitive(text, term); ok {
			hits = append(hits, start)
		}
	}
	if len(terms) > 0 && len(hits) == 0 {
		return nil
	}
	phrasePos, phraseOK := -1, false
	if query != "" {
		phrasePos, phraseOK = regexpFindCaseInsensitive(text, query)
	}
	position := 0
	switch {
	case phraseOK:
		position = phrasePos
	case len(hits) > 0:
		position = hits[0]
	}
	offset := maxInt(0, position-contextChars)
	score := 0.0
	if phraseOK || len(terms) == 0 {
		score = 1.0
	} else {
		score = float64(len(hits)) / float64(len(terms))
	}
	return &searchMatch{
		Ref:              ref,
		ToolName:         tool,
		OffsetChars:      offset,
		MatchOffsetChars: position,
		Excerpt:          sliceChars(text, offset, offset+excerptChars),
		Score:            score,
	}
}

type toolCall struct {
	Index int
	ID    string
	Name  string
	Args  any
}

func toolCallsOf(message map[string]any) []toolCall {
	var out []toolCall
	for i, raw := range getList(message, "tool_calls") {
		call, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := call["function"].(map[string]any)
		if !ok {
			fn = call
		}
		name, _ := fn["name"].(string)
		out = append(out, toolCall{
			Index: i,
			ID:    pyStr(defaultStr(call["id"], "")),
			Name:  name,
			Args:  fn["arguments"],
		})
	}
	return out
}

type searchValue struct {
	Ref   string
	Value any
	Tool  string
}

func searchValues(entry map[string]any, turn int, filters SearchFilters) []searchValue {
	var out []searchValue
	if filters.ToolName == "" && filters.Path == "" {
		for _, field := range []string{"query", "response_text", "mode_id"} {
			out = append(out, searchValue{fmt.Sprintf("t%d.%s", turn, field), defaultStr(entry[field], ""), ""})
		}
	}
	calls := map[string]toolCall{}
	for index, rawMessage := range getList(entry, "transcript") {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			continue
		}
		for _, call := range toolCallsOf(message) {
			calls[call.ID] = call
			if filters.acceptsTool(call.Name, call.Args) {
				out = append(out, searchValue{fmt.Sprintf("t%d.m%d.c%d.arguments", turn, index, call.Index), call.Args, call.Name})
				out = append(out, searchValue{fmt.Sprintf("t%d.m%d.c%d.name", turn, index, call.Index), call.Name, call.Name})
			}
		}
		if message["role"] == "tool" {
			name := ""
			var args any
			if call, ok := calls[pyStr(defaultStr(message["tool_call_id"], ""))]; ok {
				name, args = call.Name, call.Args
			}
			name = pyStr(defaultStr(message["name"], name))
			if filters.acceptsTool(name, args) {
				out = append(out, searchValue{fmt.Sprintf("t%d.m%d.content", turn, index), defaultStr(message["content"], ""), name})
			}
		}
	}
	return out
}

func scoreEntry(turn int, entry map[string]any, query string, filters SearchFilters, redaction *RedactionPolicy) *scoredEntry {
	if filters.TaskID != "" && entryTaskID(entry) != filters.TaskID {
		return nil
	}
	var matches []searchMatch
	for _, value := range searchValues(entry, turn, filters) {
		tool, _ := RedactValue(value.Tool, redaction).(string)
		if m := matchValue(permittedText(value.Value, redaction), query, value.Ref, tool); m != nil {
			matches = append(matches, *m)
		}
	}
	if len(matches) == 0 && (filters.ToolName != "" || filters.Path != "" || filters.TaskID != "") {
		return nil
	}
	if len(matches) == 0 && strField(entry, "query") == "" && strField(entry, "response_text") == "" {
		return nil
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].Score > matches[j].Score })
	score := 0.0
	if len(matches) > 0 {
		score = matches[0].Score
	}
	if len(matches) > maxMatches {
		matches = matches[:maxMatches]
	}
	if matches == nil {
		matches = []searchMatch{}
	}
	return &scoredEntry{Turn: turn, Score: score, Matches: matches}
}

// searchEntries mirrors heapq.nlargest(limit, candidates, key=(score, turn)).
func searchEntries(entries []TurnEntry, query string, limit int, filters SearchFilters, redaction *RedactionPolicy) []scoredEntry {
	var candidates []scoredEntry
	for _, te := range entries {
		if scored := scoreEntry(te.Turn, te.Entry, query, filters, redaction); scored != nil {
			candidates = append(candidates, *scored)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Score != candidates[j].Score {
			return candidates[i].Score > candidates[j].Score
		}
		return candidates[i].Turn > candidates[j].Turn
	})
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	// nlargest output order is by rank; restore chronological page order
	// (cursor slicing then matches the Python server).
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Turn < candidates[j].Turn })
	return candidates
}

// ── Hash-ref result objects (_results.py) ─────────────────────────────────

// resultObjectPath mirrors _results.result_object_path (with the symlink
// guards; it is also the read-side validation seam for r<sha> refs).
func (s *ThreadStore) resultObjectPath(ref string) (string, error) {
	if !resultReferencePattern.MatchString(ref) {
		return "", errors.New("Invalid result-object reference")
	}
	dir := s.base + ".results"
	if info, err := os.Lstat(dir); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("Result-object reference not found or unreadable")
	}
	path := filepath.Join(dir, ref+".json")
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("Result-object reference not found or unreadable")
	}
	// filepath.Clean keeps the join inside the directory; belt-and-braces
	// against a cleaned path escaping (should be impossible given the regex).
	if !strings.HasPrefix(filepath.Clean(path), filepath.Clean(dir)+string(os.PathSeparator)) {
		return "", errors.New("Result-object reference not found or unreadable")
	}
	return path, nil
}

// LoadResultObject mirrors _results.load_result_object: stream the payload
// once while hashing, verify the sha256 digest, then decode.
func (s *ThreadStore) LoadResultObject(ref string) (map[string]any, error) {
	payload, err := s.loadVerifiedResultPayload(ref)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(payload, &result); err != nil {
		return nil, errors.New("Result-object reference not found or unreadable")
	}
	content, ok := result["content"]
	if !ok {
		return map[string]any{"content": result}, nil
	}
	if _, isStr := content.(string); !isStr {
		return nil, errors.New("Invalid result-object content")
	}
	return result, nil
}

func (s *ThreadStore) loadVerifiedResultPayload(ref string) ([]byte, error) {
	path, err := s.resultObjectPath(ref)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("Result-object reference not found or unreadable")
	}
	defer f.Close()
	h := sha256.New()
	payload, err := io.ReadAll(io.TeeReader(f, h))
	if err != nil {
		return nil, errors.New("Result-object reference not found or unreadable")
	}
	if fmt.Sprintf("%x", h.Sum(nil)) != ref[1:] {
		return nil, errors.New("Result-object integrity check failed")
	}
	return payload, nil
}

// readResultWindow mirrors _results.read_result_window: decode the object
// (digest-verified) and return one character window over the content.
func (s *ThreadStore) readResultWindow(ref string, offsetChars, limitChars int) (string, int, error) {
	result, err := s.LoadResultObject(ref)
	if err != nil {
		return "", 0, err
	}
	content, ok := result["content"].(string)
	if !ok {
		content = encode(result["content"])
	}
	return sliceChars(content, offsetChars, offsetChars+limitChars), utf8.RuneCountInString(content), nil
}

// ── PageCache (_readout.py) ────────────────────────────────────────────────

type pageCacheKey struct {
	Ref    string
	Offset int
	Limit  int
	Tag    string
}

// PageCache is a bounded LRU for pages of immutable thread refs (t<turn>
// refs are append-only history; r<hash> refs are content-addressed).
type PageCache struct {
	maxPages int
	maxBytes int
	elements map[pageCacheKey]*list.Element
	lru      *list.List
	bytes    int
}

type pageCacheEntry struct {
	key pageCacheKey
	val map[string]any
}

// NewPageCache mirrors PageCache(max_pages=256, max_bytes=524_288).
func NewPageCache() *PageCache {
	return &PageCache{
		maxPages: maxInt(1, 256),
		maxBytes: maxInt(1, 524288),
		elements: map[pageCacheKey]*list.Element{},
		lru:      list.New(),
	}
}

func (c *PageCache) Get(key pageCacheKey) (map[string]any, bool) {
	el, ok := c.elements[key]
	if !ok {
		return nil, false
	}
	c.lru.MoveToBack(el)
	return el.Value.(*pageCacheEntry).val, true
}

func (c *PageCache) Put(key pageCacheKey, page map[string]any) {
	size := byteSize(page)
	if size > c.maxBytes {
		return
	}
	if el, ok := c.elements[key]; ok {
		c.lru.Remove(el)
		c.bytes -= byteSize(el.Value.(*pageCacheEntry).val)
	}
	for c.lru.Len() > 0 && (c.lru.Len() >= c.maxPages || c.bytes+size > c.maxBytes) {
		evicted := c.lru.Remove(c.lru.Front()).(*pageCacheEntry)
		delete(c.elements, evicted.key)
		c.bytes -= byteSize(evicted.val)
	}
	el := c.lru.PushBack(&pageCacheEntry{key: key, val: page})
	c.elements[key] = el
	c.bytes += size
}

// ── ThreadReader (_readout.py) ─────────────────────────────────────────────

// ThreadReader projects archive content without mutating it or exposing raw
// reasoning fields.
type ThreadReader struct {
	Store     *ThreadStore
	Redaction *RedactionPolicy
	Pages     *PageCache
}

// NewThreadReader mirrors ThreadReader(store, redaction, page_cache=...). A
// nil Pages argument builds the shared bounded page cache.
func NewThreadReader(store *ThreadStore, redaction *RedactionPolicy, pages *PageCache) *ThreadReader {
	if pages == nil {
		pages = NewPageCache()
	}
	return &ThreadReader{Store: store, Redaction: redaction, Pages: pages}
}

func (r *ThreadReader) redact(value any) any { return RedactValue(value, r.Redaction) }

func (r *ThreadReader) preview(value any, ref string) any {
	permitted := r.redact(value)
	if byteSize(permitted) <= inlineBytes {
		return permitted
	}
	text, ok := permitted.(string)
	if !ok {
		text = encode(permitted)
	}
	total := utf8.RuneCountInString(text)
	head := minInt(headPreviewChars, total)
	tail := minInt(tailPreviewChars, total-head)
	tailText := ""
	if tail > 0 {
		tailText = tailChars(text, tail)
	}
	return map[string]any{
		"ref":           ref,
		"total_chars":   total,
		"abbreviated":   true,
		"head":          headChars(text, head),
		"tail":          tailText,
		"omitted_chars": total - head - tail,
	}
}

func (r *ThreadReader) entry(entry map[string]any, turn int) map[string]any {
	result := map[string]any{
		"turn":             turn,
		"transcript_count": len(getList(entry, "transcript")),
	}
	result["timestamp_utc"] = r.preview(entry["timestamp_utc"], fmt.Sprintf("t%d.timestamp_utc", turn))
	result["provenance_source"] = r.preview(getMap(entry, "provenance")["source"], fmt.Sprintf("t%d.provenance_source", turn))
	for _, field := range entryFields {
		result[field] = r.preview(entry[field], fmt.Sprintf("t%d.%s", turn, field))
	}
	var caps []any
	for _, receipt := range getList(entry, "effect_receipts") {
		if m, ok := receipt.(map[string]any); ok {
			caps = append(caps, pyStr(defaultStr(m["capability_id"], "")))
		}
	}
	if caps == nil {
		caps = []any{}
	}
	result["task_id"] = r.preview(entryTaskID(entry), fmt.Sprintf("t%d.task_id", turn))
	result["receipt_capability_ids"] = r.preview(caps, fmt.Sprintf("t%d.receipt_capability_ids", turn))
	source := strField(entry, "content_source")
	reasoning, hasReasoning := entry["reasoning_text"]
	if source != "" && hasReasoning && reasoning != nil {
		result["content_source"] = r.preview(source, fmt.Sprintf("t%d.content_source", turn))
		reasoningText, _ := r.redact(pyStr(reasoning)).(string)
		result["reasoning_excerpt"] = tailChars(reasoningText, reasoningChars)
		result["reasoning_omitted_chars"] = maxInt(0, utf8.RuneCountInString(reasoningText)-reasoningChars)
	}
	return result
}

func (r *ThreadReader) message(message any, turn, index int) map[string]any {
	ref := fmt.Sprintf("t%d.m%d", turn, index)
	result := map[string]any{"ref": ref}
	m, ok := message.(map[string]any)
	if !ok {
		result["content"] = r.preview(message, ref)
		return result
	}
	for _, field := range messageFields {
		if v, present := m[field]; present {
			result[field] = r.preview(v, fmt.Sprintf("%s.%s", ref, field))
		}
	}
	return result
}

func parseRangeCursor(cursor, kind string) (start, end, turn, position int, err error) {
	m := rangeCursorPattern.FindStringSubmatch(cursor)
	if m == nil || m[1] != kind {
		return 0, 0, 0, 0, errors.New("Invalid thread cursor")
	}
	start, _ = strconv.Atoi(m[2])
	end, _ = strconv.Atoi(m[3])
	turn, _ = strconv.Atoi(m[4])
	position, _ = strconv.Atoi(m[5])
	if !(start <= turn && turn <= end) || position < -1 {
		return 0, 0, 0, 0, errors.New("Invalid thread cursor position")
	}
	return start, end, turn, position, nil
}

// ReadRange mirrors _readout.ThreadReader.read_range.
func (r *ThreadReader) ReadRange(startTurn, endTurn int, includeTranscript bool, cursor *string, budgetBytes int) (string, error) {
	if startTurn < 0 || endTurn < startTurn {
		return "", errors.New("Require 0 <= start_turn <= end_turn")
	}
	bounds := [2]int{startTurn, minInt(endTurn, r.Store.NextTurnIndex()-1)}
	if cursor != nil {
		start, end, _, _, err := parseRangeCursor(*cursor, "range")
		if err != nil {
			return "", err
		}
		if start != startTurn || end > endTurn {
			return "", errors.New("Cursor does not belong to this range")
		}
		bounds = [2]int{start, end}
	}
	budget, err := readoutBudget(budgetBytes)
	if err != nil {
		return "", err
	}
	return r.readWindow(bounds, "range", includeTranscript, cursor, budget)
}

// ReadRecent mirrors _readout.ThreadReader.read_recent.
func (r *ThreadReader) ReadRecent(limit int, includeTranscript bool, cursor *string, budgetBytes int) (string, error) {
	capped := maxInt(1, minInt(limit, maxRecentEntries))
	end := r.Store.NextTurnIndex() - 1
	bounds := [2]int{maxInt(0, end-capped+1), end}
	if cursor != nil {
		startB, endB, _, _, err := parseRangeCursor(*cursor, "recent")
		if err != nil {
			return "", err
		}
		bounds = [2]int{startB, endB}
	}
	budget, err := readoutBudget(budgetBytes)
	if err != nil {
		return "", err
	}
	return r.readWindow(bounds, "recent", includeTranscript, cursor, budget)
}

func (r *ThreadReader) readWindow(bounds [2]int, kind string, includeTranscript bool, cursor *string, budget int) (string, error) {
	start, end := bounds[0], bounds[1]
	turn, position := start, -1
	if cursor != nil {
		_, _, parsedTurn, parsedPosition, err := parseRangeCursor(*cursor, kind)
		if err != nil {
			return "", err
		}
		turn, position = parsedTurn, parsedPosition
	}
	if position >= 0 && !includeTranscript {
		return "", errors.New("Transcript cursor requires include_transcript=true")
	}
	page := newPage()
	for _, te := range r.Store.IterRange(turn, end) {
		ordinal := te.Turn
		var projected map[string]any
		if position == -1 {
			projected = r.entry(te.Entry, ordinal)
		} else {
			projected = map[string]any{"turn": ordinal, "transcript_start": position}
		}
		var transcript *[]any
		if includeTranscript {
			projected["transcript_start"] = maxInt(0, position)
			projected["transcript_count"] = len(getList(te.Entry, "transcript"))
			msgs := []any{}
			projected["transcript"] = msgs
			transcript = &msgs
		}
		page["entries"] = append(pageEntries(page), projected)
		if !fits(page, budget) {
			entries := pageEntries(page)
			page["entries"] = entries[:len(entries)-1]
			return finishPage(page, fmt.Sprintf("%s.%d.%d.%d.%d", kind, start, end, ordinal, position)), nil
		}
		if includeTranscript {
			next, err := r.appendMessages(page, *transcript, te, ordinal, position, budget)
			if err != nil {
				return "", err
			}
			if next >= 0 {
				return finishPage(page, fmt.Sprintf("%s.%d.%d.%d.%d", kind, start, end, ordinal, next)), nil
			}
		}
		position = -1
	}
	return finishPage(page, nil), nil
}

// appendMessages returns the next transcript position, or -1 when complete.
func (r *ThreadReader) appendMessages(page map[string]any, transcript []any, te TurnEntry, turn, position, budget int) (int, error) {
	msgs := getList(te.Entry, "transcript")
	if position > len(msgs) {
		return -1, errors.New("Cursor exceeds transcript length")
	}
	entries := pageEntries(page)
	projected := entries[len(entries)-1].(map[string]any)
	out := transcript
	for index := maxInt(0, position); index < len(msgs); index++ {
		out = append(out, r.message(msgs[index], turn, index))
		projected["transcript"] = out
		if !fits(page, budget) {
			out = out[:len(out)-1]
			projected["transcript"] = out
			return index, nil
		}
	}
	projected["transcript"] = out
	return -1, nil
}

// Search mirrors _readout.ThreadReader.search.
func (r *ThreadReader) Search(query string, limit int, cursor *string, budgetBytes int, toolName, path, taskID *string, startTurn int, endTurn *int) (string, error) {
	budget, err := readoutBudget(budgetBytes)
	if err != nil {
		return "", err
	}
	capped := maxInt(1, minInt(limit, maxSearchResults))
	if startTurn < 0 || (endTurn != nil && *endTurn < startTurn) {
		return "", errors.New("Invalid search turn range")
	}
	inputs := []any{query, capped, toolName, path, taskID, startTurn, endTurn}
	sigBytes, _ := json.Marshal(inputs)
	signature := fmt.Sprintf("%x", sha256.Sum256(append([]byte{}, sigBytes...)))[:16]
	end := r.Store.NextTurnIndex() - 1
	if endTurn != nil {
		end = minInt(end, *endTurn)
	}
	position := 0
	if cursor != nil {
		m := searchCursorPattern.FindStringSubmatch(*cursor)
		if m == nil || m[3] != signature {
			return "", errors.New("Cursor does not belong to this search")
		}
		end, _ = strconv.Atoi(m[1])
		position, _ = strconv.Atoi(m[2])
	}
	filters := SearchFilters{}
	if toolName != nil {
		filters.ToolName = *toolName
	}
	if path != nil {
		filters.Path = *path
	}
	if taskID != nil {
		filters.TaskID = *taskID
	}
	matches := searchEntries(r.Store.IterRange(startTurn, end), query, capped, filters, r.Redaction)
	if position > len(matches) {
		return "", errors.New("Cursor exceeds search results")
	}
	turns := []int{}
	rawByTurn := map[int]map[string]any{}
	for _, m := range matches[position:] {
		turns = append(turns, m.Turn)
	}
	for turn, entry := range r.Store.LoadTurns(turns) {
		rawByTurn[turn] = entry
	}
	page := newPage()
	for index := position; index < len(matches); index++ {
		candidate := matches[index]
		raw, ok := rawByTurn[candidate.Turn]
		if !ok {
			return "", errors.New("Thread reference not found")
		}
		projected := r.entry(raw, candidate.Turn)
		projected["response_preview"] = projected["response_text"]
		delete(projected, "response_text")
		projected["score"] = candidate.Score
		projected["matches"] = candidate.Matches
		page["entries"] = append(pageEntries(page), projected)
		if !fits(page, budget) {
			entries := pageEntries(page)
			page["entries"] = entries[:len(entries)-1]
			return finishPage(page, fmt.Sprintf("search.%d.%d.%s", end, index, signature)), nil
		}
	}
	return finishPage(page, nil), nil
}

// ReadContent mirrors _readout.ThreadReader.read_content.
func (r *ThreadReader) ReadContent(ref string, offsetChars, limitChars, budgetBytes int) (string, error) {
	if len(ref) > maxReferenceChars || offsetChars < 0 || limitChars <= 0 {
		return "", errors.New("Invalid reference, offset_chars, or limit_chars")
	}
	budget, err := readoutBudget(budgetBytes)
	if err != nil {
		return "", err
	}
	key := pageCacheKey{ref, offsetChars, limitChars, fmt.Sprintf("read:%d", budget)}
	if hit := r.cachedPage(key, budget); hit != "" {
		return hit, nil
	}
	windowCap := minInt(limitChars, MAX_READOUT_BYTES)
	var window string
	var total int
	var source map[string]any
	if strings.HasPrefix(ref, "r") && (r.Redaction == nil || r.Redaction.IsEmpty()) {
		window, total, err = r.Store.readResultWindow(ref, offsetChars, windowCap)
		if err != nil {
			return "", err
		}
		source = storedResultSource()
	} else {
		value, src, err := r.resolve(ref)
		if err != nil {
			return "", err
		}
		permitted := r.redact(value)
		text, ok := permitted.(string)
		if !ok {
			text = encode(permitted)
		}
		window = sliceChars(text, offsetChars, offsetChars+windowCap)
		total = utf8.RuneCountInString(text)
		source = src
	}
	if offsetChars > total {
		return "", errors.New("offset_chars exceeds content length")
	}
	lower, upper := 0, utf8.RuneCountInString(window)
	for lower < upper {
		length := (lower + upper + 1) / 2
		candidate := contentPage(ref, window, offsetChars, length, source, total)
		if byteSize(candidate) <= budget {
			lower = length
		} else {
			upper = length - 1
		}
	}
	page := contentPage(ref, window, offsetChars, lower, source, total)
	r.Pages.Put(key, page)
	return encode(page), nil
}

func (r *ThreadReader) cachedPage(key pageCacheKey, budget int) string {
	cached, ok := r.Pages.Get(key)
	if !ok {
		return ""
	}
	hit := map[string]any{}
	for k, v := range cached {
		hit[k] = v
	}
	hit["cached"] = true
	if byteSize(hit) > budget {
		return ""
	}
	return encode(hit)
}

// FindContent mirrors _readout.ThreadReader.find_content.
func (r *ThreadReader) FindContent(ref, query string, offsetChars, limit, budgetBytes int) (string, error) {
	if query == "" || offsetChars < 0 || len(ref) > maxReferenceChars {
		return "", errors.New("Require a nonempty query, valid ref, and nonnegative offset")
	}
	budget, err := readoutBudget(budgetBytes)
	if err != nil {
		return "", err
	}
	key := pageCacheKey{ref, offsetChars, limit, fmt.Sprintf("find:%d:%s", budget, query)}
	if hit := r.cachedPage(key, budget); hit != "" {
		return hit, nil
	}
	raw, source, err := r.resolve(ref)
	if err != nil {
		return "", err
	}
	value := r.redact(raw)
	text, ok := value.(string)
	if !ok {
		text = encode(value)
	}
	if offsetChars > utf8.RuneCountInString(text) {
		return "", errors.New("offset_chars exceeds content length")
	}
	page := map[string]any{
		"ref":               ref,
		"source":            source,
		"matches":           []any{},
		"next_offset_chars": nil,
		"has_more":          false,
		"total_chars":       utf8.RuneCountInString(text),
	}
	matches := page["matches"].([]any)
	position := offsetChars
	for {
		start, ok := regexpFindCaseInsensitive(sliceChars(text, position, utf8.RuneCountInString(text)), query)
		if !ok {
			break
		}
		foundStart, foundEnd := position+start, position+start+utf8.RuneCountInString(query)
		offset := maxInt(0, foundStart-contextChars)
		item := map[string]any{
			"offset_chars":       offset,
			"match_offset_chars": foundStart,
			"excerpt":            sliceChars(text, offset, offset+excerptChars),
		}
		matches = append(matches, item)
		page["matches"] = matches
		if byteSize(page)+cursorReserveBytes > budget {
			matches = matches[:len(matches)-1]
			page["matches"] = matches
			page["next_offset_chars"] = foundStart
			page["has_more"] = true
			break
		}
		position = foundEnd
		if len(matches) >= limit {
			if _, more := regexpFindCaseInsensitive(sliceChars(text, position, utf8.RuneCountInString(text)), query); more {
				page["next_offset_chars"] = position
				page["has_more"] = true
			}
			break
		}
	}
	r.Pages.Put(key, page)
	return encode(page), nil
}

func contentPage(ref, window string, offset, length int, source map[string]any, total int) map[string]any {
	nextOffset := offset + length
	hasMore := nextOffset < total
	var next any
	if hasMore {
		next = nextOffset
	}
	return map[string]any{
		"ref":               ref,
		"source":            source,
		"content":           sliceChars(window, 0, length),
		"offset_chars":      offset,
		"total_chars":       total,
		"next_offset_chars": next,
		"has_more":          hasMore,
	}
}

// resolve mirrors _readout.ThreadReader._resolve.
func (r *ThreadReader) resolve(ref string) (any, map[string]any, error) {
	if strings.HasPrefix(ref, "r") {
		obj, err := r.Store.LoadResultObject(ref)
		if err != nil {
			return nil, nil, err
		}
		return obj["content"], storedResultSource(), nil
	}
	m := referencePattern.FindStringSubmatch(ref)
	if m == nil {
		return nil, nil, errors.New("Invalid thread reference")
	}
	turn, _ := strconv.Atoi(m[1])
	entries := r.Store.LoadRange(turn, turn)
	if len(entries) == 0 {
		return nil, nil, errors.New("Thread reference not found")
	}
	entry := entries[0]
	msgIdx, callIdx := -1, -1
	if m[2] != "" {
		msgIdx, _ = strconv.Atoi(m[2])
	}
	if m[3] != "" {
		callIdx, _ = strconv.Atoi(m[3])
	}
	field := m[4]
	provenance := getMap(entry, "provenance")["source"]
	source := map[string]any{
		"scope":             "recorded_history",
		"interpretation":    recordedHistoryLabel,
		"turn":              turn,
		"timestamp_utc":     r.preview(entry["timestamp_utc"], fmt.Sprintf("t%d.timestamp_utc", turn)),
		"provenance_source": r.preview(provenance, fmt.Sprintf("t%d.provenance_source", turn)),
		"source_kind":       "entry_metadata",
	}
	if m[3] != "" {
		if m[2] == "" {
			return nil, nil, errors.New("Call reference requires a message index")
		}
		value, err := resolveCall(entry, msgIdx, callIdx, field)
		if err != nil {
			return nil, nil, err
		}
		source["source_kind"] = "assistant_tool_call"
		return value, source, nil
	}
	if m[2] != "" {
		value, role, err := resolveMessage(entry, msgIdx, field)
		if err != nil {
			return nil, nil, err
		}
		source["source_kind"] = role
		return value, source, nil
	}
	value, err := r.resolveEntryField(entry, field)
	if err != nil {
		return nil, nil, err
	}
	if field == "query" {
		source["source_kind"] = "user"
	} else if field == "response_text" {
		if strings.HasPrefix(pyStr(defaultStr(provenance, "")), "host-") {
			source["source_kind"] = "host"
		} else {
			source["source_kind"] = "assistant"
		}
	}
	return value, source, nil
}

func (r *ThreadReader) resolveEntryField(entry map[string]any, field string) (any, error) {
	if field == "timestamp_utc" {
		return entry["timestamp_utc"], nil
	}
	if field == "provenance_source" {
		return getMap(entry, "provenance")["source"], nil
	}
	if field == "task_id" {
		return entryTaskID(entry), nil
	}
	if field == "pending_approval_request_id" {
		return getMap(entry, "provenance")["pending_approval_request_id"], nil
	}
	valid := false
	for _, f := range entryFields {
		if f == field {
			valid = true
			break
		}
	}
	if !valid || field == "" {
		return nil, errors.New("Entry field not found or not available for expansion")
	}
	return entry[field], nil
}

func resolveCall(entry map[string]any, messageIndex, callIndex int, field string) (any, error) {
	transcript := getList(entry, "transcript")
	if messageIndex < 0 || messageIndex >= len(transcript) {
		return nil, errors.New("Transcript reference not found")
	}
	message, ok := transcript[messageIndex].(map[string]any)
	if !ok {
		return nil, errors.New("Call field reference not found")
	}
	calls := getList(message, "tool_calls")
	if callIndex < 0 || callIndex >= len(calls) || (field != "name" && field != "arguments") {
		return nil, errors.New("Call field reference not found")
	}
	call, ok := calls[callIndex].(map[string]any)
	if !ok {
		return nil, errors.New("Call field reference not found")
	}
	fn, ok := call["function"].(map[string]any)
	if !ok {
		fn = call
	}
	return fn[field], nil
}

func resolveMessage(entry map[string]any, index int, field string) (any, string, error) {
	transcript := getList(entry, "transcript")
	if index < 0 || index >= len(transcript) {
		return nil, "", errors.New("Transcript reference not found")
	}
	message := transcript[index]
	m, isMap := message.(map[string]any)
	roleKind := "message"
	if isMap {
		if role, ok := m["role"].(string); ok && (role == "user" || role == "assistant" || role == "tool" || role == "system") {
			roleKind = role
		}
	}
	if field == "" {
		if !isMap {
			return message, roleKind, nil
		}
		out := map[string]any{}
		for _, key := range messageFields {
			if v, present := m[key]; present {
				out[key] = v
			}
		}
		return out, roleKind, nil
	}
	valid := false
	for _, f := range messageFields {
		if f == field {
			valid = true
			break
		}
	}
	v, present := m[field]
	if !valid || !isMap || !present {
		return nil, "", errors.New("Message field not found")
	}
	return v, roleKind, nil
}

// ── Task view (_taskview.py) ───────────────────────────────────────────────

const (
	recordedHistoryLabel    = "Recorded exchanges, not the active request; earlier instructions and diagnoses are historical."
	DEFAULT_TASK_VIEW_BYTES = 8192
	objectiveChars          = 400
	latestChars             = 400
	maxModes                = 12
	maxEffects              = 12
	maxArtifacts            = 6
	maxEvidence             = 16
)

func taskViewText(value any, redaction *RedactionPolicy) string {
	permitted := RedactValue(pyStr(defaultStr(value, "")), redaction)
	if s, ok := permitted.(string); ok {
		return s
	}
	return pyStr(permitted)
}

func taskViewPreview(text string, limit int) map[string]any {
	field := map[string]any{"text": headChars(text, limit), "total_chars": utf8.RuneCountInString(text)}
	if utf8.RuneCountInString(text) > limit {
		field["abbreviated"] = true
	}
	return field
}

func taskViewStatus(entry map[string]any) string {
	provenance := getMap(entry, "provenance")
	if provenance["pending_approval_request_id"] != nil && provenance["pending_approval_request_id"] != "" {
		return "awaiting_approval"
	}
	if provenance["operator_exit"] == "paused" {
		return "paused"
	}
	if v, ok := entry["yielded"].(bool); ok && v {
		return "awaiting_user"
	}
	return "open"
}

func taskViewBlocker(entry map[string]any, redaction *RedactionPolicy) map[string]any {
	provenance := getMap(entry, "provenance")
	if reqID := provenance["pending_approval_request_id"]; reqID != nil && reqID != "" {
		return map[string]any{"kind": "approval", "request_id": taskViewText(reqID, redaction)}
	}
	if v, ok := entry["yielded"].(bool); ok && v {
		return map[string]any{"kind": "question", "question": taskViewText(entry["yield_question"], redaction)}
	}
	return nil
}

func taskViewModes(scoped []TurnEntry, redaction *RedactionPolicy) []any {
	modes := []any{}
	for _, te := range scoped {
		for _, value := range []any{te.Entry["mode_id"], te.Entry["transitioned_to"], te.Entry["handoff_to"]} {
			name := taskViewText(value, redaction)
			if name == "" {
				continue
			}
			found := false
			for _, existing := range modes {
				if existing == name {
					found = true
					break
				}
			}
			if !found {
				modes = append(modes, name)
			}
		}
	}
	if len(modes) > maxModes {
		modes = modes[:maxModes]
	}
	return modes
}

func taskViewEffects(scoped []TurnEntry, redaction *RedactionPolicy) []any {
	type key struct{ capability, effect string }
	order := []key{}
	counts := map[key]int{}
	artifacts := map[key][]string{}
	for _, te := range scoped {
		for _, rawReceipt := range getList(te.Entry, "effect_receipts") {
			receipt, ok := rawReceipt.(map[string]any)
			if !ok {
				continue
			}
			capability := pyStr(defaultStr(receipt["capability_id"], ""))
			if capability == "" {
				continue
			}
			k := key{capability, pyStr(defaultStr(receipt["effect"], ""))}
			if _, seen := counts[k]; !seen {
				order = append(order, k)
			}
			counts[k]++
			artifact := taskViewText(receipt["artifact_id"], redaction)
			if artifact != "" {
				seen := false
				for _, a := range artifacts[k] {
					if a == artifact {
						seen = true
						break
					}
				}
				if !seen {
					artifacts[k] = append(artifacts[k], artifact)
				}
			}
		}
	}
	out := []any{}
	if len(order) > maxEffects {
		order = order[:maxEffects]
	}
	for _, k := range order {
		ids := artifacts[k]
		if len(ids) > maxArtifacts {
			ids = ids[:maxArtifacts]
		}
		idsAny := []any{}
		for _, id := range ids {
			idsAny = append(idsAny, id)
		}
		out = append(out, map[string]any{
			"capability_id": taskViewText(k.capability, redaction),
			"effect":        taskViewText(k.effect, redaction),
			"count":         counts[k],
			"artifact_ids":  idsAny,
		})
	}
	return out
}

func taskViewEvidence(scoped []TurnEntry) []string {
	refs := []string{}
	for _, te := range scoped {
		for index, rawMessage := range getList(te.Entry, "transcript") {
			message, ok := rawMessage.(map[string]any)
			if !ok {
				continue
			}
			if message["role"] == "tool" {
				if _, hasContent := message["content"]; hasContent {
					refs = append(refs, fmt.Sprintf("t%d.m%d.content", te.Turn, index))
				}
			}
		}
		provenance := getMap(te.Entry, "provenance")
		if src := provenance["source"]; src == "host-approved" || src == "host-approved-failed" {
			refs = append(refs, fmt.Sprintf("t%d.response_text", te.Turn))
		}
	}
	return refs
}

func taskViewObjective(scoped []TurnEntry, redaction *RedactionPolicy) (string, int) {
	for _, te := range scoped {
		query := taskViewText(te.Entry["query"], redaction)
		if query != "" && !strings.HasPrefix(strings.TrimLeft(query, " \t\n\r"), "/") {
			return query, te.Turn
		}
	}
	return taskViewText(scoped[0].Entry["query"], redaction), scoped[0].Turn
}

// compactTaskViewText mirrors _taskview._compact_text for the dict case.
func compactTaskViewText(value any, limit int) any {
	switch t := value.(type) {
	case string:
		return headChars(t, limit)
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = compactTaskViewText(item, limit)
		}
		return out
	case map[string]any:
		compacted := map[string]any{}
		for key, item := range t {
			if strings.HasSuffix(key, "_ref") || key == "status" || key == "kind" || key == "scope" ||
				key == "selection" || key == "interpretation" || key == "status_basis" {
				compacted[key] = item
			} else if (key == "task_id" || key == "request_id") && utf8.RuneCountInString(pyStrValue(item)) > limit {
				if s, ok := item.(string); ok {
					compacted[key] = taskViewPreview(s, limit)
					continue
				}
				compacted[key] = compactTaskViewText(item, limit)
			} else {
				compacted[key] = compactTaskViewText(item, limit)
			}
		}
		if original, ok := t["text"].(string); ok && utf8.RuneCountInString(original) > limit {
			compacted["abbreviated"] = true
		}
		return compacted
	}
	return value
}

func pyStrValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func trimTaskView(view map[string]any, budget int) map[string]any {
	textLimit := objectiveChars
	for len(encode(view)) > budget {
		if evidence, ok := view["evidence"].([]any); ok && len(evidence) > 0 {
			view["evidence"] = evidence[1:]
			view["evidence_truncated"] = true
		} else if effects, ok := view["effects"].([]any); ok && len(effects) > 0 {
			view["effects"] = effects[:len(effects)-1]
			view["effects_truncated"] = true
		} else if modes, ok := view["modes"].([]any); ok && len(modes) > 0 {
			view["modes"] = modes[:len(modes)-1]
			view["modes_truncated"] = true
		} else {
			textLimit /= 2
			if textLimit < 1 {
				textLimit = 1
			}
			view = compactTaskViewText(view, textLimit).(map[string]any)
			view["truncated"] = true
		}
	}
	return view
}

// BuildTaskView mirrors _taskview.build_task_view.
func BuildTaskView(entries []TurnEntry, taskID string, redaction *RedactionPolicy, budgetBytes int) map[string]any {
	ordered := entries
	budget := maxInt(1024, budgetBytes)
	selection := "latest_recorded"
	if taskID != "" {
		selection = "explicit_task_id"
	}
	recorded := map[string]any{
		"scope":          "recorded_history",
		"interpretation": recordedHistoryLabel,
		"selection":      selection,
		"status_basis":   "recorded_observation_not_live_state",
		"as_of_utc":      nil,
	}
	if len(ordered) == 0 {
		view := mergeMaps(recorded, map[string]any{"task_id": taskViewText(taskID, redaction), "status": "empty", "turns": 0})
		return trimTaskView(view, budget)
	}
	target := taskID
	if target == "" {
		for i := len(ordered) - 1; i >= 0; i-- {
			if id := entryTaskID(ordered[i].Entry); id != "" {
				target = id
				break
			}
		}
	}
	var scoped []TurnEntry
	if target != "" {
		for _, te := range ordered {
			if entryTaskID(te.Entry) == target {
				scoped = append(scoped, te)
			}
		}
	} else {
		scoped = []TurnEntry{ordered[len(ordered)-1]}
	}
	if len(scoped) == 0 {
		view := mergeMaps(recorded, map[string]any{
			"task_id":                   taskViewText(target, redaction),
			"status":                    "empty",
			"turns":                     0,
			"history_may_be_incomplete": ordered[0].Turn > 0,
		})
		return trimTaskView(view, budget)
	}
	objective, objectiveTurn := taskViewObjective(scoped, redaction)
	latest := scoped[len(scoped)-1]
	latestText := taskViewText(latest.Entry["response_text"], redaction)
	evidence := taskViewEvidence(scoped)
	evidenceAny := []any{}
	for _, ref := range evidence {
		evidenceAny = append(evidenceAny, ref)
	}
	view := mergeMaps(recorded, map[string]any{
		"as_of_utc":                 RedactValue(latest.Entry["timestamp_utc"], redaction),
		"task_id":                   taskViewText(target, redaction),
		"task_id_ref":               fmt.Sprintf("t%d.task_id", latest.Turn),
		"status":                    taskViewStatus(latest.Entry),
		"turns":                     len(scoped),
		"turn_range":                []any{scoped[0].Turn, latest.Turn},
		"history_may_be_incomplete": ordered[0].Turn > 0,
		"objective":                 taskViewPreview(objective, objectiveChars),
		"objective_ref":             fmt.Sprintf("t%d.query", objectiveTurn),
		"modes":                     taskViewModes(scoped, redaction),
		"effects":                   taskViewEffects(scoped, redaction),
		"evidence_truncated":        len(evidence) > maxEvidence,
		"latest":                    taskViewPreview(latestText, latestChars),
		"latest_ref":                fmt.Sprintf("t%d.response_text", latest.Turn),
	})
	if len(evidenceAny) > maxEvidence {
		evidenceAny = evidenceAny[len(evidenceAny)-maxEvidence:]
	}
	view["evidence"] = evidenceAny
	if blocker := taskViewBlocker(latest.Entry, redaction); blocker != nil {
		if blocker["kind"] == "approval" {
			blocker["request_id_ref"] = fmt.Sprintf("t%d.pending_approval_request_id", latest.Turn)
		} else {
			blocker["question_ref"] = fmt.Sprintf("t%d.yield_question", latest.Turn)
		}
		view["blocker"] = blocker
	}
	return trimTaskView(view, budget)
}

func mergeMaps(a, b map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// TaskView mirrors _readout.ThreadReader.task_view.
func (r *ThreadReader) TaskView(taskID string, budgetBytes int) (string, error) {
	if budgetBytes <= 0 {
		return "", errors.New("budget_bytes must be positive")
	}
	budget := maxInt(1024, minInt(budgetBytes, MAX_READOUT_BYTES))
	end := r.Store.NextTurnIndex() - 1
	if end < 0 {
		return encode(BuildTaskView(nil, taskID, r.Redaction, budget)), nil
	}
	start := maxInt(0, end-maxInt(1, minInt(taskScanLimit, taskScanLimit))+1)
	entries := r.Store.IterRange(start, end)
	return encode(BuildTaskView(entries, taskID, r.Redaction, budget)), nil
}
