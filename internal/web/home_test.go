package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"plainmote/internal/store"
)

func postPaste(t *testing.T, app *App, form url.Values, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "https://cfg.test/paste", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// What a browser sends with a form post; without it the endpoint answers
	// as it would to curl.
	request.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
	for k, v := range headers {
		request.Header.Set(k, v)
	}
	response := httptest.NewRecorder()
	app.handler.ServeHTTP(response, request)
	return response
}

func getPasteResult(t *testing.T, app *App, posted *httptest.ResponseRecorder, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	if posted.Code != http.StatusSeeOther {
		t.Fatalf("paste must redirect to a GET result: %d %s", posted.Code, posted.Body.String())
	}
	location := posted.Header().Get("Location")
	if !strings.HasPrefix(location, pasteResultPrefix) {
		t.Fatalf("paste result location = %q", location)
	}
	request := httptest.NewRequest(http.MethodGet, "https://cfg.test"+location, nil)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	app.handler.ServeHTTP(response, request)
	return response
}

// TestAnyoneCanPasteAndGetALink is the front door working: no account, no
// cookie, one box, and an address that delivers what was typed.
func TestAnyoneCanPasteAndGetALink(t *testing.T) {
	db, _, _ := testDatabase(t)
	app := newTestApp(db)

	home := httptest.NewRecorder()
	app.handler.ServeHTTP(home, httptest.NewRequest(http.MethodGet, "https://cfg.test/", nil))
	if home.Code != http.StatusOK {
		t.Fatalf("the home page must open without a session, got %d", home.Code)
	}
	if body := home.Body.String(); !strings.Contains(body, `action="/paste"`) || !strings.Contains(body, "Continue with GitHub") {
		t.Fatal("the home page must offer the box and a way to sign in")
	}
	// The meta tag and the response header have to agree, or the one page meant
	// to be found de-indexes itself from inside the document.
	// It may say so positively; what it must never say is noindex.
	if strings.Contains(home.Body.String(), "noindex") {
		t.Fatal("the home page must not carry a noindex meta tag")
	}

	posted := postPaste(t, app, url.Values{
		"content":  {"port: 7890\n"},
		"filename": {"notes.txt"},
		"ttl":      {"5"},
	}, nil)
	response := getPasteResult(t, app, posted)
	if response.Code != http.StatusOK {
		t.Fatalf("paste result: %d %s", response.Code, response.Body.String())
	}
	page := response.Body.String()
	if strings.Contains(page, "Save to my resources") {
		t.Fatal("an anonymous visitor must not be offered an account save action")
	}
	if !strings.Contains(page, `name="robots" content="noindex, nofollow, noarchive"`) {
		t.Fatal("the private paste result must carry its noindex meta tag")
	}
	address := findDeliveryAddress(t, page)

	fetched := httptest.NewRecorder()
	app.handler.ServeHTTP(fetched, httptest.NewRequest(http.MethodGet, "https://cfg.test"+address, nil))
	if fetched.Code != http.StatusOK || fetched.Body.String() != "port: 7890\n" {
		t.Fatalf("the address must deliver the paste, got %d %q", fetched.Code, fetched.Body.String())
	}
	// Whatever it is called, it comes back as text.
	if ct := fetched.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("a paste must be delivered as text, got %q", ct)
	}
}

// TestPasteRefusalKeepsWhatWasTyped is the same lesson the resource screen
// taught: a refusal that comes back empty has thrown the visitor's work away,
// and here they have nowhere else to get it from.
func TestPasteRefusalKeepsWhatWasTyped(t *testing.T) {
	db, _, _ := testDatabase(t)
	app := newTestApp(db)

	const typed = "port: 7890\nmode: rule\n"
	response := postPaste(t, app, url.Values{
		"content":  {typed},
		"filename": {"bad/name.txt"},
		"ttl":      {"1h"},
	}, nil)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("this filename must be refused, got %d", response.Code)
	}
	page := response.Body.String()
	if !strings.Contains(page, "port: 7890") {
		t.Fatal("a refused paste must come back with what was typed")
	}
	if !strings.Contains(page, "bad/name.txt") {
		t.Fatal("a refused paste must come back with the filename too")
	}
	if !strings.Contains(page, `value="1h" aria-label="1 hour" checked`) {
		t.Fatal("a refused paste must keep the chosen lifetime")
	}
}

