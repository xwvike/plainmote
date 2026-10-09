package auth

import "testing"

func TestParseRegistrationMode(t *testing.T) {
	for input, want := range map[string]RegistrationMode{
		"OPEN":     RegistrationOpen,
		" closed ": RegistrationClosed,
	} {
		got, err := ParseRegistrationMode(input)
		if err != nil || got != want {
			t.Errorf("ParseRegistrationMode(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"", "close", "opne", "private", "allowlist"} {
		if _, err := ParseRegistrationMode(input); err == nil {
			t.Errorf("ParseRegistrationMode(%q) accepted an invalid mode", input)
		}
	}
}

func TestRegistrationModeAllowsOnlyEligibleNewUsers(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode RegistrationMode
		want bool
	}{
		{"open", RegistrationOpen, true},
		{"closed", RegistrationClosed, false},
		{"invalid mode fails closed", RegistrationMode("typo"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.mode.AllowsNewUser(); got != tc.want {
				t.Fatalf("AllowsNewUser() = %v, want %v", got, tc.want)
			}
		})
	}
}
