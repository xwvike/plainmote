package web

import (
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"plainmote/internal/store"
)

func TestPublicDeliveryIgnoresRangeAndAlwaysCounts(t *testing.T) {
	for _, ext := range []string{"txt", "png", "zip", "bin", "mp3", "mp4"} {
		t.Run(ext, func(t *testing.T) {
			db, user, _ := testDatabase(t)
			ctx := context.Background()
			payload := []byte("01234\x0056789")
			if ext == "bin" {
				payload = []byte{0, 255, 254, 1}
			}
			resource, err := db.CreateResource(ctx, user.ID, "file", "file."+ext, payload, "", "")
			if err != nil {
				t.Fatal(err)
			}
			share, err := db.CreateShare(ctx, user.ID, resource.ID, "once", time.Hour, 1)
			if err != nil {
				t.Fatal(err)
			}
			app := newTestApp(db)
			target := "https://cfg.test" + shareAddress(share.Token, resource.Filename)
			r := httptest.NewRequest(http.MethodGet, target, nil)
			r.Header.Set("Range", "bytes=0-1")
			first := httptest.NewRecorder()
			app.Handler().ServeHTTP(first, r)
			if first.Code != http.StatusOK || first.Body.String() != string(payload) {
				t.Fatalf("full delivery: %d %q", first.Code, first.Body.String())
			}
			if first.Header().Get("Accept-Ranges") != "none" || first.Header().Get("Content-Range") != "" || len(first.Result().Cookies()) != 0 {
				t.Fatal("direct delivery offered a range or continuation")
			}
			if ext != "txt" && ext != "png" && !strings.HasPrefix(first.Header().Get("Content-Disposition"), "attachment;") {
				t.Fatal("opaque resource should download")
			}
			for _, header := range []string{"", "range", "media", "navigate"} {
				r := httptest.NewRequest(http.MethodGet, target+"?raw=1&grant=old-grant", nil)
				r.AddCookie(&http.Cookie{Name: "plainmote_delivery", Value: "old-cookie"})
				if header == "range" || header == "navigate" {
					r.Header.Set("Range", "bytes=0-")
				}
				if header == "media" {
					r.Header.Set("Sec-Fetch-Dest", "video")
				}
				if header == "navigate" {
					r.Header.Set("Sec-Fetch-Mode", "navigate")
					r.Header.Set("Accept", "text/html")
				}
				w := httptest.NewRecorder()
				app.Handler().ServeHTTP(w, r)
				if w.Code != http.StatusUnauthorized {
					t.Fatalf("%s bypass: %d", header, w.Code)
				}
			}
		})
	}
}

func TestSingleUseBrowserMediaFetch(t *testing.T) {
	for _, filename := range []string{"audio.mp3", "movie.mp4", "视频 #1?.mp4"} {
		t.Run(filename, func(t *testing.T) {
			db, user, _ := testDatabase(t)
			ctx := context.Background()
			payload := []byte("01234\x0056789")
			resource, err := db.CreateResource(ctx, user.ID, "media", filename, payload, "", "")
			if err != nil {
				t.Fatal(err)
			}
			share, err := db.CreateShare(ctx, user.ID, resource.ID, "once", time.Hour, 1)
			if err != nil {
				t.Fatal(err)
			}
			app := newTestApp(db)
			get := func(target, accept string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodGet, "https://cfg.test"+target, nil)
				r.Header.Set("Accept", accept)
				r.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
				w := httptest.NewRecorder()
				app.Handler().ServeHTTP(w, r)
				return w
			}
			entry := shareAddress(share.Token, resource.Filename)
			page := get(entry, "text/html,application/xhtml+xml,*/*;q=0.8")
			if page.Code != http.StatusOK || !strings.HasPrefix(page.Header().Get("Content-Type"), "text/html") {
				t.Fatalf("player shell: %d", page.Code)
			}
			if len(page.Result().Cookies()) != 0 || page.Header().Get("Cache-Control") != "no-store" || page.Header().Get("Referrer-Policy") != "no-referrer" {
				t.Fatal("player cached or issued a credential")
			}
			csp := page.Header().Get("Content-Security-Policy")
			for _, directive := range []string{"default-src 'none'", "media-src blob:", "frame-ancestors 'none'"} {
				if !strings.Contains(csp, directive) {
					t.Fatalf("media player CSP lost %q: %q", directive, csp)
				}
			}
			if player := page.Body.String(); !strings.Contains(player, `<html lang="en">`) ||
				!strings.Contains(player, `data-loading="Loading…"`) ||
				!strings.Contains(player, `<a class="download" href="`+entry+`?raw=1" download>Download file</a>`) || strings.Contains(player, "media-language") ||
				strings.Contains(player, "正在加载") {
				t.Fatal("media player must remain English without a language switch")
			}
			match := regexp.MustCompile(`data-media-source="([^"]+)"`).FindStringSubmatch(page.Body.String())
			if len(match) != 2 {
				t.Fatal("missing full-body source")
			}
			source := html.UnescapeString(match[1])
			shares, err := db.ListShares(ctx, user.ID, resource.ID, time.Now().UTC())
			if err != nil || len(shares) != 1 || shares[0].UsedCount != 0 {
				t.Fatal("shell consumed the only use")
			}
			body := get(source, "*/*")
			if body.Code != http.StatusOK || body.Body.String() != string(payload) {
				t.Fatalf("media fetch: %d %q", body.Code, body.Body.String())
			}
			logs, err := db.ListAccess(ctx, user.ID, resource.ID, store.OutcomeSuccess, 10)
			if err != nil || len(logs) != 1 {
				t.Fatalf("one load should record one delivery: %d %v", len(logs), err)
			}
			// Refreshing can load an empty shell but can never fetch more bytes.
			get(entry, "text/html")
			if refreshed := get(source, "*/*"); refreshed.Code != http.StatusUnauthorized {
				t.Fatalf("refresh bypass: %d", refreshed.Code)
			}
			if err := db.UpdateResource(ctx, user.ID, resource.ID, "replacement", resource.Filename, []byte("new\x00media"), "", ""); err != nil {
				t.Fatal(err)
			}
			if updated := get(source, "*/*"); updated.Code != http.StatusUnauthorized {
				t.Fatal("spent link revealed updated content")
			}
		})
	}
}

