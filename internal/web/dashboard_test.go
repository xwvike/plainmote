package web

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Search text is bounded before it reaches LIKE, without splitting a
// character as a plain byte slice would do to a Chinese term.
func TestSearchTextIsTrimmedOnCharacterBoundaries(t *testing.T) {
	for _, input := range []string{
		strings.Repeat("a", 200),
		strings.Repeat("配置文件", 40),
		strings.Repeat("clash 订阅 ", 30),
		"短的",
		"",
	} {
		got := truncateBytes(input, maxQueryLength)
		if len(got) > maxQueryLength {
			t.Errorf("%q trimmed to %d bytes, over the %d budget", input[:10], len(got), maxQueryLength)
		}
		if !utf8.ValidString(got) {
			t.Errorf("trimming produced invalid UTF-8 for %q", input[:10])
		}
		if len(input) <= maxQueryLength && got != input {
			t.Errorf("a short term should pass through untouched, got %q", got)
		}
		if !strings.HasPrefix(input, got) {
			t.Errorf("trimming should keep a prefix, got %q", got)
		}
	}
}
