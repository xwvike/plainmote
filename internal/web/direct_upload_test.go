package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"plainmote/internal/auth"
	"plainmote/internal/blob"
	"plainmote/internal/store"
	"plainmote/internal/testsupport"
)

// directMemoryBlobs wraps MemoryBlobs with an httptest PUT server to exercise
// the full presign -> direct PUT -> commit round trip without an external S3.
type directMemoryBlobs struct {
	*testsupport.MemoryBlobs
	srv *httptest.Server
}

var _ blob.DirectStore = (*directMemoryBlobs)(nil)

func newDirectMemoryBlobs(t *testing.T) *directMemoryBlobs {
	t.Helper()
	mb := testsupport.NewMemoryBlobs()
	d := &directMemoryBlobs{MemoryBlobs: mb}
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/")
		data, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if err := mb.Put(r.Context(), key, bytes.NewReader(data), int64(len(data))); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(d.srv.Close)
	return d
}

func (d *directMemoryBlobs) PresignPut(_ context.Context, key string, _ int64, _ time.Duration) (string, error) {
	return d.srv.URL + "/" + key, nil
}

func openDirectTestApp(t *testing.T) (*App, *store.Store, *directMemoryBlobs) {
	t.Helper()
	blobs := newDirectMemoryBlobs(t)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	db, err := store.Open(context.Background(), testsupport.DatabaseURL(t), key, blobs)
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(db.Close)
	app := New(Config{
		PublicURL:        "https://plainmote.test",
		SessionTTL:       24 * time.Hour,
		MaxContent:       10 << 20,
		AnonymousEnabled: true,
		DirectUpload:     true,
		BlobEndpoint:     blobs.srv.URL,
	}, db, auth.NewGitHub("", ""))
	return app, db, blobs
}

