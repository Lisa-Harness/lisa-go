// Package storage is a faithful Go port of the Lisa sandboxed file-I/O
// primitive (lisa/sdk/storage/_backend.py). It preserves the two
// non-bypassable safety invariants required by the contract
// (lisa/docs/GO_CONNECTOR_SDK.md):
//
//  1. the files sandbox escapes are rejected: absolute paths, ".." segments,
//     and any resolution that escapes <data_dir>/files are refused;
//  2. the deny-glob policy: fnmatch-syntax patterns matched against the
//     forward-slash-relative sandbox path, case-insensitive, deny the path.
//
// A port that weakens either invariant is a defect, not a shortcut.
//
// The backend also owns the model-manifest pair (config/models.yaml and
// state/models.proposed.yaml), which live under the data dir's category
// dirs, outside the files sandbox (lisa/sdk/layout.py).
package storage

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// Layout constants (mirror lisa/sdk/layout.py).
const (
	configDirName = "config"
	dataDirName   = "data"
	stateDirName  = "state"
)

// filesSubdir is the sandbox subdirectory under the data dir.
const filesSubdir = "files"

// binarySuffixes are suffixes whose bytes are not text. A file with one of
// these suffixes is stored exactly as given — normalizing its trailing
// bytes would corrupt the format (a PNG's final byte is part of its IEND
// chunk, a DB's page layout is byte-exact).
var binarySuffixes = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
	".bmp": true, ".ico": true, ".tiff": true, ".pdf": true, ".zip": true,
	".gz": true, ".bz2": true, ".xz": true, ".7z": true, ".tar": true,
	".jar": true, ".whl": true, ".so": true, ".dylib": true, ".dll": true,
	".exe": true, ".bin": true, ".class": true, ".o": true, ".a": true,
	".pyc": true, ".wasm": true, ".woff": true, ".woff2": true, ".ttf": true,
	".otf": true, ".eot": true, ".mp3": true, ".mp4": true, ".m4a": true,
	".mov": true, ".avi": true, ".mkv": true, ".wav": true, ".flac": true,
	".sqlite": true, ".sqlite3": true, ".db": true,
}

// PathRejected reports that a path is denied by the glob denylist policy.
// It is distinct from the sandbox-escape and malformed-path rejections
// (which return nil from Backend.ResolveFilePath) so callers can surface an
// explicit "denied by policy" error to the model/operator.
type PathRejected struct {
	Path string
}

func (e *PathRejected) Error() string { return e.Path }

// isTextContent reports whether content may be normalized as text: it
// decodes as UTF-8 (Go strings holding invalid UTF-8 fail this check) and
// the target path does not name a binary format.
func isTextContent(path, content string) bool {
	suffix := strings.ToLower(filepath.Ext(path))
	if binarySuffixes[suffix] {
		return false
	}
	return utf8.ValidString(content)
}

// isSafeRelativePath rejects paths that could escape a root: blocks
// absolute paths and any path containing a ".." segment
// (mirrors lisa/sdk/content/_reader.py is_safe_relative_path).
func isSafeRelativePath(normalized string) bool {
	if normalized == "" {
		return false
	}
	if filepath.IsAbs(normalized) || isPosixAbs(normalized) {
		return false
	}
	parts := strings.FieldsFunc(normalized, func(r rune) bool { return r == '/' || r == '\\' })
	if len(parts) == 0 {
		return false
	}
	for _, p := range parts {
		if p == ".." {
			return false
		}
	}
	return true
}

// isPosixAbs reports whether p is an absolute path by either POSIX or
// Windows rules, so the sandbox holds regardless of the host OS.
func isPosixAbs(p string) bool {
	if strings.HasPrefix(p, "/") {
		return true
	}
	return len(p) >= 3 && p[1] == ':' && (p[2] == '/' || p[2] == '\\') &&
		((p[0] >= 'a' && p[0] <= 'z') || (p[0] >= 'A' && p[0] <= 'Z'))
}

