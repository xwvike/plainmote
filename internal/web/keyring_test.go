package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"plainmote/internal/store"
)

func keyringForm(action string) url.Values {
	encode := base64.RawURLEncoding.EncodeToString
	return url.Values{
		"action": {action}, "kdf": {store.KeyringKDF}, "iterations": {"600000"},
		"salt": {encode(bytes.Repeat([]byte{1}, 16))}, "wrapped_by_password": {encode(bytes.Repeat([]byte{2}, 60))},
		"wrapped_by_recovery": {encode(bytes.Repeat([]byte{3}, 60))},
	}
}

// The keyring endpoint is the account's own: a session reads it, a session
// with its CSRF token from this site writes it, and a stale write is refused.
func TestKeyringEndpoint(t *testing.T) {
	db, user, _ := testDatabase(t)
	client := newVersionClient(t, db, user)
	app := client.app

	if got := client.do(http.MethodGet, accountKeyringPath, nil); got.Code != http.StatusNotFound {
		t.Fatalf("no keyring yet: %d", got.Code)
	}
	// Signed out: nothing.
	anonymous := httptestRequest(app, http.MethodGet, accountKeyringPath, nil, nil)
	if anonymous.Code != http.StatusSeeOther || !strings.HasPrefix(anonymous.Header().Get("Location"), "/login") {
		t.Fatalf("signed out: %d", anonymous.Code)
	}
	// Without the CSRF token, or from another site: refused.
	form := keyringForm("create")
	noToken := httptestRequest(app, http.MethodPost, accountKeyringPath, form, []*http.Cookie{{Name: sessionCookie, Value: client.session}})
	if noToken.Code != http.StatusForbidden {
		t.Fatalf("without the token: %d", noToken.Code)
	}
	form.Set("csrf", client.csrf)
	crossSite := httptestRequest(app, http.MethodPost, accountKeyringPath, form,
		[]*http.Cookie{{Name: sessionCookie, Value: client.session}, {Name: csrfCookie, Value: client.csrf}}, "Origin", "https://evil.test")
	if crossSite.Code != http.StatusForbidden {
		t.Fatalf("from another site: %d", crossSite.Code)
	}

	created := client.do(http.MethodPost, accountKeyringPath, keyringForm("create"))
	var ring keyringView
	if err := json.Unmarshal(created.Body.Bytes(), &ring); err != nil || created.Code != http.StatusOK || ring.Version != 1 || ring.LockMinutes != 15 ||
		ring.Salt != base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 16)) || created.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	if again := client.do(http.MethodPost, accountKeyringPath, keyringForm("create")); again.Code != http.StatusBadRequest ||
		!strings.Contains(again.Body.String(), "already has a master password") {
		t.Fatalf("a second keyring: %d %s", again.Code, again.Body.String())
	}
	read := client.do(http.MethodGet, accountKeyringPath, nil)
	if read.Code != http.StatusOK || !strings.Contains(read.Body.String(), `"wrapped_by_recovery"`) {
		t.Fatalf("read: %d %s", read.Code, read.Body.String())
	}

	change := keyringForm("password")
	change.Set("version", "0")
	if stale := client.do(http.MethodPost, accountKeyringPath, change); stale.Code != http.StatusConflict {
		t.Fatalf("a stale change: %d %s", stale.Code, stale.Body.String())
	}
	change.Set("version", "1")
	change.Set("wrapped_by_password", "not base64!")
	if bad := client.do(http.MethodPost, accountKeyringPath, change); bad.Code != http.StatusBadRequest {
		t.Fatalf("a malformed key: %d", bad.Code)
	}
	change.Set("wrapped_by_password", base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{9}, 60)))
	if ok := client.do(http.MethodPost, accountKeyringPath, change); ok.Code != http.StatusOK || !strings.Contains(ok.Body.String(), `"version":2`) {
		t.Fatalf("a change: %d %s", ok.Code, ok.Body.String())
	}

	// The auto-lock time is a plain form that comes back to the page.
	if lock := client.do(http.MethodPost, accountKeyringPath, url.Values{"action": {"lock"}, "lock_minutes": {"60"}}); lock.Code != http.StatusSeeOther {
		t.Fatalf("lock time: %d", lock.Code)
	}
	if lock := client.do(http.MethodPost, accountKeyringPath, url.Values{"action": {"lock"}, "lock_minutes": {"7"}}); lock.Code != http.StatusSeeOther {
		t.Fatalf("an unsupported lock time: %d", lock.Code)
	}
	if got, _ := db.Keyring(context.Background(), user.ID); got.LockMinutes != 60 {
		t.Fatalf("lock time kept: %d", got.LockMinutes)
	}

	// Another account sees only its own - none.
	other, err := db.UpsertUser(context.Background(), "200", "bob", "Bob", "")
	if err != nil {
		t.Fatal(err)
	}
	otherClient := newVersionClient(t, db, other)
	if got := otherClient.do(http.MethodGet, accountKeyringPath, nil); got.Code != http.StatusNotFound {
		t.Fatalf("another account: %d %s", got.Code, got.Body.String())
	}
}

