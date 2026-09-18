package web

import (
	"strings"
	"testing"
	"time"
)

// TestProbeLogFoldsWithoutLosingCounts pins the trade the probe log exists to
// make: a burst produces one line, not one per request, and every probe it
// swallowed is still in the number that line reports.
func TestProbeLogFoldsWithoutLosingCounts(t *testing.T) {
	var probes probeLog
	start := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)

	for i := 0; i < 500; i++ {
		if line := probes.record(probeUnknown, start); line != "" {
			t.Fatalf("the window is still open, got %q", line)
		}
	}
	for i := 0; i < 3; i++ {
		if line := probes.record(probeMalformed, start.Add(time.Second)); line != "" {
			t.Fatalf("the window is still open, got %q", line)
		}
	}

	line := probes.record(probeUnknown, start.Add(probeWindow))
	if !strings.Contains(line, "unknown=501") || !strings.Contains(line, "malformed=3") {
		t.Fatalf("the folded line must carry every probe, got %q", line)
	}

	// The window restarts empty, so the next report is not a running total.
	next := probes.record(probeUnknown, start.Add(2*probeWindow))
	if !strings.Contains(next, "unknown=1") || strings.Contains(next, "malformed") {
		t.Fatalf("a closed window must not carry into the next, got %q", next)
	}
}
