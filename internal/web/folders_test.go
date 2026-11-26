package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"plainmote/internal/store"
)

func TestDashboardFinderViewsAndFolders(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	client := signedIn(t, db, user)

	// Create a YAML resource so we can verify human-readable Kind in list view.
	yamlRes, err := db.CreateResource(ctx, user.ID, "Cluster", "cluster.yaml", []byte("replicas: 3\n"), "")
	if err != nil {
		t.Fatal(err)
	}

	// Default view is Icon view.
	iconResp := client.do(http.MethodGet, "/resources/", nil)
	if iconResp.Code != http.StatusOK || !strings.Contains(iconResp.Body.String(), `data-fb-view="icon"`) || !strings.Contains(iconResp.Body.String(), `>YAML<`) {
		t.Fatalf("default icon view failed: %d", iconResp.Code)
	}

	// Switch to List view via ?view=list: sets cookie and renders Finder 4 columns
	// (Name, Size, Kind, Date added) without raw MIME strings.
	listReq := httptest.NewRequest(http.MethodGet, "https://cfg.test/resources/?view=list", nil)
	listReq.Header.Set("Accept-Language", "zh-CN")
	listReq.AddCookie(&http.Cookie{Name: sessionCookie, Value: client.session})
	listResp := httptest.NewRecorder()
	client.app.handler.ServeHTTP(listResp, listReq)
	body := listResp.Body.String()
	for _, want := range []string{`class="fx fb-list"`, "大小", "种类", "添加日期", "YAML 配置"} {
		if !strings.Contains(body, want) {
			t.Errorf("list view missing %q", want)
		}
	}
	if strings.Contains(body, "text/plain; charset=utf-8") {
		t.Error("list view should show human-readable kind rather than raw MIME")
	}
	var viewVal string
	for _, c := range listResp.Result().Cookies() {
		if c.Name == viewCookie {
			viewVal = c.Value
		}
	}
	if viewVal != viewList {
		t.Fatalf("expected %s=%s cookie, got %q", viewCookie, viewList, viewVal)
	}

	// Create a folder via POST /resources/folders, move the YAML resource into it,
	// and verify folder view & breadcrumbs.
	postForm := func(path string, vals url.Values) *httptest.ResponseRecorder {
		vals.Set("csrf", client.csrf)
		req := httptest.NewRequest(http.MethodPost, "https://cfg.test"+path, strings.NewReader(vals.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "https://cfg.test")
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: client.session})
		req.AddCookie(&http.Cookie{Name: csrfCookie, Value: client.csrf})
		rec := httptest.NewRecorder()
		client.app.handler.ServeHTTP(rec, req)
		return rec
	}

	created := postForm("/resources/folders", url.Values{"action": {"create"}, "name": {"Configs"}})
	if created.Code != http.StatusSeeOther {
		t.Fatalf("create folder: %d %s", created.Code, created.Body.String())
	}
	folders, err := db.ListChildFolders(ctx, user.ID, "")
	if err != nil || len(folders) != 1 || folders[0].Name != "Configs" {
		t.Fatalf("folders after create: %+v %v", folders, err)
	}
	folderID := folders[0].ID

	// Move YAML resource into the new folder via JSON (drag-and-drop path).
	moveVals := url.Values{"csrf": {client.csrf}, "resource_id": {yamlRes.ID}, "target_folder_id": {folderID}}
	moveReq := httptest.NewRequest(http.MethodPost, "https://cfg.test/resources/move", strings.NewReader(moveVals.Encode()))
	moveReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	moveReq.Header.Set("Origin", "https://cfg.test")
	moveReq.Header.Set("Accept", "application/json")
	moveReq.AddCookie(&http.Cookie{Name: sessionCookie, Value: client.session})
	moveReq.AddCookie(&http.Cookie{Name: csrfCookie, Value: client.csrf})
	moveResp := httptest.NewRecorder()
	client.app.handler.ServeHTTP(moveResp, moveReq)
	if moveResp.Code != http.StatusOK {
		t.Fatalf("move resource json: %d %s", moveResp.Code, moveResp.Body.String())
	}

	// Inside the folder, cluster.yaml appears and breadcrumb shows Configs.
	inFolder := client.do(http.MethodGet, "/resources/?folder="+folderID, nil).Body.String()
	if !strings.Contains(inFolder, "Configs") || !strings.Contains(inFolder, "cluster.yaml") {
		t.Fatalf("folder page missing breadcrumb or moved resource")
	}

	// Resource detail page shows folder in its breadcrumb.
	detail := client.do(http.MethodGet, "/resources/"+yamlRes.ID, nil).Body.String()
	if !strings.Contains(detail, "/resources/?folder="+folderID) {
		t.Fatal("resource detail breadcrumb missing parent folder link")
	}

	// Delete the folder: moves cluster.yaml to trash and redirects with folder_trashed=1.
	deleted := postForm("/resources/folders", url.Values{"action": {"delete"}, "folder_id": {folderID}})
	if deleted.Code != http.StatusSeeOther || !strings.Contains(deleted.Header().Get("Location"), "folder_trashed=1") {
		t.Fatalf("delete folder: %d %q", deleted.Code, deleted.Header().Get("Location"))
	}
	if count, err := db.TrashCount(ctx, user.ID); err != nil || count != 1 {
		t.Fatalf("trash count after folder delete: %d %v", count, err)
	}
}