// The account page shows the state the keyring is in, and every page tells
// lock.js whose key may stay - and, signed out, that none may.
func TestKeyringOnTheAccountPage(t *testing.T) {
	db, user, _ := testDatabase(t)
	client := newVersionClient(t, db, user)
	page := client.do(http.MethodGet, accountPath, nil).Body.String()
	if !strings.Contains(page, `data-keyring-open="setup" disabled`) || !strings.Contains(page, `data-keyring-dialog="setup"`) ||
		!strings.Contains(page, `<meta name="plainmote-user" content="`+user.ID+`">`) || !strings.Contains(page, assetPath("keyring.js")) {
		t.Fatal("the account page without a master password")
	}
	if _, err := db.CreateKeyring(context.Background(), user.ID, store.Keyring{
		KDF: store.KeyringKDF, Iterations: store.KeyringMinIterations, Salt: bytes.Repeat([]byte{1}, 16),
		WrappedByPassword: bytes.Repeat([]byte{2}, 60), WrappedByRecovery: bytes.Repeat([]byte{3}, 60),
	}, nowUTC()); err != nil {
		t.Fatal(err)
	}
	page = client.do(http.MethodGet, accountPath, nil).Body.String()
	for _, want := range []string{`data-keyring-unlock`, `data-keyring-dialog="recover"`, `data-keyring-dialog="password"`, `<option value="15" selected>15 minutes</option>`, `Unlocked · locks after 15 minutes idle`} {
		if !strings.Contains(page, want) {
			t.Errorf("the account page with a master password lacks %q", want)
		}
	}
	signedOut := httptestRequest(client.app, http.MethodGet, "/", nil, nil).Body.String()
	if strings.Contains(signedOut, "plainmote-user") || !strings.Contains(signedOut, assetPath("lock.js")) {
		t.Fatal("a signed-out page must load lock.js and name no user")
	}
}

// keys.js is run as it is shipped, in Node's WebCrypto: the keyring it makes
// opens with the password and the recovery key and with nothing else.
func TestKeysModule(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	script, err := os.ReadFile(filepath.Join("testdata", "keys_test.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	module, err := filepath.Abs(filepath.Join("static", "keys.js"))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(node, "--input-type=module", "-e", string(script))
	command.Env = append(os.Environ(), "KEYS_MODULE=file://"+module)
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "ok") {
		t.Fatalf("unexpected output: %s", out)
	}
}

func nowUTC() time.Time { return time.Now().UTC() }

// httptestRequest sends one request with the given cookies and header pairs.
func httptestRequest(app *App, method, target string, form url.Values, cookies []*http.Cookie, header ...string) *httptest.ResponseRecorder {
	var request *http.Request
	if form == nil {
		request = httptest.NewRequest(method, "https://cfg.test"+target, nil)
	} else {
		request = httptest.NewRequest(method, "https://cfg.test"+target, strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	request.Header.Set("Accept-Language", "en")
	for i := 0; i+1 < len(header); i += 2 {
		request.Header.Set(header[i], header[i+1])
	}
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	app.handler.ServeHTTP(response, request)
	return response
}

// seal.js, as shipped, against itself and against the bundle the service
// builds.
func TestSealModule(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	script, err := os.ReadFile(filepath.Join("testdata", "seal_test.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	keys, _ := filepath.Abs(filepath.Join("static", "keys.js"))
	seal, _ := filepath.Abs(filepath.Join("static", "seal.js"))
	command := exec.Command(node, "--input-type=module", "-e", string(script))
	command.Env = append(os.Environ(), "KEYS_MODULE=file://"+keys, "SEAL_MODULE=file://"+seal)
	out, err := command.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "ok") {
		t.Fatalf("%v\n%s", err, out)
	}
}
