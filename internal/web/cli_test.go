package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"plainmote/internal/store"
)

func TestCLIEditCommand(t *testing.T) {
	id := "ad45ba6c-634b-4418-92ae-cc40b249a79f"
	for _, tc := range []struct{ base, want string }{
		{"https://plainmote.link", "plainmote edit ad45ba6c"},
		{"https://notes.example.com", "plainmote edit ad45ba6c --server https://notes.example.com"},
		{"", "plainmote edit ad45ba6c"},
	} {
		if got := cliEditCommand(tc.base, id); got != tc.want {
			t.Errorf("cliEditCommand(%q) = %q, want %q", tc.base, got, tc.want)
		}
	}
}

// The command is offered where the command line can do the edit: a stored
// text the owner may still change. A picture, a reference or a resource the
// operator took down has nothing it could open.
func TestResourcePageOffersTheEditCommand(t *testing.T) {
	db, user, text := testDatabase(t)
	ctx := context.Background()
	picture, err := db.CreateResource(ctx, user.ID, "Image", "image.png", []byte("not\x00text"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	remote, err := db.CreateResource(ctx, user.ID, "Upstream", "remote.txt", nil, "", "https://example.com/remote.txt")
	if err != nil {
		t.Fatal(err)
	}
	banned, err := db.CreateResource(ctx, user.ID, "Banned", "banned.txt", []byte("text\n"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AdminTakedown(ctx, store.AdminActor{KeyID: "test"}, banned.ID, "test", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	app := newTestApp(db)
	session, csrf, _, err := db.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	page := func(path string) string {
		request := httptest.NewRequest(http.MethodGet, "https://cfg.test"+path, nil)
		request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
		request.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrf})
		response := httptest.NewRecorder()
		app.handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s: %d", path, response.Code)
		}
		return response.Body.String()
	}
	command := "plainmote edit " + text.ID[:8] + " --server https://cfg.test"
	body := page("/resources/" + text.ID)
	if !strings.Contains(body, "<code><b>$</b> "+command+"</code>") || !strings.Contains(body, `data-copy="`+command+`"`) || !strings.Contains(body, `<a class="txt" href="/cli">`) {
		t.Fatal("a text resource does not offer its edit command and the way to the command line")
	}
	for _, path := range []string{"/resources/" + picture.ID, "/resources/" + remote.ID, "/resources/" + banned.ID, "/resources/new"} {
		if strings.Contains(page(path), `class="cli-edit"`) {
			t.Errorf("%s offers an edit command the command line cannot carry out", path)
		}
	}
}