// Backend is the sandboxed file I/O backend. It owns two concerns, both
// derived from a single data dir:
//
//  1. File I/O — a filesystem sandbox scoped to <dataDir>/data/files.
//     Path traversal (".." segments, absolute paths) is rejected so a
//     crafted path cannot escape the sandbox. An optional deny-glob list
//     (fnmatch syntax, forward-slash paths, case-insensitive) adds
//     defense-in-depth policy on top.
//  2. Model manifests — models.yaml (active) and models.proposed.yaml
//     (proposal) live directly under the data dir, outside the files
//     sandbox.
type Backend struct {
	dataDir   string
	readonly  bool
	denyGlobs []string
}

// New builds a Backend rooted at dataDir. When readonly is false the
// files/ subdirectory is created (parents=true, exist-ok); in readonly
// mode it is NOT created — it must pre-exist or reads return empty /
// error results. The legacy checkpoints/ dir, removed in Phase 5 of the
// checkpoint teardown, is best-effort removed either way.
func New(dataDir string, readonly bool, denyGlobs []string) *Backend {
	b := &Backend{dataDir: dataDir, readonly: readonly, denyGlobs: denyGlobs}
	if !readonly {
		_ = os.MkdirAll(filepath.Join(dataDir, dataDirName, filesSubdir), 0o755)
	}
	// One-time migration: the legacy checkpoints/ directory was removed in
	// Phase 5 of the checkpoint teardown. Best-effort rmtree; failures
	// (permissions, missing dir) are ignored here (the Python logs them).
	legacy := filepath.Join(dataDir, "checkpoints")
	if _, err := os.Stat(legacy); err == nil {
		_ = os.RemoveAll(legacy)
	}
	return b
}

// DataDir returns the canonical data root.
func (b *Backend) DataDir() string { return b.dataDir }

// Readonly reports whether write/delete operations are rejected.
func (b *Backend) Readonly() bool { return b.readonly }

// DenyGlobs returns the configured deny glob list.
func (b *Backend) DenyGlobs() []string { return b.denyGlobs }

// FilesRoot returns the files sandbox root.
func (b *Backend) FilesRoot() string {
	return filepath.Join(b.dataDir, dataDirName, filesSubdir)
}

// ManifestPath returns the path to the active or proposed model manifest.
// Manifests live under the data dir's category dirs — not under the
// files/ sandbox. "active" → config/models.yaml; "proposal" →
// state/models.proposed.yaml.
func (b *Backend) ManifestPath(which string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(which))
	switch normalized {
	case "active":
		return filepath.Join(b.dataDir, configDirName, "models.yaml"), nil
	case "proposal":
		return filepath.Join(b.dataDir, stateDirName, "models.proposed.yaml"), nil
	}
	return "", fmt.Errorf("manifest kind must be 'active' or 'proposal', got %s", pyRepr(which))
}

