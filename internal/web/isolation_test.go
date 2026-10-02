package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"plainmote/internal/store"
)

// TestAnotherAccountCannotReachAResource walks every page, part and action
// that names a resource, as an account that does not own it: plainly and the
// way parts.js asks, by the resource's own id and with ids of its links and
// versions planted on the intruder's own resource. Nothing of the owner's may
// come back - not the name, the content of any version, a link address - and
// nothing the owner has may change.
func TestAnotherAccountCannotReachAResource(t *testing.T) {
	db, owner, resource := testDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// The owner's resource: a distinctive name, two versions with
	// distinctive content, a live link with terms and an ended one.
	if err := db.UpdateResource(ctx, owner.ID, resource.ID, "Owner Secret Name", "owner-secret.conf", []byte("OWNER-SECRET-V1\n"), "utf-8", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateResource(ctx, owner.ID, resource.ID, "Owner Secret Name", "owner-secret.conf", []byte("OWNER-SECRET-V2\n"), "utf-8", ""); err != nil {
		t.Fatal(err)
	}
	live, err := db.CreateShare(ctx, owner.ID, resource.ID, "owner link note", time.Hour, 10)
	if err != nil {
		t.Fatal(err)
	}
	ended, err := db.CreateShare(ctx, owner.ID, resource.ID, "owner ended note", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RevokeLink(ctx, owner.ID, resource.ID, ended.ID); err != nil {
		t.Fatal(err)
	}
	before := ownerState(t, db, owner, resource.ID, now)

	intruder, err := db.UpsertUser(ctx, "200", "mallory", "Mallory", "")
	if err != nil {
		t.Fatal(err)
	}
	own, err := db.CreateResource(ctx, intruder.ID, "Mallory's", "m.conf", []byte("mine\n"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	client := newVersionClient(t, db, intruder)

	secrets := []string{"Owner Secret Name", "owner-secret", "OWNER-SECRET", "answer=42", live.Token, ended.Token, "owner link note", "owner ended note", "alice"}
	check := func(label string, response interface {
		Header() http.Header
	}, body string, code int) {
		t.Helper()
		seen := body + " " + response.Header().Get("Location")
		for _, secret := range secrets {
			if strings.Contains(seen, secret) {
				t.Errorf("%s: %d answer carries %q", label, code, secret)
			}
		}
		if code >= 200 && code < 300 && strings.Contains(label, resource.ID) {
			t.Errorf("%s: answered %d", label, code)
		}
	}

	base := "/resources/" + resource.ID
	pages := []string{
		base, base + "?delete=1", base + "?share=" + live.ID, base + "?saved=3", base + "?created=1",
		base + "?error=x&share=" + live.ID, base + "/raw",
		base + "/versions", base + "/versions?remove=1", base + "/versions/1", base + "/versions/1?restore=1",
		base + "/versions/1/raw", base + "/versions/compare?from=1&to=2",
		// The owner's link and version numbers, asked of the intruder's own resource.
		"/resources/" + own.ID + "?share=" + live.ID,
		"/resources/" + own.ID + "?share=" + ended.ID,
	}
	for _, asParts := range []bool{false, true} {
		client.asParts = asParts
		for _, target := range pages {
			response := client.do(http.MethodGet, target, nil)
			check("GET "+target, response, response.Body.String(), response.Code)
		}

		contentForm := func() url.Values {
			return url.Values{"name": {"taken"}, "filename": {"taken.conf"}, "content": {"MALLORY-WROTE\n"},
				"content_encoding": {"utf-8"}, "content_eol": {"lf"}, "base_version": {"3"}}
		}
		posts := []struct {
			target string
			form   url.Values
		}{
			{base, contentForm()},
			{base, url.Values{"action": {"delete"}}},
			{base + "/share", url.Values{"action": {"create"}}},
			{base + "/share", url.Values{"action": {"update"}, "share_id": {live.ID}, "name": {"taken"}, "ttl": {"never"}, "uses": {"0"}}},
			{base + "/share", url.Values{"action": {"update"}, "share_id": {live.ID}, "ttl": {"custom"}, "ttl_custom": {"bad"}}},
			{base + "/share", url.Values{"action": {"revoke"}, "share_id": {live.ID}}},
			{base + "/share", url.Values{"action": {"delete"}, "share_id": {ended.ID}}},
			{base + "/share", url.Values{"action": {"revoke_all"}}},
			{base + "/versions/1", url.Values{"action": {"restore"}}},
			{base + "/versions/1", url.Values{"action": {"copy"}}},
			{base + "/versions/1", url.Values{"action": {"delete"}}},
			// The owner's links, acted on through the intruder's own resource,
			// including the refusal that reopens a share's dialog.
			{"/resources/" + own.ID + "/share", url.Values{"action": {"update"}, "share_id": {live.ID}, "name": {"taken"}, "ttl": {"never"}, "uses": {"0"}}},
			{"/resources/" + own.ID + "/share", url.Values{"action": {"update"}, "share_id": {live.ID}, "ttl": {"custom"}, "ttl_custom": {"bad"}}},
			{"/resources/" + own.ID + "/share", url.Values{"action": {"revoke"}, "share_id": {live.ID}}},
			{"/resources/" + own.ID + "/share", url.Values{"action": {"delete"}, "share_id": {ended.ID}}},
		}
		for _, post := range posts {
			response := client.do(http.MethodPost, post.target, post.form)
			label := "POST " + post.target + " " + post.form.Get("action")
			check(label, response, response.Body.String(), response.Code)
			if response.Code == http.StatusSeeOther {
				// A redirect is followed the way the browser and parts.js do.
				location := response.Header().Get("Location")
				followed := client.do(http.MethodGet, location, nil)
				check(label+" -> "+location, followed, followed.Body.String(), followed.Code)
			}
		}
	}
	client.asParts = false

	// Signed out, and signed in as the owner but with a token from elsewhere:
	// the parts header opens no door the plain request does not.
	for _, asParts := range []bool{false, true} {
		for _, target := range pages[:11] {
			request := httptest.NewRequest(http.MethodGet, "https://cfg.test"+target, nil)
			if asParts {
				request.Header.Set(partsHeader, "1")
			}
			response := httptest.NewRecorder()
			client.app.handler.ServeHTTP(response, request)
			check("signed out GET "+target, response, response.Body.String(), response.Code)
		}
		ownerClient := newVersionClient(t, db, owner)
		ownerClient.asParts = asParts
		ownerClient.csrf = client.csrf // the session is the owner's, the token is not
		for _, post := range []url.Values{
			{"action": {"revoke_all"}},
			{"action": {"update"}, "share_id": {live.ID}, "name": {"taken"}, "ttl": {"never"}, "uses": {"0"}},
		} {
			response := ownerClient.do(http.MethodPost, base+"/share", post)
			if response.Code != http.StatusForbidden {
				t.Errorf("a foreign token on %s answered %d", post.Get("action"), response.Code)
			}
		}
		saved := ownerClient.do(http.MethodPost, base, url.Values{"name": {"taken"}, "content": {"MALLORY-WROTE\n"}, "base_version": {"3"}})
		if saved.Code != http.StatusForbidden {
			t.Errorf("a foreign token on a save answered %d", saved.Code)
		}
	}

	if after := ownerState(t, db, owner, resource.ID, now); after != before {
		t.Fatalf("the owner's resource changed:\nbefore %+v\nafter  %+v", before, after)
	}
	resources, _, err := db.ListResources(ctx, intruder.ID, "", 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(resources) != 1 || resources[0].ID != own.ID {
		t.Fatalf("the intruder ended up with %d resources", len(resources))
	}
}

// ownerResource is what the isolation test compares before and after.
type ownerResource struct {
	name, filename, content   string
	version, history          int
	liveLinks, endedLinks     int
	linkName, linkToken       string
	linkUses, linkMaxUses     int
	linkExpires, linkTermsAt  string
	resourceCount, versionOne int
}

func ownerState(t *testing.T, db *store.Store, owner User, resourceID string, now time.Time) ownerResource {
	t.Helper()
	ctx := context.Background()
	resource, err := db.ResourceForOwner(ctx, owner.ID, resourceID)
	if err != nil {
		t.Fatalf("the owner's resource: %v", err)
	}
	content, err := db.ReadContent(ctx, resource)
	if err != nil {
		t.Fatal(err)
	}
	history, err := db.HistoryCount(ctx, owner.ID, resourceID)
	if err != nil {
		t.Fatal(err)
	}
	live, err := db.ListShares(ctx, owner.ID, resourceID, now)
	if err != nil {
		t.Fatal(err)
	}
	ended, err := db.ListEndedShares(ctx, owner.ID, resourceID, now, now.Add(-time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	all, _, err := db.ListResources(ctx, owner.ID, "", 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	state := ownerResource{
		name: resource.Name, filename: resource.Filename, content: string(content),
		version: resource.Version, history: history,
		liveLinks: len(live), endedLinks: len(ended), resourceCount: len(all),
	}
	if one, err := db.VersionForOwner(ctx, owner.ID, resourceID, 1); err == nil {
		state.versionOne = one.Number
	}
	if len(live) > 0 {
		link := live[0]
		state.linkName, state.linkToken, state.linkUses, state.linkMaxUses = link.Name, link.Token, link.UsedCount, link.MaxUses
		state.linkTermsAt = link.TermsAt.String()
		if link.ExpiresAt != nil {
			state.linkExpires = link.ExpiresAt.String()
		}
	}
	return state
}