// TestPasteLifetimeCannotBeChosenFreely keeps the page from being a way around
// the ceiling the store holds.
func TestPasteLifetimeCannotBeChosenFreely(t *testing.T) {
	db, _, _ := testDatabase(t)
	app := newTestApp(db)

	// A value that is not on the menu falls back to the default rather than
	// being taken at face value or costing the visitor their paste.
	posted := postPaste(t, app, url.Values{"content": {"x"}, "ttl": {"forever"}}, nil)
	response := getPasteResult(t, app, posted)
	if response.Code != http.StatusOK {
		t.Fatalf("paste result: %d %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "in about 1 hour") {
		t.Fatalf("an unknown lifetime must become the default, got %s", response.Body.String())
	}

	// A bare number is minutes, as it once was for every lifetime; past the
	// ceiling it is refused, not quietly shortened.
	if over := postPaste(t, app, url.Values{"content": {"x"}, "ttl": {"99999"}}, nil); over.Code != http.StatusBadRequest {
		t.Fatalf("a lifetime past 30 days must be refused, got %d", over.Code)
	}
}

// TestPasteRefusesADrivenCrossSitePost is the stateless stand-in for CSRF on an
// endpoint with no session to hang a token off.
func TestPasteRefusesADrivenCrossSitePost(t *testing.T) {
	db, _, _ := testDatabase(t)
	app := newTestApp(db)

	foreign := postPaste(t, app, url.Values{"content": {"x"}}, map[string]string{"Origin": "https://evil.test"})
	if foreign.Code != http.StatusForbidden {
		t.Fatalf("a cross-site post must be refused, got %d", foreign.Code)
	}
	own := postPaste(t, app, url.Values{"content": {"x"}}, map[string]string{"Origin": "https://cfg.test"})
	if own.Code != http.StatusSeeOther {
		t.Fatalf("the page's own form must work, got %d", own.Code)
	}
	// No Origin at all is curl, not an attack, and is let through.
	bare := postPaste(t, app, url.Values{"content": {"x"}}, nil)
	if bare.Code != http.StatusSeeOther {
		t.Fatalf("a request with no Origin must be allowed, got %d", bare.Code)
	}
}

// A quick share made while signed in is its creator's own from the start:
// listed with their resources, its visits in their access history, read
// only until kept, and seen by nobody else.
func TestSignedInQuickShareIsTheCreatorsOwn(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	app := newTestApp(db, user.GitHubID)
	session, csrf, _, err := db.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	owner := []*http.Cookie{{Name: sessionCookie, Value: session}, {Name: csrfCookie, Value: csrf}}
	const content = "名称: 临时节点\nport: 7890\n"
	pasteRequest := httptest.NewRequest(http.MethodPost, "https://cfg.test/paste", strings.NewReader(url.Values{
		"content": {content}, "filename": {"节点.yaml"}, "ttl": {"5"},
	}.Encode()))
	pasteRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	pasteRequest.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
	for _, cookie := range owner {
		pasteRequest.AddCookie(cookie)
	}
	posted := httptest.NewRecorder()
	app.handler.ServeHTTP(posted, pasteRequest)
	resourceID := strings.TrimPrefix(posted.Header().Get("Location"), pasteResultPrefix)
	paste := getPasteResult(t, app, posted, owner...)
	if paste.Code != http.StatusOK {
		t.Fatalf("paste result: %d %s", paste.Code, paste.Body.String())
	}
	page := paste.Body.String()
	if !strings.Contains(page, `<a href="/resources/`+resourceID+`">My resources</a>`) || strings.Contains(page, `action="/paste/save"`) {
		t.Fatal("the result must say the quick share is already in the account, with nothing to save")
	}
	address := findDeliveryAddress(t, page)

	resource, err := db.ResourceForOwner(ctx, user.ID, resourceID)
	if err != nil || !resource.QuickShare() || resource.ExpiresAt.Sub(resource.CreatedAt) != 5*time.Minute {
		t.Fatalf("stored as %+v %v", resource, err)
	}
	quick, _, err := db.ListResourcesOfKind(ctx, user.ID, "", store.KindQuickShare, 10, 0)
	if err != nil || len(quick) != 1 || quick[0].ID != resourceID {
		t.Fatalf("quick share list: %+v %v", quick, err)
	}
	kept, _, err := db.ListResourcesOfKind(ctx, user.ID, "", store.KindResource, 10, 0)
	if err != nil || len(kept) != 1 || kept[0].QuickShare() {
		t.Fatalf("resource list: %+v %v", kept, err)
	}
	quota, err := db.QuotaForUser(ctx, user.ID, time.Now().UTC())
	if err != nil || quota.Usage.StorageBytes != resource.ContentSize+kept[0].ContentSize {
		t.Fatalf("the quick share must count toward the account: %+v %v", quota.Usage, err)
	}

	delivery := httptest.NewRecorder()
	app.handler.ServeHTTP(delivery, httptest.NewRequest(http.MethodGet, "https://cfg.test"+address, nil))
	if delivery.Code != http.StatusOK || delivery.Body.String() != content {
		t.Fatalf("delivery: %d %q", delivery.Code, delivery.Body.String())
	}
	if logs, err := db.ListAccess(ctx, user.ID, resourceID, "", 10); err != nil || len(logs) != 1 || !logs[0].QuickShare {
		t.Fatalf("the visit belongs to the creator: %+v %v", logs, err)
	}

	// Somebody else sees none of it: not the result, not the resource, not
	// the visit.
	other, err := db.UpsertUser(ctx, "999999", "other", "Other", "")
	if err != nil {
		t.Fatal(err)
	}
	otherSession, otherCSRF, _, err := db.CreateSession(ctx, other.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	stranger := []*http.Cookie{{Name: sessionCookie, Value: otherSession}, {Name: csrfCookie, Value: otherCSRF}}
	if got := getPasteResult(t, app, posted, stranger...); got.Code != http.StatusGone {
		t.Fatalf("another account opened the result: %d", got.Code)
	}
	if got := getPasteResult(t, app, posted); got.Code != http.StatusGone {
		t.Fatalf("a signed-out visitor opened the result: %d", got.Code)
	}
	if _, err := db.ResourceForOwner(ctx, other.ID, resourceID); err == nil {
		t.Fatal("another account reached the quick share")
	}
	if logs, _ := db.ListAccess(ctx, other.ID, "", "", 10); len(logs) != 0 {
		t.Fatal("another account sees the visit")
	}
	if logs, _ := db.ListAccess(ctx, store.AnonymousUserID, "", "", 10); len(logs) != 0 {
		t.Fatal("the visit went to the anonymous account")
	}

	request := func(method, path string, form url.Values) *httptest.ResponseRecorder {
		var body io.Reader
		if form != nil {
			form.Set("csrf", csrf)
			body = strings.NewReader(form.Encode())
		}
		r := httptest.NewRequest(method, "https://cfg.test"+path, body)
		if form != nil {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		for _, cookie := range owner {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		app.handler.ServeHTTP(w, r)
		return w
	}

	// Read only: the page offers keeping, not editing, and an edit sent
	// anyway changes nothing.
	resourcePage := request(http.MethodGet, "/resources/"+resourceID, nil).Body.String()
	if !strings.Contains(resourcePage, `name="action" value="keep"`) || strings.Contains(resourcePage, `name="content"`) || strings.Contains(resourcePage, `data-share-panel`) {
		t.Fatal("a quick share's page must be read only, with keeping offered")
	}
	if got := request(http.MethodPost, "/resources/"+resourceID, url.Values{"name": {"x"}, "content": {"changed"}}); got.Code != http.StatusBadRequest {
		t.Fatalf("an edit to a quick share: %d", got.Code)
	}
	if body, _ := db.ReadContent(ctx, resource); string(body) != content {
		t.Fatalf("the quick share changed: %q", body)
	}
	if _, err := db.CreateShare(ctx, user.ID, resourceID, "", time.Hour, 0); err == nil {
		t.Fatal("a quick share took a second link")
	}

	// Kept, it is an ordinary resource; its link keeps its term and its use.
	keep := request(http.MethodPost, "/resources/"+resourceID, url.Values{"action": {"keep"}})
	if keep.Code != http.StatusSeeOther || keep.Header().Get("Location") != "/resources/"+resourceID+"?kept=1" {
		t.Fatalf("keep: %d %q", keep.Code, keep.Header().Get("Location"))
	}
	resource, err = db.ResourceForOwner(ctx, user.ID, resourceID)
	if err != nil || resource.QuickShare() {
		t.Fatalf("kept as %+v %v", resource, err)
	}
	shares, err := db.ListShares(ctx, user.ID, resourceID, time.Now().UTC())
	token, ok := splitDeliveryPath(address)
	if err != nil || len(shares) != 1 || !ok || shares[0].Token != token || shares[0].UsedCount != 1 || shares[0].ExpiresAt == nil {
		t.Fatalf("the link after keeping: %+v %v", shares, err)
	}
	keptPage := request(http.MethodGet, "/resources/"+resourceID+"?kept=1", nil).Body.String()
	if !strings.Contains(keptPage, "Kept as a resource.") || !strings.Contains(keptPage, `name="content"`) {
		t.Fatal("a kept quick share must be editable and say it was kept")
	}
}

// At its end a quick share goes, the way its owner deleting it would; its
// access history stays with its owner.
func TestEndedQuickShareIsDeletedAndItsHistoryKept(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	app := newTestApp(db, user.GitHubID)
	now := time.Now().UTC()
	resource, link, err := db.CreateQuickShare(ctx, user.ID, "app.log", []byte("line\n"), time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	delivery := httptest.NewRecorder()
	app.handler.ServeHTTP(delivery, httptest.NewRequest(http.MethodGet, "https://cfg.test"+shareAddress(link.Token, "app.log"), nil))
	if delivery.Code != http.StatusOK {
		t.Fatalf("delivery: %d", delivery.Code)
	}
	if removed, err := db.PruneQuickShares(ctx, now.Add(30*time.Second)); err != nil || removed != 0 {
		t.Fatalf("swept before its end: %d %v", removed, err)
	}
	if removed, err := db.PruneQuickShares(ctx, now.Add(2*time.Minute)); err != nil || removed != 1 {
		t.Fatalf("sweep: %d %v", removed, err)
	}
	if _, err := db.ResourceForOwner(ctx, user.ID, resource.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the ended quick share is still there: %v", err)
	}
	logs, err := db.ListAccess(ctx, user.ID, "", "", 10)
	if err != nil || len(logs) != 1 || logs[0].ResourceName != "app.log" || logs[0].QuickShare {
		t.Fatalf("history after the end: %+v %v", logs, err)
	}
}

func TestAnonymousPasteCanReturnFromLoginAndBeSaved(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	app := newTestApp(db, user.GitHubID)
	posted := postPaste(t, app, url.Values{"content": {"keep after login"}, "ttl": {"5"}}, nil)
	result := getPasteResult(t, app, posted)
	if result.Code != http.StatusOK {
		t.Fatalf("anonymous result: %d %s", result.Code, result.Body.String())
	}
	resultPath := posted.Header().Get("Location")
	wantLogin := `/login?next=` + url.QueryEscape(resultPath)
	if !strings.Contains(result.Body.String(), `href="`+wantLogin+`"`) {
		t.Fatalf("result login must return to %q: %s", resultPath, result.Body.String())
	}
	// Anyone signed in who holds this address can save the paste.
	if !strings.Contains(result.Body.String(), "is not the share link") {
		t.Fatal("the result page must say its own address is not for forwarding")
	}

	session, csrf, _, err := db.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	afterLogin := getPasteResult(t, app, posted,
		&http.Cookie{Name: sessionCookie, Value: session},
		&http.Cookie{Name: csrfCookie, Value: csrf},
	)
	if afterLogin.Code != http.StatusOK || !strings.Contains(afterLogin.Body.String(), "Save to my resources") {
		t.Fatalf("the result capability must remain claimable after login: %d %s", afterLogin.Code, afterLogin.Body.String())
	}
}

func findDeliveryAddress(t *testing.T, page string) string {
	t.Helper()
	// The copy button carries the address whole; the text on the page sets
	// the token apart in its own element.
	const marker = `data-copy="https://cfg.test/d/`
	start := strings.Index(page, marker)
	if start < 0 {
		t.Fatalf("no delivery address on the page: %s", page)
	}
	rest := page[start+len(`data-copy="https://cfg.test`):]
	if end := strings.IndexAny(rest, `"<`); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

func findPasteResourceID(t *testing.T, page string) string {
	t.Helper()
	const marker = `name="resource_id" value="`
	start := strings.Index(page, marker)
	if start < 0 {
		t.Fatalf("no paste resource id on the page: %s", page)
	}
	rest := page[start+len(marker):]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		t.Fatalf("unterminated paste resource id on the page: %s", page)
	}
	return rest[:end]
}

// TestAnonymousCanBeTurnedOff is what makes this deployable by someone who
// does not want an open write endpoint. The switch removes the endpoint rather
// than leaving it there refusing, and the root stops being a page at all: with
// no box there is nothing at it for a visitor without an account to do, and
// nothing for one with an account that their own list does not do better.
func TestAnonymousCanBeTurnedOff(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	app := &App{db: db, cfg: Config{PublicURL: "https://cfg.test", AllowedIDs: map[string]bool{user.GitHubID: true}}}
	app.templates = app.templateSet()
	app.handler = app.routes()

	get := func(signedIn bool) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "https://cfg.test/", nil)
		if signedIn {
			session, csrf, _, err := db.CreateSession(ctx, user.ID, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
			request.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrf})
		}
		response := httptest.NewRecorder()
		app.handler.ServeHTTP(response, request)
		return response
	}

	anon := get(false)
	if anon.Code != http.StatusSeeOther || anon.Header().Get("Location") != "/login" {
		t.Fatalf("a visitor with no account belongs at the login, got %d %q", anon.Code, anon.Header().Get("Location"))
	}
	signedIn := get(true)
	if signedIn.Code != http.StatusSeeOther || signedIn.Header().Get("Location") != dashboardPath {
		t.Fatalf("an account belongs at its own list, got %d %q", signedIn.Code, signedIn.Header().Get("Location"))
	}

	if posted := postPaste(t, app, url.Values{"content": {"x"}}, nil); posted.Code != http.StatusNotFound {
		t.Fatalf("the endpoint must not exist, got %d", posted.Code)
	}

	// Nothing is meant to be found here now, not even the root.
	robots := httptest.NewRecorder()
	app.handler.ServeHTTP(robots, httptest.NewRequest(http.MethodGet, "https://cfg.test/robots.txt", nil))
	if strings.Contains(robots.Body.String(), "Allow:") {
		t.Fatalf("robots.txt must let nothing through, got %q", robots.Body.String())
	}
	if tag := anon.Header().Get("X-Robots-Tag"); !strings.Contains(tag, "noindex") {
		t.Fatalf("the root must not be indexable when it is a redirect, got %q", tag)
	}
}

// The page preselects the store's default, and that default is one of the
// choices it offers.
func TestHomePreselectsTheDefaultLifetime(t *testing.T) {
	if pasteDefaultTTL != "1h" {
		t.Fatalf("the default lifetime is %s, want 1h", pasteDefaultTTL)
	}
	app := &App{cfg: Config{PublicURL: "https://cfg.test", AnonymousEnabled: true}}
	app.templates = app.templateSet()
	app.handler = app.routes()
	response := httptest.NewRecorder()
	app.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://cfg.test/", nil))
	if !strings.Contains(response.Body.String(), `value="1h" aria-label="1 hour" checked>`) {
		t.Fatal("the home page does not preselect 1 hour")
	}
	if ttl, value := parsePasteTTL(""); ttl != time.Hour || value != "1h" {
		t.Fatalf("a missing lifetime falls back to %s (%s)", ttl, value)
	}
}

// The list marks a quick share and narrows to it; an encrypted one is named
// as encrypted, and its page neither offers keeping nor shows a link that
// could not open it.
func TestQuickSharesInTheResourceList(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	app := newTestApp(db, user.GitHubID)
	now := time.Now().UTC()
	plain, _, err := db.CreateQuickShare(ctx, user.ID, "app.log", []byte("x\n"), time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	sealed, _, err := db.CreateEncryptedPaste(ctx, user.ID, sampleEnvelope(), time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	session, csrf, _, err := db.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	page := func(path string) string {
		r := httptest.NewRequest(http.MethodGet, "https://cfg.test"+path, nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
		r.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrf})
		w := httptest.NewRecorder()
		app.handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d", path, w.Code)
		}
		return w.Body.String()
	}
	all := page("/resources/")
	if strings.Count(all, `<span class="tag qs">Quick share</span>`) != 2 || !strings.Contains(all, "Encrypted content") || !strings.Contains(all, `<span class="st qs-left">`) {
		t.Fatal("the list does not mark its quick shares")
	}
	quick := page("/resources/?kind=quick")
	if !strings.Contains(quick, plain.ID) || !strings.Contains(quick, sealed.ID) || strings.Contains(quick, resource.ID) || !strings.Contains(quick, `value="quick" selected`) {
		t.Fatal("the quick share filter")
	}
	if kept := page("/resources/?kind=resource"); strings.Contains(kept, plain.ID) || !strings.Contains(kept, resource.ID) {
		t.Fatal("the resource filter")
	}
	encrypted := page("/resources/" + sealed.ID)
	if !strings.Contains(encrypted, "The content is end-to-end encrypted; the service cannot read it.") ||
		strings.Contains(encrypted, `value="keep"`) || strings.Contains(encrypted, `data-copy=`) {
		t.Fatal("an encrypted quick share's page")
	}
	if !strings.Contains(page("/resources/"+plain.ID), `data-copy="https://cfg.test/d/`) {
		t.Fatal("a quick share's page must offer its link")
	}
}
