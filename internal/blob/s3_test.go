package blob

import (
	"os"
	"strings"
	"testing"
)

// TestS3MeetsTheStoreContract runs the storage contract against a real bucket.
// It skips unless a bucket is configured because an in-memory fake would only
// test the fake.
//
//	PLAINMOTE_TEST_BLOB_ENDPOINT=https://<account>.r2.cloudflarestorage.com \
//	PLAINMOTE_TEST_BLOB_BUCKET=plainmote-test \
//	PLAINMOTE_TEST_BLOB_ACCESS_KEY=... \
//	PLAINMOTE_TEST_BLOB_SECRET_KEY=... \
//	go test ./internal/blob/ -run TestS3
func TestS3MeetsTheStoreContract(t *testing.T) {
	cfg := S3Config{
		Endpoint:  os.Getenv("PLAINMOTE_TEST_BLOB_ENDPOINT"),
		Bucket:    os.Getenv("PLAINMOTE_TEST_BLOB_BUCKET"),
		AccessKey: os.Getenv("PLAINMOTE_TEST_BLOB_ACCESS_KEY"),
		SecretKey: os.Getenv("PLAINMOTE_TEST_BLOB_SECRET_KEY"),
		Region:    os.Getenv("PLAINMOTE_TEST_BLOB_REGION"),
	}
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		t.Skip("no test bucket configured; set PLAINMOTE_TEST_BLOB_* to run this")
	}
	store, err := NewS3(cfg)
	if err != nil {
		t.Fatal(err)
	}
	RunStoreContract(t, store)
}

// The configuration is checked before anything reaches the network, so a
// missing setting is a clear error rather than a confusing auth failure.
func TestS3ConfigIsChecked(t *testing.T) {
	full := S3Config{Endpoint: "https://x.r2.cloudflarestorage.com", Bucket: "b", AccessKey: "k", SecretKey: "s"}
	if _, err := NewS3(full); err != nil {
		t.Fatalf("a complete config should be accepted: %v", err)
	}
	for name, broken := range map[string]S3Config{
		"no endpoint":   {Bucket: "b", AccessKey: "k", SecretKey: "s"},
		"no bucket":     {Endpoint: "https://x", AccessKey: "k", SecretKey: "s"},
		"no access key": {Endpoint: "https://x", Bucket: "b", SecretKey: "s"},
		"no secret key": {Endpoint: "https://x", Bucket: "b", AccessKey: "k"},
	} {
		if _, err := NewS3(broken); err == nil {
			t.Errorf("%s should have been rejected", name)
		}
	}
}

// An empty key would address the bucket itself, so it never reaches the wire.
func TestS3RefusesEmptyKeys(t *testing.T) {
	store, err := NewS3(S3Config{Endpoint: "https://x", Bucket: "b", AccessKey: "k", SecretKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if err := store.Put(ctx, "", strings.NewReader("x"), 1); err == nil {
		t.Error("an empty key should be refused")
	}
	if _, _, err := store.Open(ctx, ""); err == nil {
		t.Error("an empty key should be refused")
	}
	if _, _, err := store.OpenRange(ctx, "", 0, 0); err == nil {
		t.Error("an empty range key should be refused")
	}
	if _, _, err := store.OpenRange(ctx, "x", 2, 1); err == nil {
		t.Error("an invalid range should be refused")
	}
	if err := store.Delete(ctx, ""); err == nil {
		t.Error("an empty key should be refused")
	}
}