func TestBrowserMediaDownloadAndLifecycle(t *testing.T) {
	for _, scenario := range []string{"download", "revoked", "expired", "deleted"} {
		t.Run(scenario, func(t *testing.T) {
			db, user, _ := testDatabase(t)
			ctx := context.Background()
			resource, err := db.CreateResource(ctx, user.ID, "media", "a.mp4", []byte("vid\x00eo"), "", "")
			if err != nil {
				t.Fatal(err)
			}
			share, err := db.CreateShare(ctx, user.ID, resource.ID, "one", time.Hour, 1)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "revoked":
				err = db.RevokeLink(ctx, user.ID, resource.ID, share.ID)
			case "expired":
				err = db.UpdateShare(ctx, user.ID, resource.ID, share.ID, "expired", time.Nanosecond, 1)
			case "deleted":
				err = db.DeleteResource(ctx, user.ID, resource.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			app := newTestApp(db)
			r := httptest.NewRequest(http.MethodGet, "https://cfg.test"+shareAddress(share.Token, resource.Filename)+"?download=1", nil)
			r.Header.Set("Accept", "text/html")
			w := httptest.NewRecorder()
			app.Handler().ServeHTTP(w, r)
			if scenario == "download" {
				if w.Code != http.StatusOK || w.Body.String() != "vid\x00eo" || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment;") {
					t.Fatal("explicit browser download returned a shell")
				}
			} else if w.Code != http.StatusUnauthorized {
				t.Fatalf("%s was allowed: %d", scenario, w.Code)
			}
		})
	}
}

// Without scripts a browser opening a media link downloads it straight away.
// Only a browser navigation gets the refresh that does it: a preview crawler
// sends no Fetch Metadata, and following the refresh would spend a use.
func TestMediaPlayerSavesDirectlyWithoutScriptForBrowsersOnly(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	resource, err := db.CreateResource(ctx, user.ID, "tone", "tone.mp3", []byte("01234\x0056789"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	share, err := db.CreateShare(ctx, user.ID, resource.ID, "", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	app := newTestApp(db)
	entry := shareAddress(share.Token, resource.Filename)
	open := func(browser bool) string {
		r := httptest.NewRequest(http.MethodGet, "https://cfg.test"+entry, nil)
		r.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
		if browser {
			r.Header.Set("Sec-Fetch-Mode", "navigate")
			r.Header.Set("Sec-Fetch-Dest", "document")
			r.Header.Set("Sec-Fetch-Site", "none")
		}
		w := httptest.NewRecorder()
		app.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("player shell: %d", w.Code)
		}
		return w.Body.String()
	}
	refresh := `<noscript><meta http-equiv="refresh" content="0; url=` + entry + `?raw=1"></noscript>`
	if page := open(true); !strings.Contains(page, refresh) {
		t.Fatalf("a browser opening the link must get the no-script download: %s", page)
	}
	if page := open(false); strings.Contains(page, "http-equiv") {
		t.Fatal("a client without Fetch Metadata must not be handed a redirect to follow")
	}
	links, err := db.ListShares(ctx, user.ID, resource.ID, time.Now().UTC())
	if err != nil || len(links) != 1 || links[0].UsedCount != 0 {
		t.Fatalf("opening the player page must not spend the single use: %+v %v", links, err)
	}
}
