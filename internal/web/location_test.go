package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"plainmote/internal/store"
)

func locationRequest(peer string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "https://cfg.test/d/x", nil)
	r.RemoteAddr = peer + ":40000"
	for name, value := range headers {
		r.Header.Set(name, value)
	}
	return r
}

var cloudflareHeaders = map[string]string{
	"CF-IPCountry": "de",
	"CF-Region":    "Bavaria",
	"CF-IPCity":    "München",
}

func TestVisitorLocationIsBelievedOnlyFromATrustedProxy(t *testing.T) {
	proxies := []netip.Prefix{netip.MustParsePrefix("172.18.0.2/32")}
	on := &App{cfg: Config{TrustedProxies: proxies, CloudflareLocation: true}}
	off := &App{cfg: Config{TrustedProxies: proxies}}

	want := store.Location{Country: "DE", Region: "Bavaria", City: "München"}
	if got := on.visitorLocation(locationRequest("172.18.0.2", cloudflareHeaders)); got != want {
		t.Fatalf("from the trusted proxy: got %+v, want %+v", got, want)
	}
	// Anyone else can name any place they like.
	if got := on.visitorLocation(locationRequest("203.0.113.9", cloudflareHeaders)); got != (store.Location{}) {
		t.Fatalf("headers from an untrusted peer were believed: %+v", got)
	}
	// Switched off, a trusted proxy that merely passes headers along (nginx
	// without Cloudflare) does not make the client's own headers true.
	if got := off.visitorLocation(locationRequest("172.18.0.2", cloudflareHeaders)); got != (store.Location{}) {
		t.Fatalf("headers were believed with the switch off: %+v", got)
	}
}

func TestVisitorLocationKeepsOnlyWhatIsAPlace(t *testing.T) {
	app := &App{cfg: Config{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}, CloudflareLocation: true}}
	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    store.Location
	}{
		{"unknown country", map[string]string{"CF-IPCountry": "XX", "CF-IPCity": "Nowhere"}, store.Location{}},
		{"no country", map[string]string{"CF-IPCity": "Berlin"}, store.Location{}},
		{"not a code", map[string]string{"CF-IPCountry": "<b>", "CF-IPCity": "Berlin"}, store.Location{}},
		{"tor", map[string]string{"CF-IPCountry": "T1"}, store.Location{Country: "T1"}},
		{"controls dropped", map[string]string{"CF-IPCountry": "US", "CF-Region": "New\tYork​", "CF-IPCity": "  New   York "}, store.Location{Country: "US", Region: "New York", City: "New York"}},
		{"invalid UTF-8 dropped", map[string]string{"CF-IPCountry": "FR", "CF-IPCity": "Par\xffis"}, store.Location{Country: "FR", City: "Paris"}},
		{"too long to be a name", map[string]string{"CF-IPCountry": "FR", "CF-IPCity": strings.Repeat("a", 200)}, store.Location{Country: "FR"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := app.visitorLocation(locationRequest("127.0.0.1", tc.headers)); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestLocationText(t *testing.T) {
	for _, tc := range []struct {
		locale   string
		location store.Location
		want     string
	}{
		{"zh-CN", store.Location{Country: "CN", Region: "Guangdong", City: "Shenzhen"}, "中国 · Guangdong · Shenzhen"},
		{"zh-TW", store.Location{Country: "DE"}, "德國"},
		{"ja", store.Location{Country: "JP", Region: "Tokyo", City: "Tokyo"}, "日本 · Tokyo"},
		{"en", store.Location{Country: "US", City: "Ashburn"}, "United States · Ashburn"},
		{"xx", store.Location{Country: "FR"}, "France"},
		{"en", store.Location{Country: "T1"}, "Tor"},
		{"en", store.Location{}, ""},
	} {
		if got := locationText(tc.locale, tc.location); got != tc.want {
			t.Errorf("locationText(%s, %+v) = %q, want %q", tc.locale, tc.location, got, tc.want)
		}
	}
	for _, tc := range []struct {
		location store.Location
		want     string
	}{
		{store.Location{Country: "US", Region: "California", City: "Los Angeles"}, "United States · Los Angeles"},
		{store.Location{Country: "US", Region: "California"}, "United States · California"},
		{store.Location{Country: "SG"}, "Singapore"},
	} {
		if got := locationShort("en", tc.location); got != tc.want {
			t.Errorf("locationShort(%+v) = %q, want %q", tc.location, got, tc.want)
		}
	}
}

// TestDeliveryRecordsTheVisitorLocation follows a link through the public
// handler behind a trusted proxy, then reads the row back as its owner sees
// it - and checks a second owner's history shows none of it.
func TestDeliveryRecordsTheVisitorLocation(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	link, err := db.CreateShare(ctx, user.ID, resource.ID, "", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.UpsertUser(ctx, "200", "bob", "Bob", "")
	if err != nil {
		t.Fatal(err)
	}
	app := newTestApp(db)
	app.cfg.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("172.18.0.2/32")}
	app.cfg.CloudflareLocation = true

	address := "https://cfg.test" + shareAddress(link.Token, resource.Filename)
	visit := func(peer string) {
		request := httptest.NewRequest(http.MethodGet, address, nil)
		request.RemoteAddr = peer + ":40000"
		request.Header.Set("CF-Connecting-IP", "198.51.100.7")
		for name, value := range cloudflareHeaders {
			request.Header.Set(name, value)
		}
		response := httptest.NewRecorder()
		app.handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("delivery: %d", response.Code)
		}
	}
	visit("172.18.0.2")
	visit("203.0.113.9")

	rows, err := db.ListAccess(ctx, user.ID, resource.ID, "", 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows: %+v %v", rows, err)
	}
	located := map[string]store.Location{}
	for _, row := range rows {
		located[row.RemoteIP] = row.Location
	}
	if got := located["198.51.100.7"]; got != (store.Location{Country: "DE", Region: "Bavaria", City: "München"}) {
		t.Fatalf("through the proxy: %+v", got)
	}
	if got, ok := located["203.0.113.9"]; !ok || got != (store.Location{}) {
		t.Fatalf("direct, the client's own headers were kept: %+v", located)
	}

	page := func(owner store.User) string {
		session, csrf, _, err := db.CreateSession(ctx, owner.ID, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodGet, "https://cfg.test/logs", nil)
		request.Header.Set("Accept-Language", "zh-CN")
		request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
		request.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrf})
		response := httptest.NewRecorder()
		app.handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("logs page: %d", response.Code)
		}
		return response.Body.String()
	}
	if body := page(user); !strings.Contains(body, `title="德国 · Bavaria · München">德国 · München</span>`) ||
		!strings.Contains(body, `198.51.100.7 <span class="mut" title="根据 IP 地址估算">· 德国 · Bavaria · München</span>`) {
		t.Fatal("the owner's history does not show the location")
	}
	if body := page(other); strings.Contains(body, "München") || strings.Contains(body, "198.51.100.7") {
		t.Fatal("another account's history shows this visitor")
	}
}
