package web

import (
	"strings"
	"testing"
)

// TestContentDispositionCarriesNonASCIINames is what let the filename rule
// widen: a header field is ASCII, so a name that is not gets the quoted form
// for clients that read only that and the RFC 6266 extended form beside it.
func TestContentDispositionCarriesNonASCIINames(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"clash.yaml", `inline; filename="clash.yaml"`},
		{"机场配置.yaml", `inline; filename="____.yaml"; filename*=UTF-8''%E6%9C%BA%E5%9C%BA%E9%85%8D%E7%BD%AE.yaml`},
		{"設定.yaml", `inline; filename="__.yaml"; filename*=UTF-8''%E8%A8%AD%E5%AE%9A.yaml`},
		{"إعدادات.yaml", `inline; filename="_______.yaml"; filename*=UTF-8''%D8%A5%D8%B9%D8%AF%D8%A7%D8%AF%D8%A7%D8%AA.yaml`},
		// Already ASCII, and a space inside a quoted string is legal, so the
		// extended form would add nothing.
		{"my config.yaml", `inline; filename="my config.yaml"`},
		// A quote is a legal filename character, so the header escapes it
		// rather than the validator refusing it - and the extended form comes
		// along, since it carries the exact bytes with no escaping to undo.
		{`quo"te.yaml`, `inline; filename="quo\"te.yaml"; filename*=UTF-8''quo%22te.yaml`},
		{`back\slash.yaml`, `inline; filename="back\\slash.yaml"; filename*=UTF-8''back%5Cslash.yaml`},
	} {
		if got := contentDisposition(tc.name); got != tc.want {
			t.Errorf("%q:\n got %s\nwant %s", tc.name, got, tc.want)
		}
	}
}

// TestContentDispositionCannotBeBrokenOut is the property the whole header
// rests on. These names cannot reach it - validateFilename refuses them - but
// the header has to hold even if one ever did, because it is the last thing
// standing between a filename and a response header of the caller's choosing.
func TestContentDispositionCannotBeBrokenOut(t *testing.T) {
	for _, hostile := range []string{
		`a".yaml`,
		"a\r\nX-Injected: 1.yaml",
		`a\".yaml`,
		"a\x00b.yaml",
		`"; attachment; filename="evil.sh`,
	} {
		got := contentDisposition(hostile)
		if strings.ContainsAny(got, "\r\n") {
			t.Errorf("%q put a line break in the header: %q", hostile, got)
		}
		if !strings.HasPrefix(got, `inline; filename="`) {
			t.Errorf("%q lost the header shape: %q", hostile, got)
		}
		// Every quote inside the field must be escaped, so exactly one
		// unescaped quote closes it and nothing after that is attacker text.
		field := strings.TrimPrefix(got, `inline; filename="`)
		if closing := unescapedQuote(field); closing != len(field)-1 && !strings.HasPrefix(field[closing:], `"; filename*=`) {
			t.Errorf("%q ended its quoted field early: %q", hostile, got)
		}
	}
}

// unescapedQuote reports where the quoted field actually ends: the first quote
// not preceded by an odd number of backslashes.
func unescapedQuote(field string) int {
	slashes := 0
	for i := 0; i < len(field); i++ {
		switch field[i] {
		case '\\':
			slashes++
		case '"':
			if slashes%2 == 0 {
				return i
			}
			slashes = 0
		default:
			slashes = 0
		}
	}
	return -1
}