// pyRepr renders s like Python's repr(str), so error text matches the
// reference backend's {which!r} formatting byte-for-byte.
func pyRepr(s string) string {
	quote := byte('\'')
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == rune(quote):
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// DeniedByPolicy reports whether the normalized relative path matches any
// deny glob. Matching is case-insensitive on the forward-slash-normalized
// path so patterns stay portable across Windows/POSIX. fnmatch is used
// (not PurePath.match) because the latter is per-segment and surprises
// with multi-segment "**" patterns.
func (b *Backend) DeniedByPolicy(normalized string) bool {
	if len(b.denyGlobs) == 0 {
		return false
	}
	posix := strings.ReplaceAll(normalized, "\\", "/")
	lowered := strings.ToLower(posix)
	for _, pat := range b.denyGlobs {
		if fnmatch(posix, pat) || fnmatch(lowered, strings.ToLower(pat)) {
			return true
		}
	}
	return false
}

// ResolveFilePath resolves a user-supplied relative path against the files
// root. It returns ("", nil) when the path is rejected (empty, absolute,
// contains "..", or escapes the root after resolution), or a *PathRejected
// error if the path matches a deny glob.
func (b *Backend) ResolveFilePath(relativePath string) (string, error) {
	raw := strings.TrimSpace(relativePath)
	if raw == "" || filepath.IsAbs(raw) || isPosixAbs(raw) {
		return "", nil
	}
	normalized := strings.Trim(raw, "/\\")
	if !isSafeRelativePath(normalized) {
		return "", nil
	}
	if b.DeniedByPolicy(normalized) {
		return "", &PathRejected{Path: normalized}
	}
	root := b.FilesRoot()
	resolved := filepath.Join(root, normalized)
	absResolved, err := filepath.Abs(resolved)
	if err != nil {
		return "", nil
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", nil
	}
	if !withinRoot(absResolved, absRoot) {
		return "", nil
	}
	return absResolved, nil
}

// withinRoot mirrors Python's resolved.relative_to(root.resolve()) check:
// the resolved path must be the root itself or strictly under it.
func withinRoot(resolved, root string) bool {
	if resolved == root {
		return true
	}
	rootWithSep := strings.TrimSuffix(root, string(filepath.Separator)) + string(filepath.Separator)
	return strings.HasPrefix(resolved, rootWithSep)
}

// resolveOrError consolidates the rejection modes (unsafe/escape,
// policy-denied) into one call. On success: (path, ""). On rejection:
// ("", json error string).
func (b *Backend) resolveOrError(path string) (string, string) {
	resolved, err := b.ResolveFilePath(path)
	if err != nil {
		if _, ok := err.(*PathRejected); ok {
			return "", errJSON("path denied by policy")
		}
		return "", errJSON("path rejected: unsafe or escapes sandbox")
	}
	if resolved == "" {
		return "", errJSON("path rejected: unsafe or escapes sandbox")
	}
	return resolved, ""
}

// errJSON renders {"error": msg} exactly like Python's json.dumps.
func errJSON(msg string) string {
	payload, _ := json.Marshal(map[string]any{"error": msg})
	return string(payload)
}

// ReadFile reads a text file from the files sandbox. Returns the file
// content, or a JSON error object string if the path is unsafe, denied, or
// the file does not exist / is not valid text for the requested encoding.
// Only utf-8 encoding is supported (the Go port's documented restriction).
func (b *Backend) ReadFile(path, encoding string) string {
	if encoding == "" {
		encoding = "utf-8"
	}
	resolved, errStr := b.resolveOrError(path)
	if errStr != "" {
		return errStr
	}
	info, err := os.Stat(resolved)
	if err != nil || info.IsDir() {
		return errJSON(fmt.Sprintf("file not found: %s", path))
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return errJSON(fmt.Sprintf("read failed: %v", err))
	}
	if !utf8.Valid(data) {
		return errJSON("file is not valid text for the requested encoding")
	}
	return string(data)
}

// WriteFile writes text content to a file in the files sandbox, creating
// parent directories as needed. Respects readonly.
//
// Text content is stored newline-terminated (a POSIX text file ends with a
// newline). The receipt reports trailing_newline_added so the caller knows
// the stored bytes differ from the bytes it sent. Binary content — a path
// with a binary suffix, or a payload that is not valid UTF-8 — is stored
// exactly as given.
func (b *Backend) WriteFile(path, content string) string {
	if b.readonly {
		return errJSON("server is read-only")
	}
	resolved, errStr := b.resolveOrError(path)
	if errStr != "" {
		return errStr
	}
	stored := content
	newlineAdded := false
	if content != "" && !strings.HasSuffix(content, "\n") && isTextContent(path, content) {
		stored = content + "\n"
		newlineAdded = true
	}
	if !utf8.ValidString(stored) {
		// The payload is not encodable as UTF-8 (a lone surrogate, which a
		// cut multi-byte character can produce). It is not text, so it was
		// not normalized, and it cannot be stored as a text file either —
		// report that instead of raising out of the tool.
		return errJSON("content is not valid UTF-8 text")
	}
	payload := []byte(stored)
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return errJSON(fmt.Sprintf("write failed: %v", err))
	}
	// Binary mode, so the stored bytes are exactly the bytes that were
	// sent. A storage primitive is byte-faithful; the same guard already
	// guards AppendFile.
	if err := os.WriteFile(resolved, payload, 0o644); err != nil {
		return errJSON(fmt.Sprintf("write failed: %v", err))
	}
	payloadJSON, _ := json.Marshal(map[string]any{
		"ok":                     true,
		"path":                   path,
		"bytes":                  len(payload),
		"trailing_newline_added": newlineAdded,
	})
	return string(payloadJSON)
}

