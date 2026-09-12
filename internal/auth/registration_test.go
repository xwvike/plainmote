package auth

import "testing"

func TestParseRegistrationMode(t *testing.T) {
	for input, want := range map[string]RegistrationMode{
		"open":      RegistrationOpen,
		"ALLOWLIST": RegistrationAllowlist,
		" closed ":  RegistrationClosed,
	} {
		got, err := ParseRegistrationMode(input)
		if err != nil || got != want {
			t.Errorf("ParseRegistrationMode(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"", "close", "opne", "private"} {
		if _, err := ParseRegistrationMode(input); err == nil {
			t.Errorf("ParseRegistrationMode(%q) accepted an invalid mode", input)
		}
	}
}

func TestRegistrationModeAllowsOnlyEligibleNewUsers(t *testing.T) {
	allowed := map[string]bool{"100": true}
	for _, tc := range []struct {
		name string
		mode RegistrationMode
		id   string
		want bool
	}{
		{"open", RegistrationOpen, "999", true},
		{"allowlisted", RegistrationAllowlist, "100", true},
		{"not allowlisted", RegistrationAllowlist, "999", false},
		{"closed", RegistrationClosed, "100", false},
		{"invalid mode fails closed", RegistrationMode("typo"), "100", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.mode.AllowsNewUser(tc.id, allowed); got != tc.want {
				t.Fatalf("AllowsNewUser() = %v, want %v", got, tc.want)
			}
		})
	}
}
