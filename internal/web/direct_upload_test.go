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
	"github.com/jackc/pgx/v5"

	"plainmote/internal/auth"
	"plainmote/internal/blob"
	"plainmote/internal/store"
	"plainmote/internal/testsupport"
)

// directMemoryBlobs wraps MemoryBlobs with an httptest PUT server that enforces
// Content-Length and X-Amz-Checksum-Sha256 just like Cloudflare R2, exercising
// the full presign -> direct PUT -> server-side Copy -> commit pipeline.
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
		expectedSum := r.URL.Query().Get("sha256")
		if expectedSum != "" {
			gotHeader := r.Header.Get("X-Amz-Checksum-Sha256")
			sum := sha256.Sum256(data)
			actualB64 := base64.StdEncoding.EncodeToString(sum[:])
			if gotHeader != expectedSum || actualB64 != expectedSum {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte("BadDigest"))
				return
			}
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

func (d *directMemoryBlobs) PresignPut(_ context.Context, key string, _ int64, sha256B64 string, _ time.Duration) (blob.PresignedPut, error) {
	headers := map[string]string{}
	u := d.srv.URL + "/" + key
	if sha256B64 != "" {
		headers["X-Amz-Checksum-Sha256"] = sha256B64
		u += "?sha256=" + url.QueryEscape(sha256B64)
	}
	return blob.PresignedPut{URL: u, Headers: headers}, nil
}

func openDirectTestApp(t *testing.T) (*App, *store.Store, *directMemoryBlobs) {
	t.Helper()
	app, db, blobs, _ := openDirectTestAppWithURL(t)
	return app, db, blobs
}

func openDirectTestAppWithURL(t *testing.T) (*App, *store.Store, *directMemoryBlobs, string) {
	t.Helper()
	blobs := newDirectMemoryBlobs(t)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	dbURL := testsupport.DatabaseURL(t)
	db, err := store.Open(context.Background(), dbURL, key, blobs)
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
	return app, db, blobs, dbURL
}

func sha256Of(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type presignReply struct {
	URL        string            `json:"url"`
	Headers    map[string]string `json:"headers"`
	Token      string            `json:"token"`
	ResourceID string            `json:"resource_id"`
}

func doPresign(t *testing.T, h http.Handler, body map[string]any, cookies ...*http.Cookie) (*httptest.ResponseRecorder, presignReply) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/uploads/presign", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out presignReply
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func doDirectPut(t *testing.T, p presignReply, payload []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, p.URL, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("new put: %v", err)
	}
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do put: %v", err)
	}
	return resp
}

