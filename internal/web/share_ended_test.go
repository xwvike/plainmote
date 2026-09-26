package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A revoked link stays on the resource page for a while, struck through, with
// nothing left to do but open its history or delete it - no copy, settings or
// revoke.
func TestRevokedLinkStaysOnThePageAsEnded(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	share, err := db.CreateShare(ctx, user.ID, resource.ID, "old phone", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RevokeLink(ctx, user.ID, resource.ID, share.ID); err != nil {
		t.Fatal(err)
	}
	client := signedIn(t, db, user)
	request := httptest.NewRequest(http.MethodGet, "https://cfg.test/resources/"+resource.ID, nil)
	request.Header.Set("Accept-Language", "en")
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: client.session})
	request.AddCookie(&http.Cookie{Name: csrfCookie, Value: client.csrf})
	response := httptest.NewRecorder()
	client.app.handler.ServeHTTP(response, request)
	page := response.Body.String()

	start := strings.Index(page, `<li class="ended">`)
	if start < 0 {
		t.Fatalf("no ended row on the page")
	}
	row := page[start : start+strings.Index(page[start:], "</li>")]
	for _, want := range []string{"old phone", "Revoked", `href="/logs?resource=` + resource.ID + `"`, `name="action" value="delete"`} {
		if !strings.Contains(row, want) {
			t.Errorf("the ended row is missing %q", want)
		}
	}
	for _, unwanted := range []string{"data-copy", "?share=", `value="revoke"`} {
		if strings.Contains(row, unwanted) {
			t.Errorf("an ended link offers %q", unwanted)
		}
	}
}

// A live link that expires carries its ended state too, and the seconds until
// the stylesheet shows it; a link that never expires carries neither.
func TestExpiringLinkCarriesItsEndedState(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	if _, err := db.CreateShare(ctx, user.ID, resource.ID, "soon", time.Hour, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateShare(ctx, user.ID, resource.ID, "forever", 0, 0); err != nil {
		t.Fatal(err)
	}
	client := signedIn(t, db, user)
	request := httptest.NewRequest(http.MethodGet, "https://cfg.test/resources/"+resource.ID, nil)
	request.Header.Set("Accept-Language", "en")
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: client.session})
	request.AddCookie(&http.Cookie{Name: csrfCookie, Value: client.csrf})
	response := httptest.NewRecorder()
	client.app.handler.ServeHTTP(response, request)
	page := response.Body.String()

	rows := strings.Split(page, "<li")
	var soon, forever string
	for _, row := range rows {
		switch {
		case strings.Contains(row, ">soon<"):
			soon = row
		case strings.Contains(row, ">forever<"):
			forever = row
		}
	}
	if !strings.Contains(soon, `style="--end: 3`) || !strings.Contains(soon, `class="st off after"`) || !strings.Contains(soon, `class="acts after"`) {
		t.Errorf("the expiring row does not carry its ended state: %s", soon)
	}
	if strings.Contains(forever, "--end") || strings.Contains(forever, " after") {
		t.Errorf("a link that never expires has no ended state: %s", forever)
	}
}
