package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A share address is for opening, downloading and previewing. Another site's
// page loading it as one of its own elements is image hosting on this
// service's account, and is refused before the link is touched.
func TestShareAddressCannotBeEmbeddedCrossSite(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	share, err := db.CreateShare(ctx, user.ID, resource.ID, "", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	app := newTestApp(db, user.GitHubID)
	address := shareAddress(share.Token, resource.Filename)

	fetch := func(site, mode, dest string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "https://cfg.test"+address, nil)
		for header, value := range map[string]string{"Sec-Fetch-Site": site, "Sec-Fetch-Mode": mode, "Sec-Fetch-Dest": dest} {
			if value != "" {
				request.Header.Set(header, value)
			}
		}
		response := httptest.NewRecorder()
		app.handler.ServeHTTP(response, request)
		return response
	}
	uses := func() int {
		t.Helper()
		links, err := db.ListShares(ctx, user.ID, resource.ID, time.Now().UTC())
		if err != nil || len(links) != 1 {
			t.Fatalf("list shares: %v %d", err, len(links))
		}
		return links[0].UsedCount
	}
	logged := func() int {
		t.Helper()
		rows, err := db.ListAccess(ctx, user.ID, "", "", 100)
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}

	for _, embed := range []struct{ name, mode, dest string }{
		{"img", "no-cors", "image"},
		{"video", "no-cors", "video"},
		{"audio", "no-cors", "audio"},
		{"iframe", "navigate", "iframe"},
		{"fetch", "cors", "empty"},
		{"script", "no-cors", "script"},
	} {
		response := fetch("cross-site", embed.mode, embed.dest)
		if response.Code != http.StatusForbidden {
			t.Errorf("a cross-site %s must be refused, got %d", embed.name, response.Code)
		}
	}
	if used, rows := uses(), logged(); used != 0 || rows != 0 {
		t.Fatalf("refused embeds must cost nothing: %d uses, %d log rows", used, rows)
	}

	for _, allowed := range []struct{ name, site, mode, dest string }{
		{"a link clicked on another site", "cross-site", "navigate", "document"},
		{"an address typed or bookmarked", "none", "navigate", "document"},
		{"the media player's own fetch", "same-origin", "cors", "empty"},
		{"curl or a chat preview", "", "", ""},
	} {
		response := fetch(allowed.site, allowed.mode, allowed.dest)
		if response.Code != http.StatusOK {
			t.Errorf("%s must be served, got %d", allowed.name, response.Code)
			continue
		}
		if response.Header().Get("Cross-Origin-Resource-Policy") != "same-origin" {
			t.Errorf("%s: the body must still refuse to render inside another site", allowed.name)
		}
	}
	if used := uses(); used != 4 {
		t.Fatalf("each served request is one use, got %d", used)
	}
}

// Refusals are counted where an operator will see them, without a database row.
func TestRefusedEmbedsAreCounted(t *testing.T) {
	var probes probeLog
	start := time.Now()
	probes.record(probeEmbed, start)
	if line := probes.record(probeEmbed, start.Add(probeWindow)); !strings.Contains(line, "embed=2") {
		t.Fatalf("the probe line does not count embeds: %q", line)
	}
}
