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

// TestUntilTextSaysLongLifetimesInDaysAndHours: a link that lasts a month is
// "in about 30 days", not "in about 43200 minutes".
func TestUntilTextSaysLongLifetimesInDaysAndHours(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		left time.Duration
		want string
	}{
		{30 * time.Second, "in less than a minute"},
		{10 * time.Minute, "in about 10 minutes"},
		{time.Hour - time.Second, "in about 1 hour"},
		{5*time.Hour + 10*time.Minute, "in about 5 hours"},
		{24*time.Hour - time.Second, "in about 1 day"},
		{7 * 24 * time.Hour, "in about 7 days"},
		{30*24*time.Hour - time.Second, "in about 30 days"},
	} {
		if got := untilText("en", now.Add(tc.left)); got != tc.want {
			t.Errorf("%s left: got %q, want %q", tc.left, got, tc.want)
		}
	}
}
