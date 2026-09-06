package config

import "testing"

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
