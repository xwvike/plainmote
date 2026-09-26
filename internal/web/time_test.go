package web

import (
	"strings"
	"testing"
	"time"
)

func TestLocalTimeMarkupCarriesUTCInstant(t *testing.T) {
	value := time.Date(2026, 9, 23, 12, 34, 56, 0, time.FixedZone("JST", 9*60*60))
	markup := string(localTimeMarkup(value, false))
	for _, want := range []string{
		`datetime="2026-09-23T03:34:56Z"`,
		`data-local-time="second"`,
		`2026-09-23 03:34:56 UTC`,
	} {
		if !strings.Contains(markup, want) {
			t.Errorf("markup %q does not contain %q", markup, want)
		}
	}
}

func TestLocalTimeMarkupMinuteAndZeroFallback(t *testing.T) {
	value := time.Date(2026, 9, 23, 3, 34, 56, 0, time.UTC)
	markup := string(localTimeMarkup(value, true))
	if !strings.Contains(markup, `data-local-time="minute"`) || !strings.Contains(markup, `2026-09-23 03:34 UTC`) {
		t.Fatalf("unexpected minute markup %q", markup)
	}
	if got := string(localTimeMarkup(time.Time{}, false)); got != "-" {
		t.Fatalf("zero time rendered as %q", got)
	}
}