// ListFiles lists files under a prefix in the files sandbox. Returns a
// JSON array of relative paths (forward slashes). Denied paths are
// filtered from results; a denied prefix returns a JSON error object.
func (b *Backend) ListFiles(prefix string) string {
	root := b.FilesRoot()
	searchRoot := root
	if strings.TrimSpace(prefix) != "" {
		normalized := strings.Trim(strings.TrimSpace(prefix), "/\\")
		if !isSafeRelativePath(normalized) {
			return errJSON("prefix rejected: unsafe or escapes sandbox")
		}
		if b.DeniedByPolicy(normalized) {
			return errJSON("prefix denied by policy")
		}
		searchRoot = filepath.Join(root, normalized)
		absSearch, err := filepath.Abs(searchRoot)
		if err != nil {
			return errJSON("prefix escapes sandbox")
		}
		absRoot, err := filepath.Abs(root)
		if err != nil {
			return errJSON("prefix escapes sandbox")
		}
		if !withinRoot(absSearch, absRoot) {
			return errJSON("prefix escapes sandbox")
		}
		searchRoot = absSearch
	}
	info, err := os.Stat(searchRoot)
	if err != nil || !info.IsDir() {
		return "[]"
	}
	var files []string
	_ = filepath.WalkDir(searchRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		relPosix := strings.ReplaceAll(rel, "\\", "/")
		if len(b.denyGlobs) > 0 && b.DeniedByPolicy(relPosix) {
			return nil
		}
		files = append(files, relPosix)
		return nil
	})
	sort.Strings(files)
	if files == nil {
		return "[]"
	}
	payload, _ := json.Marshal(files)
	return string(payload)
}

// DeleteFile deletes a file from the files sandbox. Fails on directories.
// Respects readonly.
func (b *Backend) DeleteFile(path string) string {
	if b.readonly {
		return errJSON("server is read-only")
	}
	resolved, errStr := b.resolveOrError(path)
	if errStr != "" {
		return errStr
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return errJSON(fmt.Sprintf("file not found: %s", path))
	}
	if info.IsDir() {
		return errJSON("delete_file targets a directory; use a file path")
	}
	if err := os.Remove(resolved); err != nil {
		return errJSON(fmt.Sprintf("delete failed: %v", err))
	}
	payload, _ := json.Marshal(map[string]any{"ok": true, "path": path})
	return string(payload)
}

// StatFile returns file metadata (size, modified timestamp, is_dir, sha256).
// The hash is empty for directories; file hashes can guard a later EditFile.
func (b *Backend) StatFile(path string) string {
	resolved, errStr := b.resolveOrError(path)
	if errStr != "" {
		return errStr
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return errJSON(fmt.Sprintf("not found: %s", path))
	}
	digest := ""
	if info.Mode().IsRegular() {
		data, err := os.ReadFile(resolved)
		if err != nil {
			return errJSON(fmt.Sprintf("read failed: %v", err))
		}
		digest = fmt.Sprintf("%x", sha256.Sum256(data))
	}
	payload, _ := json.Marshal(map[string]any{
		"path":     path,
		"size":     info.Size(),
		"modified": info.ModTime().UTC().Format("2006-01-02T15:04:05.999999999+00:00"),
		"is_dir":   info.IsDir(),
		"sha256":   digest,
	})
	return string(payload)
}

