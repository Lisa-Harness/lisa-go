// redaction.go is a faithful Go port of lisa/sdk/src/sdk/thread/redaction.py:
// strikethrough of regex-matched substrings when thread entries are read into
// the model's context. The raw thread log is never mutated; redaction is a
// read-time transform only. This prevents the model from pulling its own
// harness-authored budget/cap prose back into context as ground truth.
package thread

import (
	"log/slog"
	"regexp"
)

// RedactionMarker is the strikethrough marker substituted for matched
// substrings (Unicode combining long stroke overlay), so the redaction is
// visually obvious without removing the structural context.
const RedactionMarker = "̶"

// DefaultRedactionPatterns strip prior cap-fire / budget-exhaustion
// self-reports from thread context (mirrors DEFAULT_REDACTION_PATTERNS).
var DefaultRedactionPatterns = []string{
	// Model-authored: "I've hit my iteration budget in <mode> (...)."
	`I've hit my iteration budget[^\n]*`,
	// Model-authored: "I've hit my iteration budget for this turn. ..."
	`I've hit my iteration budget for this turn[^\n]*`,
	// Harness-fabricated capability-cap yield: "I've reached a resource
	// budget for this turn. ..."
	`I've reached a resource budget[^\n]*`,
	// Harness pause diagnostic: "stuck in <mode> for N iterations (cap)".
	`stuck in \w+ for \d+ iterations \(cap\)[^\n]*`,
	// Harness yield: "iteration cap N reached".
	`iteration cap \d+ reached[^\n]*`,
	// Harness yield: "turn budget exhausted".
	`turn budget exhausted[^\n]*`,
}

// RedactionPolicy is a compiled regex set for read-time thread text
// redaction. Immutable and safe for concurrent use.
type RedactionPolicy struct {
	compiled []*regexp.Regexp
	logger   *slog.Logger
}

// RedactionPolicyFromPatterns compiles raw regex strings into a policy.
// Invalid patterns are logged and skipped — a bad regex must not crash the
// thread read path.
func RedactionPolicyFromPatterns(patterns []string, logger *slog.Logger) *RedactionPolicy {
	p := &RedactionPolicy{logger: logger}
	for _, raw := range patterns {
		re, err := regexp.Compile(raw)
		if err != nil {
			if logger != nil {
				logger.Warn("thread: skipping invalid redaction pattern", "pattern", raw, "err", err)
			}
			continue
		}
		p.compiled = append(p.compiled, re)
	}
	return p
}

// DefaultRedactionPolicy is the default policy: DefaultRedactionPatterns.
func DefaultRedactionPolicy(logger *slog.Logger) *RedactionPolicy {
	return RedactionPolicyFromPatterns(DefaultRedactionPatterns, logger)
}

// IsEmpty reports whether no patterns are compiled (redaction is a no-op).
func (p *RedactionPolicy) IsEmpty() bool {
	return p == nil || len(p.compiled) == 0
}

// Redact replaces all pattern matches in text with the strikethrough marker.
// Returns the text unchanged when the policy is empty or nothing matches.
func (p *RedactionPolicy) Redact(text string) string {
	if p.IsEmpty() || text == "" {
		return text
	}
	result := text
	for _, re := range p.compiled {
		result = re.ReplaceAllString(result, RedactionMarker)
	}
	return result
}

// ResolveRedactionPolicy mirrors the non-bypassable env contract in
// mcp_server/thread/server.py::_resolve_redaction_policy:
// LISA_THREAD_REDACTION_ENABLED=0/false/no/off disables; a
// LISA_THREAD_REDACTION_PATTERNS value of "none" disables; non-empty
// patterns (line-split) replace the framework defaults; unset → defaults.
func ResolveRedactionPolicy(enabledValue, patternsValue string, logger *slog.Logger) *RedactionPolicy {
	enabled := lowerTrim(enabledValue)
	raw := trimSpace(patternsValue)
	switch enabled {
	case "0", "false", "no", "off":
		return nil
	}
	if lowerTrim(raw) == "none" {
		return nil
	}
	var patterns []string
	if raw != "" {
		for _, line := range splitLines(raw) {
			if trimSpace(line) != "" {
				patterns = append(patterns, line)
			}
		}
	} else {
		patterns = DefaultRedactionPatterns
	}
	if len(patterns) == 0 {
		return nil
	}
	return RedactionPolicyFromPatterns(patterns, logger)
}
