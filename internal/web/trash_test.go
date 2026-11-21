package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"plainmote/internal/store"
)

func TestTrashTemplatesRenderCleanly(t *testing.T) {
	app := staticApp(t)
	now := time.Now().UTC()
	deletedAt := now.Add(-2 * time.Hour)
	item := store.Resource{
		ID:          "11111111-1111-1111-1111-111111111111",
		Name:        "nginx.conf",
		Filename:    "nginx.conf",
		ContentType: "text/plain",
		ContentSize: 256,
		UpdatedAt:   now,
		DeletedAt:   &deletedAt,
	}
	sealed := store.Resource{
		ID:          "22222222-2222-2222-2222-222222222222",
		ContentType: store.SealedContentType,
		ContentSize: 512,
		SealedKey:   []byte("k"),
		UpdatedAt:   now,
		DeletedAt:   &deletedAt,
	}

	render := func(name string, data pageData) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "https://cfg.test/account/trash", nil)
		rec := httptest.NewRecorder()
		app.renderTemplate(rec, req, http.StatusOK, name, data)
		body := rec.Body.String()
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d body %q", name, rec.Code, body)
		}
		for _, bad := range []string{"template error", "<no value>", "ZgotmplZ"} {
			if strings.Contains(body, bad) {
				t.Fatalf("%s: output contains %q:\n%s", name, bad, body)
			}
		}
		return body
	}

	base := pageData{
		User:       User{ID: "u1", Login: "alice"},
		Active:     "account",
		Section:    "trash",
		SignedIn:   true,
		CSRF:       "csrf",
		BaseURL:    "https://cfg.test",
		TrashDays:  30,
		TrashCount: 2,
		Quota:      store.UserQuota{Limit: store.QuotaLimit{Resources: 100, StorageBytes: 1 << 20}},
	}

	emptyBody := render("account.html", pageData{
		User: base.User, Active: "account", Section: "trash", SignedIn: true, CSRF: "csrf", BaseURL: base.BaseURL, TrashDays: 30, Quota: base.Quota,
	})
	if !strings.Contains(emptyBody, "Trash is empty") || !strings.Contains(emptyBody, `href="/account/trash" class="on"`) {
		t.Fatalf("empty trash missing empty state or active side-nav")
	}

	listData := base
	listData.Resources = []Resource{item, sealed}
	listData.Pager = buildPager(1, 10, 2, pageSizes, func(n int) string { return trashURL(10, n) })
	listData.RestoredResource = &item
	listData.PurgeTarget = &item
	body := render("account.html", listData)
	for _, want := range []string{"nginx.conf", "Encrypted content", "Restore", "Delete permanently", `Restored “nginx.conf”`, `data-sealed-trash`} {
		if !strings.Contains(body, want) {
			t.Errorf("account.html trash section missing %q", want)
		}
	}

	emptyDlgData := base
	emptyDlgData.EmptyOpen = true
	emptyDlg := render("account.html#parts", emptyDlgData)
	if !strings.Contains(emptyDlg, "Empty trash") || !strings.Contains(emptyDlg, "all 2 items") {
		t.Errorf("account.html#parts empty dialog unexpected: %q", emptyDlg)
	}

	dashData := base
	dashData.Active = "resources"
	dashData.Section = ""
	dashData.Trashed = true
	dash := render("dashboard.html", dashData)
	if !strings.Contains(dash, `/account/trash`) || !strings.Contains(dash, "Moved to trash") {
		t.Errorf("dashboard.html missing trash notice link")
	}
}