func TestDirectUploadEndToEnd(t *testing.T) {
	app, db, blobs := openDirectTestApp(t)
	h := app.Handler()

	// 1. Anonymous quick share direct upload: presign -> PUT -> commit -> result page 200 -> idempotent retry -> post-commit overwrite blocked.
	payload := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte{0x00, 0x01, 0x02, 0x03}, 1000)...)
	rec, presigned := doPresign(t, h, map[string]any{
		"purpose": "paste",
		"size":    len(payload),
		"sha256":  sha256Of(payload),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous presign: %d %s", rec.Code, rec.Body.String())
	}

	// Wrong bytes with mismatched SHA-256 must be rejected by storage before commit.
	badBytes := bytes.Repeat([]byte{0xff}, len(payload))
	badPut := doDirectPut(t, presigned, badBytes)
	badPut.Body.Close()
	if badPut.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 BadDigest for mismatched SHA-256, got %d", badPut.StatusCode)
	}

	goodPut := doDirectPut(t, presigned, payload)
	goodPut.Body.Close()
	if goodPut.StatusCode != http.StatusOK {
		t.Fatalf("direct PUT failed: %d", goodPut.StatusCode)
	}

	commitAnon := func() *httptest.ResponseRecorder {
		commitForm := url.Values{
			"ttl":             {"1d"},
			"upload_token":    {presigned.Token},
			"upload_filename": {"report.pdf"},
		}
		commitReq := httptest.NewRequest(http.MethodPost, "/paste", strings.NewReader(commitForm.Encode()))
		commitReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		commitReq.Header.Set("Accept", "application/json")
		commitReq.Header.Set(uploadJSONHeader, "1")
		commitRec := httptest.NewRecorder()
		h.ServeHTTP(commitRec, commitReq)
		return commitRec
	}

	commitRec := commitAnon()
	if commitRec.Code != http.StatusCreated {
		t.Fatalf("commit anonymous paste: %d %s", commitRec.Code, commitRec.Body.String())
	}
	var commitAns map[string]string
	_ = json.Unmarshal(commitRec.Body.Bytes(), &commitAns)
	if commitAns["location"] != "/paste/"+presigned.ResourceID {
		t.Fatalf("unexpected location: %#v", commitAns)
	}

	// P2 #1: Result page must return 200 (paste_claims written in same TX), not 410!
	resultReq := httptest.NewRequest(http.MethodGet, commitAns["location"], nil)
	resultRec := httptest.NewRecorder()
	h.ServeHTTP(resultRec, resultReq)
	if resultRec.Code != http.StatusOK {
		t.Fatalf("anonymous direct paste result page returned %d, want 200", resultRec.Code)
	}

	// P1 #1: Retrying commit with the same ticket must succeed idempotently and NOT delete the published blob!
	retryRec := commitAnon()
	if retryRec.Code != http.StatusCreated {
		t.Fatalf("idempotent retry commit failed: %d %s", retryRec.Code, retryRec.Body.String())
	}
	resAfterRetry, _, err := db.AnonymousPasteResult(context.Background(), presigned.ResourceID, time.Now().UTC())
	if err != nil {
		t.Fatalf("paste vanished after retry: %v", err)
	}
	storedAfterRetry, err := db.ReadContent(context.Background(), resAfterRetry)
	if err != nil || !bytes.Equal(storedAfterRetry, payload) {
		t.Fatalf("published paste body damaged after retry: err=%v len=%d", err, len(storedAfterRetry))
	}

	// P1 #2: Re-uploading to the presigned staging URL cannot mutate the published object.
	if strings.HasPrefix(resAfterRetry.ContentKey, "staging/") {
		t.Fatalf("published ContentKey must not be a staging key, got %q", resAfterRetry.ContentKey)
	}
	committedStagingKey := strings.TrimPrefix(strings.Split(presigned.URL, "?")[0], blobs.srv.URL+"/")
	overwritePut := doDirectPut(t, presigned, payload) // even re-putting to staging key does not affect published key
	overwritePut.Body.Close()
	// Also simulate tampering with the staging key directly in storage.
	_ = blobs.Put(context.Background(), committedStagingKey, bytes.NewReader(badBytes), int64(len(badBytes)))
	stillIntact, err := db.ReadContent(context.Background(), resAfterRetry)
	if err != nil || !bytes.Equal(stillIntact, payload) {
		t.Fatalf("published object was mutated via staging key!")
	}
	if _, err := blobs.Head(context.Background(), committedStagingKey); err != nil {
		t.Fatalf("expected re-PUT staging copy present before prune: %v", err)
	}

	// 2. Signed-in plain resource create + idempotent retry + concurrent seal race (P1 #4).
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

	textPayload := []byte("hello direct upload\nline 2\n")
	rec2, p2 := doPresign(t, h, map[string]any{
		"purpose": "resource_create",
		"size":    len(textPayload),
		"sha256":  sha256Of(textPayload),
		"csrf":    csrf,
	}, cookies...)
	if rec2.Code != http.StatusOK {
		t.Fatalf("resource_create presign: %d %s", rec2.Code, rec2.Body.String())
	}
	putResp2 := doDirectPut(t, p2, textPayload)
	putResp2.Body.Close()
	if putResp2.StatusCode != http.StatusOK {
		t.Fatalf("direct PUT resource: %d", putResp2.StatusCode)
	}

	commitCreate := func() *httptest.ResponseRecorder {
		createForm := url.Values{
			"csrf":            {csrf},
			"name":            {"Direct Note"},
			"filename":        {"notes.txt"},
			"upload_token":    {p2.Token},
			"upload_filename": {"notes.txt"},
		}
		createReq := httptest.NewRequest(http.MethodPost, "/resources/new", strings.NewReader(createForm.Encode()))
		createReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		createReq.Header.Set(uploadJSONHeader, "1")
		for _, c := range cookies {
			createReq.AddCookie(c)
		}
		createRec := httptest.NewRecorder()
		h.ServeHTTP(createRec, createReq)
		return createRec
	}
	if got := commitCreate(); got.Code != http.StatusCreated {
		t.Fatalf("commit resource_create: %d %s", got.Code, got.Body.String())
	}
	// Retry create must remain idempotent and keep body intact.
	if got := commitCreate(); got.Code != http.StatusCreated {
		t.Fatalf("retry commit resource_create: %d %s", got.Code, got.Body.String())
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

	// P1 #4: Start a plaintext resource_save upload, then seal the resource before committing -> must fail!
	plainUpdate := []byte("plain update after seal\n")
	recSave, pSave := doPresign(t, h, map[string]any{
		"purpose":     "resource_save",
		"resource_id": createdRes.ID,
		"size":        len(plainUpdate),
		"sha256":      sha256Of(plainUpdate),
		"csrf":        csrf,
	}, cookies...)
	if recSave.Code != http.StatusOK {
		t.Fatalf("resource_save presign: %d %s", recSave.Code, recSave.Body.String())
	}
	putSave := doDirectPut(t, pSave, plainUpdate)
	putSave.Body.Close()

	// Seal the resource concurrently before the plaintext save commits.
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
	sealedBlob := append([]byte("PMr1"), bytes.Repeat([]byte{0xab}, 64)...)
	sealedMeta := append([]byte("PMm1"), bytes.Repeat([]byte{0xcd}, 40)...)
	sealedKey := bytes.Repeat([]byte{0xef}, 60)
	if _, err := db.SealResource(context.Background(), user.ID, createdRes.ID, createdRes.Version,
		store.SealedPart{Content: sealedBlob, Meta: sealedMeta}, nil, sealedKey); err != nil {
		t.Fatalf("seal resource: %v", err)
	}

	saveForm := url.Values{
		"csrf":            {csrf},
		"name":            {"Overwrite"},
		"filename":        {"notes.txt"},
		"base_version":    {"1"},
		"upload_token":    {pSave.Token},
		"upload_filename": {"notes.txt"},
	}
	saveReq := httptest.NewRequest(http.MethodPost, "/resources/"+createdRes.ID, strings.NewReader(saveForm.Encode()))
	saveReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	saveReq.Header.Set(uploadJSONHeader, "1")
	for _, c := range cookies {
		saveReq.AddCookie(c)
	}
	saveRec := httptest.NewRecorder()
	h.ServeHTTP(saveRec, saveReq)
	if saveRec.Code == http.StatusOK {
		t.Fatalf("plaintext direct save must be rejected after resource was sealed")
	}
	afterSeal, err := db.ResourceForOwner(context.Background(), user.ID, createdRes.ID)
	if err != nil || !afterSeal.Sealed() {
		t.Fatalf("resource should remain sealed after refused plaintext direct save: err=%v sealed=%v", err, afterSeal.Sealed())
	}

	// 3. Sealed resource create via direct upload + idempotent retry.
	sealedID := uuid.NewString()
	rec3, p3 := doPresign(t, h, map[string]any{
		"purpose":     "sealed_create",
		"size":        len(sealedBlob),
		"sha256":      sha256Of(sealedBlob),
		"resource_id": sealedID,
		"csrf":        csrf,
	}, cookies...)
	if rec3.Code != http.StatusOK {
		t.Fatalf("sealed_create presign: %d %s", rec3.Code, rec3.Body.String())
	}
	putResp3 := doDirectPut(t, p3, sealedBlob)
	putResp3.Body.Close()
	if putResp3.StatusCode != http.StatusOK {
		t.Fatalf("direct PUT sealed: %d", putResp3.StatusCode)
	}

	commitSealed := func() *httptest.ResponseRecorder {
		var mp bytes.Buffer
		mw := newMultipartForm(&mp, map[string]string{
			"csrf":         csrf,
			"id":           sealedID,
			"sealed_key":   base64.RawURLEncoding.EncodeToString(sealedKey),
			"meta":         base64.RawURLEncoding.EncodeToString(sealedMeta),
			"upload_token": p3.Token,
		})
		sealedReq := httptest.NewRequest(http.MethodPost, "/resources/sealed", &mp)
		sealedReq.Header.Set("Content-Type", mw)
		for _, c := range cookies {
			sealedReq.AddCookie(c)
		}
		sealedRec := httptest.NewRecorder()
		h.ServeHTTP(sealedRec, sealedReq)
		return sealedRec
	}
	if got := commitSealed(); got.Code != http.StatusCreated {
		t.Fatalf("commit sealed_create: %d %s", got.Code, got.Body.String())
	}
	if got := commitSealed(); got.Code != http.StatusCreated {
		t.Fatalf("retry commit sealed_create: %d %s", got.Code, got.Body.String())
	}
	sealedRes, err := db.ResourceForOwner(context.Background(), user.ID, sealedID)
	if err != nil {
		t.Fatalf("load sealed resource: %v", err)
	}
	gotSealed, err := db.ReadContent(context.Background(), sealedRes)
	if err != nil || !bytes.Equal(gotSealed, sealedBlob) {
		t.Fatalf("sealed content damaged after retry: err=%v", err)
	}

	// P1 #3: Uncommitted staged uploads reserve quota and are pruned on expiry.
	recOrphan, pOrphan := doPresign(t, h, map[string]any{
		"purpose": "resource_create",
		"size":    512,
		"sha256":  sha256Of(bytes.Repeat([]byte("a"), 512)),
		"csrf":    csrf,
	}, cookies...)
	if recOrphan.Code != http.StatusOK {
		t.Fatalf("orphan presign: %d %s", recOrphan.Code, recOrphan.Body.String())
	}
	putOrphan := doDirectPut(t, pOrphan, bytes.Repeat([]byte("a"), 512))
	putOrphan.Body.Close()
	orphanStagingKey := strings.TrimPrefix(strings.Split(pOrphan.URL, "?")[0], blobs.srv.URL+"/")
	if _, err := blobs.Head(context.Background(), orphanStagingKey); err != nil {
		t.Fatalf("expected staging object present before prune: %v", err)
	}
	if _, err := db.Prune(context.Background(), 0, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatalf("prune staged uploads: %v", err)
	}
	if _, err := blobs.Head(context.Background(), orphanStagingKey); err != blob.ErrNotFound {
		t.Fatalf("expected expired staging object deleted by Prune, got err=%v", err)
	}
	// P2 #1: Re-PUT staging copy for an already-committed upload must also be
	// swept after the presigned URL expires, while the published copy stays intact.
	if _, err := blobs.Head(context.Background(), committedStagingKey); err != blob.ErrNotFound {
		t.Fatalf("expected re-PUT staging copy %q deleted after presign expiry, got err=%v", committedStagingKey, err)
	}
	if stillIntact, err := db.ReadContent(context.Background(), resAfterRetry); err != nil || !bytes.Equal(stillIntact, payload) {
		t.Fatalf("published paste damaged after pruning committed staging key: err=%v", err)
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

	// P2 #5: API link listing omits incomplete url for key-in-fragment sealed links
	// and provides base_url + owner_key + sealed=true instead.
	fragLinkID := uuid.NewString()
	ownerKey := bytes.Repeat([]byte{0x44}, 60)
	linkKey := bytes.Repeat([]byte{0x55}, 60)
	if _, err := db.CreateSealedShare(context.Background(), user.ID, sealedID, fragLinkID, "frag", 0, 0, linkKey, ownerKey); err != nil {
		t.Fatalf("create sealed fragment share: %v", err)
	}
	codeLinkID := uuid.NewString()
	codeSealedKey := bytes.Repeat([]byte{0x66}, 16+60)
	codeOwnerKey := bytes.Repeat([]byte{0x77}, 12+4+16)
	if _, err := db.CreateSealedShare(context.Background(), user.ID, sealedID, codeLinkID, "code", 0, 0, codeSealedKey, codeOwnerKey); err != nil {
		t.Fatalf("create sealed code share: %v", err)
	}
	apiTok := issueToken(t, db, user, store.TokenScopeRead)
	linksRec := callAPI(t, app, apiCall{method: http.MethodGet, path: apiPrefix + "resources/" + sealedID + "/links", token: apiTok})
	if linksRec.Code != http.StatusOK {
		t.Fatalf("api list sealed links: %d %s", linksRec.Code, linksRec.Body.String())
	}
	var linksOut struct {
		Links []apiLink `json:"links"`
	}
	if err := json.Unmarshal(linksRec.Body.Bytes(), &linksOut); err != nil {
		t.Fatalf("decode links: %v", err)
	}
	if len(linksOut.Links) != 2 {
		t.Fatalf("expected 2 sealed links, got %d", len(linksOut.Links))
	}
	for _, l := range linksOut.Links {
		if !l.Sealed || l.OwnerKey == "" {
			t.Fatalf("sealed link missing metadata: %+v", l)
		}
		if l.ID == fragLinkID {
			if l.URL != "" || l.BaseURL == "" || l.CodeLink {
				t.Fatalf("fragment-key sealed link must omit url and set base_url: %+v", l)
			}
		}
		if l.ID == codeLinkID {
			if l.URL == "" || l.BaseURL != "" || !l.CodeLink {
				t.Fatalf("code sealed link must include url and code_link=true: %+v", l)
			}
		}
	}
}

func newMultipartForm(buf *bytes.Buffer, fields map[string]string) string {
	w := multipart.NewWriter(buf)
	for k, v := range fields {
		_ = w.WriteField(k, v)
	}
	_ = w.Close()
	return w.FormDataContentType()
}

// TestDirectUploadFallbackReusesQuotaReservation verifies that when presign
// succeeds and reserves the account's last resource/storage slot, but direct
// PUT fails (e.g. CORS or network error), falling back to a normal form upload
// with fallback_upload_token reuses that reservation in one transaction.
func TestDirectUploadFallbackReusesQuotaReservation(t *testing.T) {
	app, db, _, dbURL := openDirectTestAppWithURL(t)
	h := app.Handler()
	ctx := context.Background()

	user, err := db.UpsertUser(ctx, "2002", "quotauser", "Quota User", "")
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	// Tighten default plan to exactly 1 resource and just enough storage for one upload.
	payload := bytes.Repeat([]byte("q"), 1024)
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect test schema: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `UPDATE plans SET max_resources = 1, max_storage = $1 WHERE is_default`, int64(len(payload))); err != nil {
		t.Fatalf("tighten default plan: %v", err)
	}

	sessionToken, csrf, _, err := db.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	cookies := []*http.Cookie{
		{Name: sessionCookie, Value: sessionToken},
		{Name: csrfCookie, Value: csrf},
	}

	// Step 1: Presign reserves the sole resource slot and all 1024 bytes.
	rec, presigned := doPresign(t, h, map[string]any{
		"purpose": "resource_create",
		"size":    len(payload),
		"sha256":  sha256Of(payload),
		"csrf":    csrf,
	}, cookies...)
	if rec.Code != http.StatusOK {
		t.Fatalf("presign: %d %s", rec.Code, rec.Body.String())
	}

	// Step 2: Direct PUT fails (skipped). A normal upload WITHOUT fallback_upload_token
	// would be rejected by the reservation.
	postFallback := func(fallbackToken string) *httptest.ResponseRecorder {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		_ = mw.WriteField("csrf", csrf)
		_ = mw.WriteField("name", "Fallback File")
		_ = mw.WriteField("filename", "fallback.txt")
		if fallbackToken != "" {
			_ = mw.WriteField("fallback_upload_token", fallbackToken)
		}
		part, _ := mw.CreateFormFile("upload", "fallback.txt")
		_, _ = part.Write(payload)
		_ = mw.Close()
		req := httptest.NewRequest(http.MethodPost, "/resources/new", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set(uploadJSONHeader, "1")
		for _, c := range cookies {
			req.AddCookie(c)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	withoutToken := postFallback("")
	if withoutToken.Code == http.StatusCreated {
		t.Fatalf("expected upload without fallback token to be refused while reservation is active")
	}

	// Step 3: Fallback carrying fallback_upload_token reuses the reservation atomically.
	withToken := postFallback(presigned.Token)
	if withToken.Code != http.StatusCreated {
		t.Fatalf("fallback upload with ticket failed: %d %s", withToken.Code, withToken.Body.String())
	}

	// Step 4: Replaying the same fallback token must not bypass the 1-resource limit.
	replay := postFallback(presigned.Token)
	if replay.Code == http.StatusCreated {
		t.Fatalf("replayed fallback_upload_token must not bypass resource quota")
	}
}