func sha256Of(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestDirectUploadEndToEnd(t *testing.T) {
	app, db, blobs := openDirectTestApp(t)
	h := app.Handler()

	// 1. Anonymous quick share direct upload (presign -> PUT -> commit).
	payload := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte{0x00, 0x01, 0x02, 0x03}, 1000)...)
	presignReq := httptest.NewRequest(http.MethodPost, "/uploads/presign", strings.NewReader(
		`{"purpose":"paste","size":`+jsonInt(len(payload))+`}`,
	))
	presignReq.Header.Set("Content-Type", "application/json")
	presignRec := httptest.NewRecorder()
	h.ServeHTTP(presignRec, presignReq)
	if presignRec.Code != http.StatusOK {
		t.Fatalf("anonymous presign: %d %s", presignRec.Code, presignRec.Body.String())
	}
	var presigned struct {
		URL        string `json:"url"`
		Token      string `json:"token"`
		ResourceID string `json:"resource_id"`
	}
	if err := json.Unmarshal(presignRec.Body.Bytes(), &presigned); err != nil {
		t.Fatalf("decode presign response: %v", err)
	}
	putReq, err := http.NewRequest(http.MethodPut, presigned.URL, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("new put request: %v", err)
	}
	putResp, err := http.DefaultClient.Do(putReq)
	if err != nil || putResp.StatusCode != http.StatusOK {
		t.Fatalf("direct PUT failed: err=%v resp=%v", err, putResp)
	}
	putResp.Body.Close()

	commitForm := url.Values{
		"ttl":             {"10m"},
		"upload_token":    {presigned.Token},
		"upload_filename": {"report.pdf"},
		"upload_sha256":   {sha256Of(payload)},
	}
	commitReq := httptest.NewRequest(http.MethodPost, "/paste", strings.NewReader(commitForm.Encode()))
	commitReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	commitReq.Header.Set("Accept", "application/json")
	commitReq.Header.Set(uploadJSONHeader, "1")
	commitRec := httptest.NewRecorder()
	h.ServeHTTP(commitRec, commitReq)
	if commitRec.Code != http.StatusCreated {
		t.Fatalf("commit anonymous paste: %d %s", commitRec.Code, commitRec.Body.String())
	}
	var commitAns map[string]string
	_ = json.Unmarshal(commitRec.Body.Bytes(), &commitAns)
	if commitAns["location"] != "/paste/"+presigned.ResourceID {
		t.Fatalf("unexpected location: %#v", commitAns)
	}

	// 2. Signed-in plain resource create & save via direct upload.
	user, err := db.UpsertUser(context.Background(), "1001", "alice", "Alice", "")
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	sessionToken, csrf, _, err := db.CreateSession(context.Background(), user.ID, time.Hour)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	cookies := []*http.Cookie{
		{Name: sessionCookie, Value: sessionToken},
		{Name: csrfCookie, Value: csrf},
	}

	// Create resource via presign + PUT + commit.
	textPayload := []byte("hello direct upload\nline 2\n")
	reqBody, _ := json.Marshal(map[string]any{
		"purpose": "resource_create",
		"size":    len(textPayload),
		"csrf":    csrf,
	})
	pre2 := httptest.NewRequest(http.MethodPost, "/uploads/presign", bytes.NewReader(reqBody))
	for _, c := range cookies {
		pre2.AddCookie(c)
	}
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, pre2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("resource_create presign: %d %s", rec2.Code, rec2.Body.String())
	}
	var p2 struct {
		URL        string `json:"url"`
		Token      string `json:"token"`
		ResourceID string `json:"resource_id"`
	}
	_ = json.Unmarshal(rec2.Body.Bytes(), &p2)
	putReq2, _ := http.NewRequest(http.MethodPut, p2.URL, bytes.NewReader(textPayload))
	putResp2, err := http.DefaultClient.Do(putReq2)
	if err != nil || putResp2.StatusCode != http.StatusOK {
		t.Fatalf("direct PUT resource: err=%v resp=%v", err, putResp2)
	}
	putResp2.Body.Close()

	createForm := url.Values{
		"csrf":            {csrf},
		"name":            {"Direct Note"},
		"filename":        {"notes.txt"},
		"upload_token":    {p2.Token},
		"upload_filename": {"notes.txt"},
		"upload_sha256":   {sha256Of(textPayload)},
	}
	createReq := httptest.NewRequest(http.MethodPost, "/resources/new", strings.NewReader(createForm.Encode()))
	createReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	createReq.Header.Set(uploadJSONHeader, "1")
	for _, c := range cookies {
		createReq.AddCookie(c)
	}
	createRec := httptest.NewRecorder()
	h.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("commit resource_create: %d %s", createRec.Code, createRec.Body.String())
	}
	createdRes, err := db.ResourceForOwner(context.Background(), user.ID, p2.ResourceID)
	if err != nil {
		t.Fatalf("load created resource: %v", err)
	}
	if createdRes.ContentSize != int64(len(textPayload)) || createdRes.ContentSHA256 != sha256Of(textPayload) {
		t.Fatalf("unexpected created resource: %+v", createdRes)
	}
	gotBody, err := db.ReadContent(context.Background(), createdRes)
	if err != nil || !bytes.Equal(gotBody, textPayload) {
		t.Fatalf("stored body mismatch: err=%v got=%q", err, gotBody)
	}

	// 3. Sealed resource create via direct upload (checks 4-byte PMr1 magic via Range).
	if _, err := db.CreateKeyring(context.Background(), user.ID, store.Keyring{
		KDF:               store.KeyringKDF,
		Salt:              bytes.Repeat([]byte{1}, 16),
		Iterations:        600000,
		WrappedByPassword: bytes.Repeat([]byte{2}, 60),
		WrappedByRecovery: bytes.Repeat([]byte{3}, 60),
		LockMinutes:       30,
	}, time.Now().UTC()); err != nil {
		t.Fatalf("create keyring: %v", err)
	}
	sealedID := uuid.NewString()
	sealedBlob := append([]byte("PMr1"), bytes.Repeat([]byte{0xab}, 64)...)
	sealedMeta := append([]byte("PMm1"), bytes.Repeat([]byte{0xcd}, 40)...)
	sealedKey := bytes.Repeat([]byte{0xef}, 60)

	reqBody3, _ := json.Marshal(map[string]any{
		"purpose":     "sealed_create",
		"size":        len(sealedBlob),
		"resource_id": sealedID,
		"csrf":        csrf,
	})
	pre3 := httptest.NewRequest(http.MethodPost, "/uploads/presign", bytes.NewReader(reqBody3))
	for _, c := range cookies {
		pre3.AddCookie(c)
	}
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, pre3)
	if rec3.Code != http.StatusOK {
		t.Fatalf("sealed_create presign: %d %s", rec3.Code, rec3.Body.String())
	}
	var p3 struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec3.Body.Bytes(), &p3)
	putReq3, _ := http.NewRequest(http.MethodPut, p3.URL, bytes.NewReader(sealedBlob))
	putResp3, err := http.DefaultClient.Do(putReq3)
	if err != nil || putResp3.StatusCode != http.StatusOK {
		t.Fatalf("direct PUT sealed: err=%v resp=%v", err, putResp3)
	}
	putResp3.Body.Close()

	// Commit sealed create via multipart (just like sealed.js does).
	var mp bytes.Buffer
	mw := newMultipartForm(&mp, map[string]string{
		"csrf":          csrf,
		"id":            sealedID,
		"sealed_key":    base64.RawURLEncoding.EncodeToString(sealedKey),
		"meta":          base64.RawURLEncoding.EncodeToString(sealedMeta),
		"upload_token":  p3.Token,
		"upload_sha256": sha256Of(sealedBlob),
	})
	sealedReq := httptest.NewRequest(http.MethodPost, "/resources/sealed", &mp)
	sealedReq.Header.Set("Content-Type", mw)
	for _, c := range cookies {
		sealedReq.AddCookie(c)
	}
	sealedRec := httptest.NewRecorder()
	h.ServeHTTP(sealedRec, sealedReq)
	if sealedRec.Code != http.StatusCreated {
		t.Fatalf("commit sealed_create: %d %s", sealedRec.Code, sealedRec.Body.String())
	}
	if blobs.Count() < 3 {
		t.Fatalf("expected 3 staged blobs in store, got %d", blobs.Count())
	}

	// 4. Tampered upload_token is rejected.
	badForm := url.Values{"ttl": {"10m"}, "upload_token": {"not-a-valid-token"}}
	badReq := httptest.NewRequest(http.MethodPost, "/paste", strings.NewReader(badForm.Encode()))
	badReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	badReq.Header.Set(uploadJSONHeader, "1")
	badRec := httptest.NewRecorder()
	h.ServeHTTP(badRec, badReq)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for tampered upload_token, got %d %s", badRec.Code, badRec.Body.String())
	}
}

func jsonInt(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func newMultipartForm(buf *bytes.Buffer, fields map[string]string) string {
	w := multipart.NewWriter(buf)
	for k, v := range fields {
		_ = w.WriteField(k, v)
	}
	_ = w.Close()
	return w.FormDataContentType()
}
