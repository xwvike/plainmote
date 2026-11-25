package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
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

	// Inside the folder, Cluster appears and breadcrumb shows Configs.
	inFolder := client.do(http.MethodGet, "/resources/?folder="+folderID, nil).Body.String()
	if !strings.Contains(inFolder, "Configs") || !strings.Contains(inFolder, "Cluster") {
		t.Fatalf("folder page missing breadcrumb or moved resource")
	}

	// Resource detail page shows folder in its breadcrumb.
	detail := client.do(http.MethodGet, "/resources/"+yamlRes.ID, nil).Body.String()
	if !strings.Contains(detail, "/resources/?folder="+folderID) {
		t.Fatal("resource detail breadcrumb missing parent folder link")
	}

	// Delete the folder: moves Cluster to trash and redirects with folder_trashed=1.
	deleted := postForm("/resources/folders", url.Values{"action": {"delete"}, "folder_id": {folderID}})
	if deleted.Code != http.StatusSeeOther || !strings.Contains(deleted.Header().Get("Location"), "folder_trashed=1") {
		t.Fatalf("delete folder: %d %q", deleted.Code, deleted.Header().Get("Location"))
	}
	if count, err := db.TrashCount(ctx, user.ID); err != nil || count != 1 {
		t.Fatalf("trash count after folder delete: %d %v", count, err)
	}
}