// MakeDir creates a directory (and parents) in the files sandbox. Respects
// readonly.
func (b *Backend) MakeDir(path string) string {
	if b.readonly {
		return errJSON("server is read-only")
	}
	resolved, errStr := b.resolveOrError(path)
	if errStr != "" {
		return errStr
	}
	if err := os.MkdirAll(resolved, 0o755); err != nil {
		return errJSON(fmt.Sprintf("make_dir failed: %v", err))
	}
	payload, _ := json.Marshal(map[string]any{"ok": true, "path": path})
	return string(payload)
}

// ReadFileRange reads a char-bounded slice of a text file from the files
// sandbox. Returns {"content", "start", "length", "total", "eof"}.
// Slicing is char-based, not byte-based, so multibyte codepoints are never
// split.
func (b *Backend) ReadFileRange(path string, start, length int, encoding string) string {
	if encoding == "" {
		encoding = "utf-8"
	}
	resolved, errStr := b.resolveOrError(path)
	if errStr != "" {
		return errStr
	}
	info, err := os.Stat(resolved)
	if err != nil || info.IsDir() {
		return errJSON(fmt.Sprintf("file not found: %s", path))
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return errJSON(fmt.Sprintf("read failed: %v", err))
	}
	if !utf8.Valid(data) {
		return errJSON("file is not valid text for the requested encoding")
	}
	text := []rune(string(data))
	total := len(text)
	s := max(0, start)
	n := max(0, length)
	end := min(total, s+n)
	// A start past EOF yields an empty slice (Python semantics), not a panic.
	content := string(text[min(s, end):end])
	payload, _ := json.Marshal(map[string]any{
		"content": content,
		"start":   s,
		"length":  len([]rune(content)),
		"total":   total,
		"eof":     end >= total,
	})
	return string(payload)
}

// AppendFile appends text content to a file in the files sandbox. Creates
// parent directories as needed and the file if it does not exist. Respects
// readonly.
//
// Returns {"ok", "path", "bytes" (new total), "appended_bytes" (bytes this
// call added)}. appended_bytes distinguishes a genuine no-op (empty
// append) from a write: a receipt that claims a write happened when no
// bytes moved is not proof of an effect.
func (b *Backend) AppendFile(path, content string) string {
	if b.readonly {
		return errJSON("server is read-only")
	}
	resolved, errStr := b.resolveOrError(path)
	if errStr != "" {
		return errStr
	}
	if !utf8.ValidString(content) {
		return errJSON("content is not valid UTF-8 text")
	}
	appended := len([]byte(content))
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return errJSON(fmt.Sprintf("append failed: %v", err))
	}
	f, err := os.OpenFile(resolved, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return errJSON(fmt.Sprintf("append failed: %v", err))
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return errJSON(fmt.Sprintf("append failed: %v", err))
	}
	if err := f.Close(); err != nil {
		return errJSON(fmt.Sprintf("append failed: %v", err))
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return errJSON(fmt.Sprintf("append failed: %v", err))
	}
	payload, _ := json.Marshal(map[string]any{
		"ok":             true,
		"path":           path,
		"bytes":          info.Size(),
		"appended_bytes": appended,
	})
	return string(payload)
}

// ReadModelManifest reads the active or proposed model manifest as text.
// Returns the manifest content, or a JSON error object if the kind is
// invalid or the file does not exist.
func (b *Backend) ReadModelManifest(which string) string {
	if which == "" {
		which = "proposal"
	}
	path, err := b.ManifestPath(which)
	if err != nil {
		return errJSON(err.Error())
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return errJSON(fmt.Sprintf("manifest not found: %s (%s)", which, filepath.Base(path)))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return errJSON(fmt.Sprintf("read_model_manifest failed: %v", err))
	}
	return string(data)
}

