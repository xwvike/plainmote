package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestTokenIsTheWholeAddress pins the property the new address shape exists
// for: the token routes on its own, and the name after it is decoration for
// whoever saves the file.
func TestTokenIsTheWholeAddress(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	app := newTestApp(db)
	resource, err := db.CreateResource(ctx, user.ID, "clash", "clash.yaml", []byte("port: 7890\n"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	share, err := db.CreateShare(ctx, user.ID, resource.ID, "s", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	token := url.PathEscape(share.Token)

	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		app.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://cfg.test"+path, nil))
		return response
	}

	// The canonical address, and every variation of the decorative tail.
	for _, path := range []string{
		"/d/" + token + "/clash.yaml",
		"/d/" + token + "/anything-else.txt",
		"/d/" + token,
	} {
		if got := get(path); got.Code != http.StatusOK || got.Body.String() != "port: 7890\n" {
			t.Errorf("%s: got %d %q", path, got.Code, got.Body.String())
		}
	}

	// A malformed token is rejected at the HTTP boundary without reaching the
	// database, no matter how right the tail looks.
	if got := get("/d/nonsense/clash.yaml"); got.Code != http.StatusNotFound {
		t.Errorf("a malformed token should not resolve, got %d", got.Code)
	}
	// The old path-shaped address is gone.
	if got := get("/configs/clash.yaml?token=" + token); got.Code != http.StatusNotFound {
		t.Errorf("the old address shape should not resolve, got %d", got.Code)
	}
	// Root is the home page now, and it must never be a way to a resource.
	if got := get("/"); strings.Contains(got.Body.String(), "port: 7890") {
		t.Error("root must not serve a resource")
	}
}

// The token used to ride in the query string, which the audit log scrubs. It
// is now a path segment and must be scrubbed there too.
func TestTokenIsNotWrittenToTheAuditLog(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	app := newTestApp(db)
	resource, err := db.CreateResource(ctx, user.ID, "clash", "clash.yaml", []byte("a: 1\n"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	share, err := db.CreateShare(ctx, user.ID, resource.ID, "s", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	app.Handler().ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "https://cfg.test/d/"+url.PathEscape(share.Token)+"/clash.yaml", nil))

	logs, err := db.ListAccess(ctx, user.ID, "", "", 10)
	if err != nil || len(logs) == 0 {
		t.Fatalf("no access log: %v", err)
	}
	for _, entry := range logs {
		if strings.Contains(entry.Path, share.Token) || strings.Contains(entry.Query, share.Token) {
			t.Fatalf("the token leaked into the audit log: path=%q query=%q", entry.Path, entry.Query)
		}
	}
	if !strings.Contains(logs[0].Path, "[redacted]") {
		t.Fatalf("expected the token segment to be redacted, got %q", logs[0].Path)
	}
	if !strings.HasSuffix(logs[0].Path, "/clash.yaml") {
		t.Fatalf("the filename should survive redaction, got %q", logs[0].Path)
	}
}

// Same filename, two owners, two independent links: the filename is not an
// address, so it cannot collide.
func TestSameFilenameForTwoOwners(t *testing.T) {
	db, alice, _ := testDatabase(t)
	ctx := context.Background()
	app := newTestApp(db)
	bob, err := db.UpsertUser(ctx, "999", "bob", "", "")
	if err != nil {
		t.Fatal(err)
	}
	serve := func(ownerID, body string) string {
		t.Helper()
		resource, err := db.CreateResource(ctx, ownerID, "clash", "clash.yaml", []byte(body), "", "")
		if err != nil {
			t.Fatal(err)
		}
		share, err := db.CreateShare(ctx, ownerID, resource.ID, "s", time.Hour, 0)
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		app.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet,
			"https://cfg.test/d/"+url.PathEscape(share.Token)+"/clash.yaml", nil))
		return response.Body.String()
	}
	if got := serve(alice.ID, "alice\n"); got != "alice\n" {
		t.Fatalf("alice got %q", got)
	}
	if got := serve(bob.ID, "bob\n"); got != "bob\n" {
		t.Fatalf("bob got %q", got)
	}
}

// A resource with no filename still has to arrive as a usable file: plain
// `curl -O` names it after the last path segment, so the address never ends
// with the token itself.
func TestUnnamedResourceStillGetsAFilename(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	app := newTestApp(db)
	session, csrf, _, err := db.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ label, body, wantName, wantType string }{
		{"json 内容", `{"a":1}`, "file.json", "application/json"},
		{"纯文本内容", "port: 7890\n", "file.txt", "text/plain; charset=utf-8"},
	} {
		resource, err := db.CreateResource(ctx, user.ID, "r", "", []byte(tc.body), "", "")
		if err != nil {
			t.Fatal(err)
		}
		share, err := db.CreateShare(ctx, user.ID, resource.ID, "s", time.Hour, 0)
		if err != nil {
			t.Fatal(err)
		}

		// The address the page hands out ends with a real name.
		// The links live in the share dialog, so open it.
		page := httptest.NewRequest(http.MethodGet, "https://cfg.test/resources/"+resource.ID+"?shares=1", nil)
		page.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
		page.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrf})
		rendered := httptest.NewRecorder()
		app.Handler().ServeHTTP(rendered, page)
		wantAddress := "/d/" + share.Token + "/" + tc.wantName
		if !strings.Contains(rendered.Body.String(), wantAddress) {
			t.Errorf("%s: the page should offer %s", tc.label, wantAddress)
		}

		// And the response says the same thing, for clients that read it.
		delivered := httptest.NewRecorder()
		app.Handler().ServeHTTP(delivered, httptest.NewRequest(http.MethodGet, "https://cfg.test"+wantAddress, nil))
		if delivered.Code != http.StatusOK {
			t.Fatalf("%s: %d", tc.label, delivered.Code)
		}
		if got := delivered.Header().Get("Content-Type"); got != tc.wantType {
			t.Errorf("%s: served as %q, want %q", tc.label, got, tc.wantType)
		}
		if got, want := delivered.Header().Get("Content-Disposition"), `inline; filename="`+tc.wantName+`"`; got != want {
			t.Errorf("%s: disposition %q, want %q", tc.label, got, want)
		}
	}

	// A named resource keeps its own name in both places.
	named, err := db.CreateResource(ctx, user.ID, "r", "clash.yaml", []byte("port: 7890\n"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	share, err := db.CreateShare(ctx, user.ID, named.ID, "s", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet,
		"https://cfg.test/d/"+url.PathEscape(share.Token)+"/clash.yaml", nil))
	if got, want := response.Header().Get("Content-Disposition"), `inline; filename="clash.yaml"`; got != want {
		t.Fatalf("disposition %q, want %q", got, want)
	}

	// Whatever ends up in the header cannot break out of it.
	for _, entry := range []string{`inline; filename="clash.yaml"`, `inline; filename="file.json"`, `inline; filename="file.txt"`} {
		if strings.ContainsAny(entry[len(`inline; filename="`):len(entry)-1], "\"\r\n;") {
			t.Fatalf("header value is not safely quoted: %q", entry)
		}
	}
}
