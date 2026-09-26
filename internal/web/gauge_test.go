package web

import (
	"strings"
	"testing"
	"time"
)

// The gauge is how much of a link's current terms is left, never outside
// 0-100, and full for a link that does not expire.
func TestLeftPercent(t *testing.T) {
	now := time.Now()
	at := func(d time.Duration) *time.Time { t := now.Add(d); return &t }
	for _, c := range []struct {
		name  string
		start time.Time
		end   *time.Time
		want  int
	}{
		{"never expires", now.Add(-time.Hour), nil, 100},
		{"just made", now, at(time.Hour), 100},
		{"a quarter gone", now.Add(-15 * time.Minute), at(45 * time.Minute), 75},
		{"expired", now.Add(-time.Hour), at(-time.Minute), 0},
		{"no length", now, at(0), 0},
	} {
		if got := leftPercent(c.start, c.end); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

// The gauge style carries only numbers this code formatted, and a link with
// time left says how long, so the gauge can drain on its own.
func TestGaugeStyle(t *testing.T) {
	now := time.Now()
	end := now.Add(45 * time.Minute)
	if got := string(gaugeStyle(now.Add(-15*time.Minute), &end)); !strings.HasPrefix(got, "--left: 0.7") || !strings.Contains(got, "; --secs: 2700s") {
		t.Errorf("a quarter gone: %q", got)
	}
	if got := gaugeStyle(now, nil); got != "--left: 1" {
		t.Errorf("never expires: %q", got)
	}
	past := now.Add(-time.Minute)
	if got := gaugeStyle(now.Add(-time.Hour), &past); got != "--left: 0" {
		t.Errorf("expired: %q", got)
	}
}

// The page sets the token apart; the three parts still read back as the
// whole address.
func TestAddressParts(t *testing.T) {
	for address, want := range map[string][3]string{
		"https://plainmote.link/d/abc/app.log": {"https://plainmote.link/d/", "abc", "/app.log"},
		"https://plainmote.link/d/abc":         {"https://plainmote.link/d/", "abc", ""},
		"https://plainmote.link/other":         {"https://plainmote.link/other", "", ""},
	} {
		got := addressParts(address)
		if got != want || got[0]+got[1]+got[2] != address {
			t.Errorf("%s: got %q", address, got)
		}
	}
}

// What the custom box shows is readable and parses back to at least as long.
func TestShareDurationRoundTrip(t *testing.T) {
	for _, left := range []time.Duration{
		45 * time.Second, 90 * time.Minute, 24 * time.Hour, 7 * 24 * time.Hour,
		477827 * time.Second, 30*24*time.Hour + 3*time.Hour,
	} {
		shown := shareDurationInput(left)
		back, err := parseShareDuration(shown)
		if err != nil || back < left || back-left >= time.Minute {
			t.Errorf("%v shows as %q and reads back as %v (%v)", left, shown, back, err)
		}
	}
	if got := shareDurationInput(477827 * time.Second); got != "5d12h44m" {
		t.Errorf("477827s shows as %q", got)
	}
	if _, err := parseShareDuration("5dx"); err == nil {
		t.Error("a malformed duration must be refused")
	}
}
