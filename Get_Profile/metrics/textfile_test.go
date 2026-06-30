package metrics

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteProducesParsableTextfile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "getprofile.prom")

	snap := Snapshot{
		Processed: 100, Successful: 80, Failed: 20, ExactMatch: 75,
		TotalTokens: 10, Alive: 6, Dead: 3, Exhausted: 1,
		Done: 100, TotalLines: 500,
	}
	if err := Write(path, "t1", snap); err != nil {
		t.Fatalf("Write: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	out := string(data)

	want := []string{
		`getprofile_processed_total{tenant="t1"} 100`,
		`getprofile_successful_total{tenant="t1"} 80`,
		`getprofile_failed_total{tenant="t1"} 20`,
		`getprofile_tokens_alive{tenant="t1"} 6`,
		`getprofile_lines_done{tenant="t1"} 100`,
		`getprofile_lines_total{tenant="t1"} 500`,
		"# TYPE getprofile_processed_total counter",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("output missing %q\n--- got ---\n%s", w, out)
		}
	}
}

func TestWriteIsAtomicNoTmpLeftBehind(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "getprofile.prom")
	if err := Write(path, "t1", Snapshot{}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}
