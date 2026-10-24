package thread

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode/utf8"
)

// encode is JSON with compact separators and no HTML escaping, the Go
// counterpart of json.dumps(ensure_ascii=False, separators=(",", ":")).
func encode(value any) string {
	buf, err := json.Marshal(value)
	if err != nil {
		return "null"
	}
	return string(buf)
}

// byteSize is len(encode(value).encode("utf-8")).
func byteSize(value any) int { return len(encode(value)) }

func trimSpace(s string) string { return strings.TrimSpace(s) }

func lowerTrim(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func splitLines(s string) []string { return strings.Split(s, "\n") }

// pyStr mirrors str(value) for JSON-decoded values.
func pyStr(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case float64:
		if t == float64(int64(t)) {
			return encode(t)[:len(encode(t))-2] // trim ".0"
		}
		return encode(t)
	default:
		return encode(t)
	}
}

// strField mirrors str(entry.get(key) or "").
func strField(entry map[string]any, key string) string {
	if s, ok := entry[key].(string); ok {
		return s
	}
	return ""
}

// getMap mirrors entry.get(key) as a dict, or nil.
func getMap(entry map[string]any, key string) map[string]any {
	m, _ := entry[key].(map[string]any)
	return m
}

// getList mirrors entry.get(key) as a list, or nil.
func getList(entry map[string]any, key string) []any {
	l, _ := entry[key].([]any)
	return l
}

// sortedKeys returns the sorted keys of m (json.Marshal sorts map keys; use
// this anywhere iteration order matters for byte-count parity).
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// sliceChars mirrors Python text[lo:hi] over Unicode code points
// (len(text) is the rune count).
func sliceChars(s string, lo, hi int) string {
	n := utf8.RuneCountInString(s)
	lo = minInt(maxInt(lo, 0), n)
	hi = minInt(maxInt(hi, lo), n)
	if lo >= hi {
		return ""
	}
	start := 0
	for i := 0; i < lo; i++ {
		_, w := utf8.DecodeRuneInString(s[start:])
		start += w
	}
	end := start
	for i := lo; i < hi; i++ {
		_, w := utf8.DecodeRuneInString(s[end:])
		end += w
	}
	return s[start:end]
}

// tailChars mirrors Python text[-n:] ("" for n <= 0) over code points.
func tailChars(s string, n int) string {
	if n <= 0 {
		return ""
	}
	return sliceChars(s, utf8.RuneCountInString(s)-n, utf8.RuneCountInString(s))
}

// headChars mirrors Python text[:n] over code points.
func headChars(s string, n int) string {
	if n <= 0 {
		return ""
	}
	return sliceChars(s, 0, n)
}
