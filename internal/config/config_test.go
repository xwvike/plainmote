package config

import (
	"testing"

	"plainmote/internal/auth"
)

func setRequiredEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://plainmote:secret@postgres/plainmote")
	t.Setenv("PLAINMOTE_BLOB_ENDPOINT", "https://example.r2.cloudflarestorage.com")
	t.Setenv("PLAINMOTE_BLOB_BUCKET", "plainmote")
	t.Setenv("PLAINMOTE_BLOB_ACCESS_KEY", "access")
	t.Setenv("PLAINMOTE_BLOB_SECRET_KEY", "secret")
	t.Setenv("PLAINMOTE_PUBLIC_URL", "https://config.example.com")
	t.Setenv("PLAINMOTE_TOKEN_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("GITHUB_CLIENT_ID", "client")
	t.Setenv("GITHUB_CLIENT_SECRET", "secret")
	t.Setenv("GITHUB_ALLOWED_IDS", "100")
	t.Setenv("PLAINMOTE_REGISTRATION_MODE", "")
}

func TestLoadReadsEnvironment(t *testing.T) {
	setRequiredEnvironment(t)
	t.Setenv("PLAINMOTE_ALLOW_PRIVATE_UPSTREAM", "true")
	t.Setenv("PLAINMOTE_TRUSTED_PROXIES", "127.0.0.1, 172.18.0.0/16, 127.0.0.1/32")
	t.Setenv("PLAINMOTE_SESSION_TTL", "24h")
	t.Setenv("PLAINMOTE_MAX_CONTENT_MIB", "8")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabaseURL == "" || cfg.BlobBucket != "plainmote" || cfg.MaxContent != 8<<20 {
		t.Fatalf("environment was not loaded: %+v", cfg)
	}
	if !cfg.AllowPrivateUpstream || cfg.SessionTTL.Hours() != 24 {
		t.Fatalf("typed values were not parsed: %+v", cfg)
	}
	if len(cfg.TrustedProxies) != 2 {
		t.Fatalf("got %d trusted proxies, want 2", len(cfg.TrustedProxies))
	}
	if cfg.RegistrationMode != auth.RegistrationAllowlist || !cfg.AllowedIDs["100"] {
		t.Fatalf("default registration policy was not loaded: mode=%q ids=%v", cfg.RegistrationMode, cfg.AllowedIDs)
	}
}

func TestLoadRegistrationPolicy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    string
		ids     string
		want    auth.RegistrationMode
		wantIDs []string
		wantErr bool
	}{
		{name: "open without allowlist", mode: "open", want: auth.RegistrationOpen},
		{name: "closed without allowlist", mode: "closed", want: auth.RegistrationClosed},
		{name: "allowlist", mode: "allowlist", ids: "100, 00200,100", want: auth.RegistrationAllowlist, wantIDs: []string{"100", "200"}},
		{name: "allowlist requires IDs", mode: "allowlist", wantErr: true},
		{name: "invalid mode", mode: "opne", ids: "100", wantErr: true},
		{name: "wildcard is not an ID", mode: "allowlist", ids: "*", wantErr: true},
		{name: "login is not an ID", mode: "allowlist", ids: "alice", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setRequiredEnvironment(t)
			t.Setenv("PLAINMOTE_REGISTRATION_MODE", tc.mode)
			t.Setenv("GITHUB_ALLOWED_IDS", tc.ids)
			cfg, err := Load()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("invalid registration policy was accepted: %+v", cfg)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.RegistrationMode != tc.want {
				t.Fatalf("mode = %q, want %q", cfg.RegistrationMode, tc.want)
			}
			if len(cfg.AllowedIDs) != len(tc.wantIDs) {
				t.Fatalf("allowed IDs = %v, want %v", cfg.AllowedIDs, tc.wantIDs)
			}
			for _, id := range tc.wantIDs {
				if !cfg.AllowedIDs[id] {
					t.Errorf("allowed IDs %v do not contain %q", cfg.AllowedIDs, id)
				}
			}
		})
	}
}

func TestLoadFailsFastOnMissingRequiredValue(t *testing.T) {
	setRequiredEnvironment(t)
	t.Setenv("DATABASE_URL", "")
	if _, err := Load(); err == nil {
		t.Fatal("missing DATABASE_URL was accepted")
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	for name, value := range map[string]string{
		"PLAINMOTE_ALLOW_PRIVATE_UPSTREAM": "perhaps",
		"PLAINMOTE_TRUSTED_PROXIES":        "docker-network",
		"PLAINMOTE_SESSION_TTL":            "forever",
		"PLAINMOTE_MAX_CONTENT_MIB":        "zero",
	} {
		t.Run(name, func(t *testing.T) {
			setRequiredEnvironment(t)
			t.Setenv(name, value)
			if _, err := Load(); err == nil {
				t.Fatalf("invalid %s was accepted", name)
			}
		})
	}
}
