// Package metrics writes Get_Profile runtime counters to a Prometheus
// node_exporter textfile, consumed by the textfile collector on each VPS.
package metrics

import (
	"fmt"
	"os"
	"strings"
)

// Snapshot is a point-in-time view of the run's counters.
type Snapshot struct {
	Processed   int64
	Successful  int64
	Failed      int64
	ExactMatch  int64
	TotalTokens int64
	Alive       int64
	Dead        int64
	Exhausted   int64
	Done        int64
	TotalLines  int64
}

type metric struct {
	name, help, typ string
	value           int64
}

// Write renders snap to path in node_exporter textfile format, labelled by
// tenant. The write is atomic: content goes to a temp file in the same
// directory, then os.Rename replaces the target.
func Write(path, tenant string, snap Snapshot) error {
	metrics := []metric{
		{"getprofile_processed_total", "Emails processed (terminal outcomes)", "counter", snap.Processed},
		{"getprofile_successful_total", "Successful profile fetches", "counter", snap.Successful},
		{"getprofile_failed_total", "Failed profile fetches", "counter", snap.Failed},
		{"getprofile_exact_match_total", "Exact-match profiles written", "counter", snap.ExactMatch},
		// tokens_total is monotonically increasing (tokens are only ever added),
		// so it is a counter — the _total suffix is reserved for counters.
		{"getprofile_tokens_total", "Tokens seen total", "counter", snap.TotalTokens},
		{"getprofile_tokens_alive", "Tokens currently alive", "gauge", snap.Alive},
		{"getprofile_tokens_dead", "Tokens marked dead", "gauge", snap.Dead},
		{"getprofile_tokens_exhausted", "Tokens marked quota-exhausted", "gauge", snap.Exhausted},
		{"getprofile_lines_done", "Lines marked done in the bitmap", "gauge", snap.Done},
		{"getprofile_lines_total", "Total lines in the emails file", "gauge", snap.TotalLines},
	}

	var b strings.Builder
	label := fmt.Sprintf("{tenant=%q}", tenant)
	for _, m := range metrics {
		fmt.Fprintf(&b, "# HELP %s %s\n", m.name, m.help)
		fmt.Fprintf(&b, "# TYPE %s %s\n", m.name, m.typ)
		fmt.Fprintf(&b, "%s%s %d\n", m.name, label, m.value)
	}

	// Write to a temp file, fsync, then atomically rename — same crash-safe
	// pattern as progress.Bitmap.SaveAtomic, so the textfile collector never
	// reads a half-written or zero-length .prom file. Mode 0o644 lets
	// node_exporter (a different OS user) read it.
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create temp metrics file: %w", err)
	}
	if _, err := f.WriteString(b.String()); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp) // best-effort cleanup
		return fmt.Errorf("write temp metrics file: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp) // best-effort cleanup
		return fmt.Errorf("sync temp metrics file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp) // best-effort cleanup
		return fmt.Errorf("close temp metrics file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp) // best-effort cleanup
		return fmt.Errorf("rename metrics file: %w", err)
	}
	return nil
}