func TestModalSheetsRenameTagsAndSingleFilename(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	client := signedIn(t, db, user)

	postForm := func(path string, vals url.Values, acceptJSON bool) *httptest.ResponseRecorder {
		vals.Set("csrf", client.csrf)
		req := httptest.NewRequest(http.MethodPost, "https://cfg.test"+path, strings.NewReader(vals.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "https://cfg.test")
		if acceptJSON {
			req.Header.Set("Accept", "application/json")
		}
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: client.session})
		req.AddCookie(&http.Cookie{Name: csrfCookie, Value: client.csrf})
		rec := httptest.NewRecorder()
		client.app.handler.ServeHTTP(rec, req)
		return rec
	}

	// 1. Creating a resource with only "filename" (no "name") populates both fields
	// and preserves ?modal=edit on redirect.
	created := postForm("/resources/new", url.Values{
		"filename":         {"notes.md"},
		"content":          {"# Hello\n"},
		"content_encoding": {"utf-8"},
		"content_eol":      {"lf"},
		"modal":            {"edit"},
	}, false)
	if created.Code != http.StatusSeeOther {
		t.Fatalf("create single-filename resource: %d %s", created.Code, created.Body.String())
	}
	loc := created.Header().Get("Location")
	if !strings.Contains(loc, "created=1") || !strings.Contains(loc, "modal=edit") {
		t.Fatalf("expected created=1&modal=edit redirect, got %q", loc)
	}
	resID := strings.TrimPrefix(strings.Split(loc, "?")[0], "/resources/")
	res, err := db.ResourceForOwner(ctx, user.ID, resID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Filename != "notes.md" || res.Name != "notes.md" {
		t.Fatalf("expected unified name/filename 'notes.md', got name=%q filename=%q", res.Name, res.Filename)
	}

	// 2. Standalone resource page stays strictly DENY / frame-ancestors 'none'.
	standalone := client.do(http.MethodGet, "/resources/"+resID, nil)
	if got := standalone.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Fatalf("standalone X-Frame-Options = %q, want DENY", got)
	}
	if csp := standalone.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("standalone CSP missing frame-ancestors 'none': %q", csp)
	}

	// 3. ?modal=edit renders pure edit sheet (SAMEORIGIN, no share panel, no history/delete/topbar).
	editModal := client.do(http.MethodGet, "/resources/"+resID+"?modal=edit", nil)
	if editModal.Code != http.StatusOK {
		t.Fatalf("edit modal: %d", editModal.Code)
	}
	if got := editModal.Header().Get("X-Frame-Options"); got != "SAMEORIGIN" {
		t.Fatalf("modal X-Frame-Options = %q, want SAMEORIGIN", got)
	}
	if csp := editModal.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'self'") {
		t.Fatalf("modal CSP missing frame-ancestors 'self': %q", csp)
	}
	editBody := editModal.Body.String()
	for _, want := range []string{`class="is-modal modal-edit"`, `name="filename"`, `notes.md`, `modal-bridge.js`} {
		if !strings.Contains(editBody, want) {
			t.Errorf("edit modal missing %q", want)
		}
	}
	for _, forbid := range []string{`data-part="shares"`, `data-part="hist"`, `?delete=1`, `<header class="top"`} {
		if strings.Contains(editBody, forbid) {
			t.Errorf("pure edit modal should not contain %q", forbid)
		}
	}

	// 4. ?modal=share renders only share panel (no editor form).
	shareModal := client.do(http.MethodGet, "/resources/"+resID+"?modal=share", nil)
	shareBody := shareModal.Body.String()
	if shareModal.Code != http.StatusOK || !strings.Contains(shareBody, `data-part="shares"`) {
		t.Fatalf("share modal missing shares panel: %d", shareModal.Code)
	}
	if strings.Contains(shareBody, `class="rform"`) {
		t.Error("share modal should not render editor form")
	}

	// 5. ?modal=versions renders compact versions modal with SAMEORIGIN and valid
	// unescaped compare/view links, and compare?modal=versions returns HTTP 200.
	_ = postForm("/resources/"+resID, url.Values{
		"filename":         {"notes.md"},
		"content":          {"# Hello v2\n"},
		"content_encoding": {"utf-8"},
		"content_eol":      {"lf"},
		"base_version":     {"1"},
		"modal":            {"edit"},
	}, false)
	verModal := client.do(http.MethodGet, "/resources/"+resID+"/versions?modal=versions", nil)
	verBody := verModal.Body.String()
	if verModal.Code != http.StatusOK || verModal.Header().Get("X-Frame-Options") != "SAMEORIGIN" || strings.Contains(verBody, `<header class="top"`) {
		t.Fatalf("versions modal unexpected response: %d", verModal.Code)
	}
	if strings.Contains(verBody, "%26modal") || strings.Contains(verBody, "%3Fmodal") {
		t.Fatalf("versions modal has URL-escaped modal query params in links")
	}
	if !strings.Contains(verBody, `compare?from=1&amp;to=2&amp;modal=versions`) {
		t.Fatalf("versions modal missing clean compare link")
	}
	cmpModal := client.do(http.MethodGet, "/resources/"+resID+"/versions/compare?from=1&to=2&modal=versions", nil)
	if cmpModal.Code != http.StatusOK || cmpModal.Header().Get("X-Frame-Options") != "SAMEORIGIN" {
		t.Fatalf("compare modal failed: %d", cmpModal.Code)
	}

	// 6. POST /resources/rename updates filename & name atomically.
	ren := postForm("/resources/rename", url.Values{"resource_id": {resID}, "filename": {"readme.md"}}, true)
	if ren.Code != http.StatusOK || !strings.Contains(ren.Body.String(), `"location"`) {
		t.Fatalf("rename resource: %d %s", ren.Code, ren.Body.String())
	}
	resAfter, err := db.ResourceForOwner(ctx, user.ID, resID)
	if err != nil || resAfter.Filename != "readme.md" || resAfter.Name != "readme.md" {
		t.Fatalf("after rename: %+v %v", resAfter, err)
	}

	// 7. POST /resources/{id}/tags with Accept: application/json adds and removes tags cleanly.
	addTag := postForm("/resources/"+resID+"/tags", url.Values{"add": {"ops"}}, true)
	if addTag.Code != http.StatusOK || !strings.Contains(addTag.Body.String(), `"ops"`) {
		t.Fatalf("add tag json: %d %s", addTag.Code, addTag.Body.String())
	}
	remTag := postForm("/resources/"+resID+"/tags", url.Values{"remove": {"ops"}}, true)
	if remTag.Code != http.StatusOK || strings.Contains(remTag.Body.String(), `"ops"`) {
		t.Fatalf("remove tag json: %d %s", remTag.Code, remTag.Body.String())
	}

	// 8. Per-file access logs modal (?modal=share) renders tabbed switcher,
	// supports link + outcome filters, and /logs?resource=... redirects cleanly.
	linkA, err := db.CreateShare(ctx, user.ID, resID, "alpha", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	linkB, err := db.CreateShare(ctx, user.ID, resID, "beta", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.RecordAccess(ctx, store.AccessEvent{OwnerID: user.ID, ResourceID: resID, LinkID: linkA.ID, LinkName: "alpha", Outcome: store.OutcomeSuccess, Status: 200, Version: 2}, store.RequestMeta{RemoteIP: "198.51.100.1"})
	_ = db.RecordAccess(ctx, store.AccessEvent{OwnerID: user.ID, ResourceID: resID, LinkID: linkB.ID, LinkName: "beta", Outcome: store.OutcomeRevoked, Status: 410}, store.RequestMeta{RemoteIP: "198.51.100.2"})

	logsModal := client.do(http.MethodGet, "/resources/"+resID+"/logs?modal=share", nil)
	logsBody := logsModal.Body.String()
	if logsModal.Code != http.StatusOK || logsModal.Header().Get("X-Frame-Options") != "SAMEORIGIN" {
		t.Fatalf("logs modal: %d %q", logsModal.Code, logsModal.Header().Get("X-Frame-Options"))
	}
	for _, want := range []string{`class="mbar-tabs"`, "alpha", "beta", "198.51.100.1", "198.51.100.2"} {
		if !strings.Contains(logsBody, want) {
			t.Errorf("logs modal missing %q", want)
		}
	}
	onlyA := client.do(http.MethodGet, "/resources/"+resID+"/logs?link="+linkA.ID+"&modal=share", nil).Body.String()
	if !strings.Contains(onlyA, "198.51.100.1") || strings.Contains(onlyA, "198.51.100.2") {
		t.Fatal("link filter did not narrow access logs to linkA")
	}
	onlyFailed := client.do(http.MethodGet, "/resources/"+resID+"/logs?outcome=failed&modal=share", nil).Body.String()
	if strings.Contains(onlyFailed, "198.51.100.1") || !strings.Contains(onlyFailed, "198.51.100.2") {
		t.Fatal("outcome=failed filter did not narrow to blocked accesses")
	}
	redir := client.do(http.MethodGet, "/logs?resource="+resID, nil)
	if redir.Code != http.StatusSeeOther || redir.Header().Get("Location") != "/resources/"+resID+"/logs" {
		t.Fatalf("legacy /logs?resource redirect: %d %q", redir.Code, redir.Header().Get("Location"))
	}
}

func TestSiblingFilenameUniqueness(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	client := signedIn(t, db, user)

	postForm := func(path string, vals url.Values) *httptest.ResponseRecorder {
		vals.Set("csrf", client.csrf)
		req := httptest.NewRequest(http.MethodPost, "https://cfg.test"+path, strings.NewReader(vals.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "https://cfg.test")
		req.Header.Set("Accept", "application/json")
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: client.session})
		req.AddCookie(&http.Cookie{Name: csrfCookie, Value: client.csrf})
		rec := httptest.NewRecorder()
		client.app.handler.ServeHTTP(rec, req)
		return rec
	}

	// 1. Create notes.md in root, then verify creating NOTES.MD auto-suffixes
	// like Finder ("NOTES (2).MD") instead of failing.
	r1, err := db.CreateResource(ctx, user.ID, "notes.md", "notes.md", []byte("one\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	dup := postForm("/resources/new", url.Values{
		"filename":         {"NOTES.MD"},
		"content":          {"two\n"},
		"content_encoding": {"utf-8"},
		"content_eol":      {"lf"},
	})
	if dup.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 auto-numbered create on duplicate filename, got %d %s", dup.Code, dup.Body.String())
	}
	dupID := strings.TrimPrefix(strings.Split(dup.Header().Get("Location"), "?")[0], "/resources/")
	dupRes, err := db.ResourceForOwner(ctx, user.ID, dupID)
	if err != nil || dupRes.Filename != "NOTES (2).MD" {
		t.Fatalf("expected auto-numbered 'NOTES (2).MD', got %+v %v", dupRes, err)
	}

	// 1b. Creating unnamed resources via web UI auto-numbers ("Untitled resource",
	// "Untitled resource (2)").
	u1 := postForm("/resources/new", url.Values{"content": {"first\n"}, "content_encoding": {"utf-8"}, "content_eol": {"lf"}})
	u2 := postForm("/resources/new", url.Values{"content": {"second\n"}, "content_encoding": {"utf-8"}, "content_eol": {"lf"}})
	if u1.Code != http.StatusSeeOther || u2.Code != http.StatusSeeOther {
		t.Fatalf("unnamed creates failed: %d %d", u1.Code, u2.Code)
	}
	u2ID := strings.TrimPrefix(strings.Split(u2.Header().Get("Location"), "?")[0], "/resources/")
	u2Res, err := db.ResourceForOwner(ctx, user.ID, u2ID)
	if err != nil || u2Res.Filename != "Untitled resource (2)" {
		t.Fatalf("expected second unnamed resource 'Untitled resource (2)', got %+v %v", u2Res, err)
	}

	// 2. Creating notes.md inside a subfolder stays "notes.md" (no suffix).
	sub, err := db.CreateFolder(ctx, user.ID, "", "Sub")
	if err != nil {
		t.Fatal(err)
	}
	inSub := postForm("/resources/new", url.Values{
		"folder_id":        {sub.ID},
		"filename":         {"notes.md"},
		"content":          {"sub\n"},
		"content_encoding": {"utf-8"},
		"content_eol":      {"lf"},
	})
	if inSub.Code != http.StatusSeeOther {
		t.Fatalf("creating same filename in subfolder should succeed, got %d %s", inSub.Code, inSub.Body.String())
	}
	inSubID := strings.TrimPrefix(strings.Split(inSub.Header().Get("Location"), "?")[0], "/resources/")
	inSubRes, err := db.ResourceForOwner(ctx, user.ID, inSubID)
	if err != nil || inSubRes.Filename != "notes.md" {
		t.Fatalf("expected subfolder copy to keep 'notes.md', got %+v %v", inSubRes, err)
	}

	// 3. Moving root notes.md into Sub (which already has notes.md) is refused.
	mv := postForm("/resources/move", url.Values{"resource_id": {r1.ID}, "target_folder_id": {sub.ID}})
	if mv.Code != http.StatusBadRequest {
		t.Fatalf("moving duplicate filename into folder should fail with 400, got %d %s", mv.Code, mv.Body.String())
	}

	// 4. Renaming another root file to notes.md is refused.
	r2, err := db.CreateResource(ctx, user.ID, "other.md", "other.md", []byte("other\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	ren := postForm("/resources/rename", url.Values{"resource_id": {r2.ID}, "filename": {"Notes.md"}})
	if ren.Code != http.StatusBadRequest {
		t.Fatalf("renaming to existing sibling filename should fail with 400, got %d %s", ren.Code, ren.Body.String())
	}

	// 5. Trashing notes.md, creating a fresh notes.md, then restoring the trashed
	// one auto-suffixes to "notes (3).md" (since notes.md & NOTES (2).MD exist).
	if _, err := db.TrashResource(ctx, user.ID, r1.ID, r1.CreatedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateResource(ctx, user.ID, "notes.md", "notes.md", []byte("replacement\n"), ""); err != nil {
		t.Fatalf("creating notes.md after trashing old one should succeed: %v", err)
	}
	restored, err := db.RestoreResource(ctx, user.ID, r1.ID, r1.CreatedAt)
	if err != nil {
		t.Fatalf("restore trashed notes.md: %v", err)
	}
	if restored.Filename != "notes (3).md" {
		t.Fatalf("expected restored duplicate to become 'notes (3).md', got %q", restored.Filename)
	}
}

func TestMarkdownPreviewSupport(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	client := signedIn(t, db, user)

	// 1. Static markdown.js asset is embedded and served with JS content-type.
	assetRec := httptest.NewRecorder()
	client.app.handler.ServeHTTP(assetRec, httptest.NewRequest(http.MethodGet, assetPath("markdown.js"), nil))
	if assetRec.Code != http.StatusOK {
		t.Fatalf("GET markdown.js: got %d", assetRec.Code)
	}
	if ct := assetRec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Fatalf("unexpected markdown.js content-type: %q", ct)
	}

	// 2. Create a .md resource and verify both modal and standalone pages carry the preview switcher.
	res, err := db.CreateResource(ctx, user.ID, "guide.md", "guide.md", []byte("# Hello\n\n- [x] ready\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/resources/" + res.ID + "?modal=edit",
		"/resources/" + res.ID,
		"/resources/new?modal=edit",
	} {
		rec := client.do(http.MethodGet, path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: got %d", path, rec.Code)
		}
		body := rec.Body.String()
		for _, want := range []string{`data-md-mode`, `data-md-view="preview"`, `data-md-view="edit"`, `data-msg-md-empty`} {
			if !strings.Contains(body, want) {
				t.Fatalf("GET %s missing %q", path, want)
			}
		}
	}

	// 3. Save a second version and verify version preview carries the read-only switcher.
	if err := db.UpdateResource(ctx, user.ID, res.ID, "guide.md", "guide.md", []byte("# Hello v2\n"), "utf-8"); err != nil {
		t.Fatal(err)
	}
	verRec := client.do(http.MethodGet, "/resources/"+res.ID+"/versions/1?modal=history", nil)
	if verRec.Code != http.StatusOK {
		t.Fatalf("GET version 1: got %d", verRec.Code)
	}
	verBody := verRec.Body.String()
	for _, want := range []string{`data-md-mode`, `data-md-view="preview"`, `data-md-view="edit"`, `data-msg-md-empty`} {
		if !strings.Contains(verBody, want) {
			t.Fatalf("GET version 1 missing %q", want)
		}
	}
}
