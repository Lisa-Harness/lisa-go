package thread

import (
	"encoding/json"
	"os"
	"testing"
)

// TestGoCrossCheck is invoked by the session crosscheck driver with
// LISA_CROSSCHECK_FILE (Python-captured outputs fixture path) and
// LISA_CROSSCHECK_BASE (the exact same thread store the Python reader ran
// against). It skips silently in normal test runs.
func TestGoCrossCheck(t *testing.T) {
	file := os.Getenv("LISA_CROSSCHECK_FILE")
	base := os.Getenv("LISA_CROSSCHECK_BASE")
	if file == "" || base == "" {
		t.Skip("crosscheck env not set")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read crosscheck file: %v", err)
	}
	var pyOutputs map[string]string
	if err := json.Unmarshal(raw, &pyOutputs); err != nil {
		t.Fatalf("unmarshal crosscheck file: %v", err)
	}

	store := NewThreadStore(base, DefaultRedactionPolicy(nil), nil)
	reader := NewThreadReader(store, DefaultRedactionPolicy(nil), NewPageCache())

	canon := func(s string) (string, error) {
		var v any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			return "", err
		}
		// Sorted-key canonicalization equivalent to
		// json.dumps(sort_keys=True): encoding/json already sorts map keys.
		b, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}

	goCases := map[string]func() (string, error){
		"read_range": func() (string, error) {
			return reader.ReadRange(0, 2, false, nil, DEFAULT_READOUT_BYTES)
		},
		"read_recent": func() (string, error) {
			return reader.ReadRecent(10, false, nil, DEFAULT_READOUT_BYTES)
		},
		"read_content_t1": func() (string, error) {
			return reader.ReadContent("t1.response_text", 0, 4000, DEFAULT_READOUT_BYTES)
		},
		"find_content_t1": func() (string, error) {
			return reader.FindContent("t1.response_text", "ALPHA_TOKEN", 0, 10, DEFAULT_READOUT_BYTES)
		},
		"search": func() (string, error) {
			return reader.Search("ALPHA_TOKEN", 10, nil, DEFAULT_READOUT_BYTES, nil, nil, nil, 0, nil)
		},
		"task_view": func() (string, error) {
			return reader.TaskView("", TaskScanLimit, DEFAULT_READOUT_BYTES)
		},
	}

	for name, fn := range goCases {
		pyOut, ok := pyOutputs[name]
		if !ok {
			t.Errorf("crosscheck: python output %q missing", name)
			continue
		}
		goOut, err := fn()
		if err != nil {
			t.Errorf("crosscheck %s: go error %v", name, err)
			continue
		}
		pyC, err := canon(pyOut)
		if err != nil {
			t.Errorf("crosscheck %s: python output not JSON: %v", name, err)
			continue
		}
		goC, err := canon(goOut)
		if err != nil {
			t.Errorf("crosscheck %s: go output not JSON: %v", name, err)
			continue
		}
		if pyC != goC {
			t.Errorf("crosscheck %s: STRUCTURE MISMATCH\npython: %s\ngo:     %s", name, pyC, goC)
		}
	}
}