// WriteModelManifest writes text content to the active or proposed model
// manifest. Creates the parent directory if needed. Respects readonly.
// Returns {"ok", "which", "bytes"}. Stored byte-faithfully.
func (b *Backend) WriteModelManifest(content, which string) string {
	if b.readonly {
		return errJSON("server is read-only")
	}
	if which == "" {
		which = "proposal"
	}
	path, err := b.ManifestPath(which)
	if err != nil {
		return errJSON(err.Error())
	}
	if !utf8.ValidString(content) {
		return errJSON("content is not valid UTF-8 text")
	}
	payload := []byte(content)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return errJSON(fmt.Sprintf("write_model_manifest failed: %v", err))
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		return errJSON(fmt.Sprintf("write_model_manifest failed: %v", err))
	}
	payloadJSON, _ := json.Marshal(map[string]any{
		"ok":    true,
		"which": which,
		"bytes": len(payload),
	})
	return string(payloadJSON)
}

// fnmatch implements Python's fnmatch.fnmatch: case-sensitive matching of
// name against the shell-style pattern, where "*" matches everything
// INCLUDING path separators (unlike path.Match), "?" matches any single
// character, "[seq]" matches any character in seq, and "[!seq]" negates.
// A leading "!" or "]" inside a class is literal.
//
// NOTE on "*": Python fnmatch's "*" spans "/" as well as every other
// character. Python's fnmatch does NOT special-case "**" (that is
// glob.glob's recursive form); "**" is exactly two "*" characters back to
// back. Backslash is NOT an escape inside or outside classes — it is a
// literal character.
func fnmatch(name, pattern string) bool {
	pi, ni := 0, 0
	for pi < len(pattern) {
		switch pattern[pi] {
		case '*':
			// Collapse consecutive stars.
			for pi < len(pattern) && pattern[pi] == '*' {
				pi++
			}
			if pi == len(pattern) {
				return true
			}
			for k := ni; k <= len(name); k++ {
				if fnmatch(name[k:], pattern[pi:]) {
					return true
				}
			}
			return false
		case '?':
			if ni >= len(name) {
				return false
			}
			pi++
			ni++
		case '[':
			end := fnmatchClassEnd(pattern, pi)
			if end < 0 {
				// Unterminated class: fnmatch treats "[" as a literal.
				if ni >= len(name) || name[ni] != '[' {
					return false
				}
				pi++
				ni++
				continue
			}
			if ni >= len(name) {
				return false
			}
			class := pattern[pi+1 : end-1]
			ok, _ := fnmatchClassMatch(name[ni], class)
			if !ok {
				return false
			}
			pi = end
			ni++
		default:
			if ni >= len(name) || name[ni] != pattern[pi] {
				return false
			}
			pi++
			ni++
		}
	}
	return ni == len(name)
}

// fnmatchClassEnd returns the index just past the closing "]" of a class
// starting at pattern[i] == '[', or -1 if the class is malformed.
func fnmatchClassEnd(pattern string, i int) int {
	j := i + 1
	if j < len(pattern) && (pattern[j] == '!' || pattern[j] == '^') {
		j++
	}
	if j < len(pattern) && pattern[j] == ']' {
		j++
	}
	for j < len(pattern) && pattern[j] != ']' {
		j++
	}
	if j >= len(pattern) {
		return -1
	}
	return j + 1
}

func fnmatchClassMatch(c byte, class string) (bool, bool) {
	negate := false
	i := 0
	if i < len(class) && (class[i] == '!' || class[i] == '^') {
		negate = true
		i++
	}
	matched := false
	first := true
	for i < len(class) {
		if class[i] == ']' && !first {
			break
		}
		first = false
		var lo byte = class[i]
		if i+2 < len(class) && class[i+1] == '-' && class[i+2] != ']' {
			hi := class[i+2]
			if lo <= c && c <= hi {
				matched = true
			}
			i += 3
			continue
		}
		if lo == c {
			matched = true
		}
		i++
	}
	return matched != negate, true
}
