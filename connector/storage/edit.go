package storage

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// EditFile replaces an exact substring in an existing sandboxed file without
// newline normalization. Sandbox and deny-glob checks and readonly mode apply.
// An ambiguous match is rejected unless replaceAll is set; expectedSHA256, when
// nonempty, must match the current bytes. Replacement is atomic and preserves
// file permissions. Like ReadFile, this Go port supports only UTF-8.
func (b *Backend) EditFile(path, oldString, newString string, replaceAll bool, expectedSHA256, encoding string) string {
	if b.readonly {
		return errJSON("server is read-only")
	}
	if oldString == "" {
		return errJSON("old_string must not be empty (an empty match has no unique position)")
	}
	if oldString == newString {
		return errJSON("old_string and new_string are identical; the edit would change nothing")
	}
	resolved, errStr := b.resolveOrError(path)
	if errStr != "" {
		return errStr
	}
	info, err := os.Stat(resolved)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errJSON(fmt.Sprintf("file not found: %s", path))
		}
		return errJSON(fmt.Sprintf("read failed: %v", err))
	}
	if !info.Mode().IsRegular() {
		return errJSON("edit_file targets a directory; use a file path")
	}
	// Resolve existing links before reading or staging the replacement so an
	// edit cannot follow a link outside the sandbox or replace the link itself.
	resolved, err = filepath.EvalSymlinks(resolved)
	if err != nil {
		return errJSON(fmt.Sprintf("read failed: %v", err))
	}
	root, err := filepath.EvalSymlinks(b.FilesRoot())
	if err != nil {
		return errJSON(fmt.Sprintf("read failed: %v", err))
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return errJSON(fmt.Sprintf("read failed: %v", err))
	}
	if !withinRoot(resolved, root) {
		return errJSON("path rejected: unsafe or escapes sandbox")
	}
	switch strings.ToLower(strings.ReplaceAll(encoding, "_", "-")) {
	case "", "utf-8", "utf8":
	default:
		return errJSON("unsupported encoding: only UTF-8 is supported")
	}
	original, err := os.ReadFile(resolved)
	if err != nil {
		return errJSON(fmt.Sprintf("read failed: %v", err))
	}
	if !utf8.Valid(original) {
		return errJSON("file is not valid text for the requested encoding")
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(original))
	if expectedSHA256 != "" && strings.ToLower(strings.TrimSpace(expectedSHA256)) != digest {
		payload, err := json.Marshal(map[string]any{
			"error":  "file changed since it was read (sha256 mismatch); re-read and edit the current content",
			"sha256": digest,
		})
		if err != nil {
			return errJSON(fmt.Sprintf("encoding edit error: %v", err))
		}
		return string(payload)
	}
	text := string(original)
	occurrences := strings.Count(text, oldString)
	if errStr := editMatchError(text, oldString, occurrences, replaceAll, digest); errStr != "" {
		return errStr
	}
	firstLine := strings.Count(text[:strings.Index(text, oldString)], "\n") + 1
	replacements := 1
	if replaceAll {
		replacements = occurrences
	}
	updated := strings.Replace(text, oldString, newString, replacements)
	if !utf8.ValidString(updated) {
		return errJSON("new content is not valid text for the requested encoding")
	}
	if err := atomicReplace(resolved, []byte(updated)); err != nil {
		return errJSON(fmt.Sprintf("edit failed: %v", err))
	}
	payload, err := json.Marshal(map[string]any{
		"ok":           true,
		"path":         path,
		"replacements": replacements,
		"first_line":   firstLine,
		"bytes_before": len(original),
		"bytes_after":  len(updated),
		"sha256":       fmt.Sprintf("%x", sha256.Sum256([]byte(updated))),
	})
	if err != nil {
		return errJSON(fmt.Sprintf("encoding edit receipt: %v", err))
	}
	return string(payload)
}

func editMatchError(text, fragment string, occurrences int, replaceAll bool, digest string) string {
	var fields map[string]any
	switch {
	case occurrences == 0:
		hint := ""
		fileCRLF := strings.Contains(text, "\r\n")
		fragmentCRLF := strings.Contains(fragment, "\r\n")
		if fileCRLF && !fragmentCRLF {
			hint = " (the file uses CRLF line endings; old_string must match them)"
		} else if !fileCRLF && fragmentCRLF {
			hint = " (the file uses LF line endings; old_string contains CRLF)"
		}
		fields = map[string]any{
			"error":  "old_string not found in file" + hint,
			"bytes":  len(text),
			"sha256": digest,
		}
	case occurrences > 1 && !replaceAll:
		var lines []int
		for start := 0; len(lines) < 10; {
			offset := strings.Index(text[start:], fragment)
			if offset < 0 {
				break
			}
			index := start + offset
			lines = append(lines, strings.Count(text[:index], "\n")+1)
			start = index + 1
		}
		fields = map[string]any{
			"error":  fmt.Sprintf("old_string occurs %d times; add surrounding context to select one, or set replace_all", occurrences),
			"lines":  lines,
			"sha256": digest,
		}
	default:
		return ""
	}
	payload, err := json.Marshal(fields)
	if err != nil {
		return errJSON(fmt.Sprintf("encoding edit error: %v", err))
	}
	return string(payload)
}

func atomicReplace(target string, payload []byte) (err error) {
	f, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".*.edit")
	if err != nil {
		return fmt.Errorf("creating temporary file: %w", err)
	}
	closed, renamed := false, false
	defer func() {
		if !closed {
			if closeErr := f.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("closing temporary file: %w", closeErr))
			}
		}
		if !renamed {
			if removeErr := os.Remove(f.Name()); removeErr != nil {
				err = errors.Join(err, fmt.Errorf("removing temporary file: %w", removeErr))
			}
		}
	}()
	if _, err := f.Write(payload); err != nil {
		return fmt.Errorf("writing temporary file: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("syncing temporary file: %w", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("reading target permissions: %w", err)
	}
	if err := f.Chmod(info.Mode()); err != nil {
		return fmt.Errorf("preserving target permissions: %w", err)
	}
	err = f.Close()
	closed = true
	if err != nil {
		return fmt.Errorf("closing temporary file: %w", err)
	}
	if err := os.Rename(f.Name(), target); err != nil {
		return fmt.Errorf("replacing target: %w", err)
	}
	renamed = true
	return nil
}
