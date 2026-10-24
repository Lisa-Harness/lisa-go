package thread

import (
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// encode is JSON with compact separators and no HTML escaping, the Go
// counterpart of json.dumps(ensure_ascii=False, separators=(",", ":")).
// Go's json.Marshal HTML-escapes <, >, and &, inflating byte counts and
// changing digest inputs; encoding/json.Encoder with SetEscapeHTML(false)
// matches Python's raw emission byte-for-byte.
func encode(value any) string {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return "null"
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

func trimSpace(s string) string { return strings.TrimSpace(s) }

func lowerTrim(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func splitLines(s string) []string { return strings.Split(s, "\n") }

// encodeWithDefaultSeparators returns the byte length of value serialized the
// way Python's json.dumps does with default separators (", " / ": ") — the
// measure _taskview._trim uses. It walks the compact stream with a
// scanner-aware pass: spaces are inserted after every comma and colon that
// sits outside a string literal, where a string literal is tracked from the
// opening quote through escaped characters. Python guarantees object keys to
// be strings, and the values here are JSON-decoded trees, so the result is
// byte-identical to json.dumps(value).
func pyDefaultSepLen(value any) int {
	compact := encode(value)
	inString := false
	escaped := false
	var b strings.Builder
	for i := 0; i < len(compact); i++ {
		c := compact[i]
		if escaped {
			b.WriteByte(c)
			escaped = false
			continue
		}
		switch {
		case inString && c == '\\':
			b.WriteByte(c)
			escaped = true
		case c == '"':
			inString = !inString
			b.WriteByte(c)
		case !inString && (c == ',' || c == ':'):
			b.WriteByte(c)
			b.WriteByte(' ')
		default:
			b.WriteByte(c)
		}
	}
	return b.Len()
}

// byteSize is len(encode(value).encode("utf-8")).
func byteSize(value any) int { return len(encode(value)) }

// pyTruthy mirrors Python truthiness for JSON-decoded values: nil, "", false,
// 0, and empty containers are false; everything else is true.
func pyTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	case map[string]any:
		return len(t) != 0
	case []any:
		return len(t) != 0
	default:
		return true
	}
}

// defaultStr mirrors (x or def) with Python truthiness: falsy values collapse
// to def, matching every `or` site in the Python thread modules.
func defaultStr(v any, def string) any {
	if !pyTruthy(v) {
		return def
	}
	return v
}

// strField mirrors str(entry.get(key) or ""): Python truthiness, then str().
func strField(entry map[string]any, key string) string {
	v := entry[key]
	if pyTruthy(v) {
		return pyStr(v)
	}
	return ""
}

// pyGetStrDefault mirrors str(dict.get(key, default)): the default applies
// only when the key is absent; a present null renders "None" (Python's
// str(None)), as in _backend.build_recent_excerpt's prov.get("source", ...).
func pyGetStrDefault(m map[string]any, key, def string) string {
	if m == nil {
		return def
	}
	v, ok := m[key]
	if !ok {
		return def
	}
	if v == nil {
		return "None"
	}
	return pyStr(v)
}
func pyIsUnicodeSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ',
		0x1c, 0x1d, 0x1e, 0x1f, 0x85, 0xa0, 0x1680,
		0x2028, 0x2029, 0x202f, 0x205f, 0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// pyLstrip mirrors Python str.lstrip() (Unicode whitespace).
func pyLstrip(s string) string {
	return strings.TrimLeftFunc(s, pyIsUnicodeSpace)
}

// pyStr mirrors str(value) for JSON-decoded values. Integral JSON numbers
// decode to Python ints (str(5) -> "5", str(5.0) -> "5.0"), but Go decodes
// every number to float64; render integral values without ".0" so the Go
// str() matches Python's str() over the same JSON document. Non-integral or
// out-of-integer-range floats go through Python's float repr rules: fixed
// notation only below 1e16 / above 1e-4, exponent form at and beyond those
// thresholds, shortest-roundtrip digits.
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
		return formatPyFloat(t)
	default:
		return encode(t)
	}
}

const (
	pyFloatMaxFixed = 1e16
	pyFloatMinExp   = 1e-4
)

// formatPyFloat mirrors Python str(float) (repr): shortest round-trip digits
// with fixed notation for magnitudes in [1e-4, 1e16) (and zero), switching to
// e-notation outside; always ".0"-suffixed when fixed notation carries no
// fractional part. Integral float64s render WITHOUT the ".0" suffix: JSON
// integral numbers decode to Python ints (str(5) == "5"), and Go's decoder
// collapses that distinction, so the int-style rendering matches the
// overwhelmingly-common host-written integer fields.
func formatPyFloat(t float64) string {
	if math.IsNaN(t) {
		return "nan"
	}
	if math.IsInf(t, 1) {
		return "inf"
	}
	if math.IsInf(t, -1) {
		return "-inf"
	}
	if t == 0 {
		return "0"
	}
	a := math.Abs(t)
	if a < pyFloatMaxFixed && a >= pyFloatMinExp && t == math.Trunc(t) {
		return strconv.FormatInt(int64(t), 10)
	}
	if a >= pyFloatMaxFixed || a < pyFloatMinExp {
		return strconv.FormatFloat(t, 'e', -1, 64)
	}
	s := strconv.FormatFloat(t, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
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
