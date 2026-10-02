package web

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"plainmote/internal/store"
)

// TestNoDeliveryWithoutARecord follows a link through the public handler:
// every answer that spends a use - a GET, a HEAD, a 304 - leaves exactly one
// row, and when no row can be written the content does not go out at all.
func TestNoDeliveryWithoutARecord(t *testing.T) {
	ctx := context.Background()
	dsn := testDatabaseURL(t)
	db, err := store.Open(ctx, dsn, bytes.Repeat([]byte{7}, 32), newMemoryBlobs())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	user, err := db.UpsertUser(ctx, "100", "alice", "Alice", "")
	if err != nil {
		t.Fatal(err)
	}
	resource, err := db.CreateResource(ctx, user.ID, "Secret", "secret.conf", []byte("SECRET-BODY\n"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	link, err := db.CreateShare(ctx, user.ID, resource.ID, "web-01", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	app := newTestApp(db, user.GitHubID)
	address := "https://cfg.test" + shareAddress(link.Token, resource.Filename)
	fetch := func(method, etag string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, address, nil)
		if etag != "" {
			request.Header.Set("If-None-Match", etag)
		}
		response := httptest.NewRecorder()
		app.handler.ServeHTTP(response, request)
		return response
	}
	uses := func() int {
		live, err := db.ListShares(ctx, user.ID, resource.ID, time.Now().UTC())
		if err != nil || len(live) != 1 {
			t.Fatalf("shares: %+v %v", live, err)
		}
		return live[0].UsedCount
	}

	first := fetch(http.MethodGet, "")
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), "SECRET-BODY") {
		t.Fatalf("GET: %d", first.Code)
	}
	if head := fetch(http.MethodHead, ""); head.Code != http.StatusOK {
		t.Fatalf("HEAD: %d", head.Code)
	}
	if cached := fetch(http.MethodGet, first.Header().Get("ETag")); cached.Code != http.StatusNotModified {
		t.Fatalf("conditional GET: %d", cached.Code)
	}
	rows, err := db.ListAccess(ctx, user.ID, resource.ID, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[int]int{}
	for _, row := range rows {
		statuses[row.Status]++
		if row.Outcome != store.OutcomeSuccess || row.LinkName != "web-01" {
			t.Fatalf("a row does not describe its use: %+v", row)
		}
	}
	if len(rows) != 3 || uses() != 3 || statuses[http.StatusOK] != 2 || statuses[http.StatusNotModified] != 1 {
		t.Fatalf("three uses, %d spent, rows by status %v", uses(), statuses)
	}

	// The log cannot be written: nothing may be delivered, nothing spent.
	if _, err := pool.Exec(ctx, `
CREATE FUNCTION refuse_access_log() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'access log unavailable'; END $$;
CREATE TRIGGER refuse_access_log BEFORE INSERT ON access_logs FOR EACH ROW EXECUTE FUNCTION refuse_access_log();
`); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		refused := fetch(method, "")
		if refused.Code < 500 || strings.Contains(refused.Body.String(), "SECRET-BODY") {
			t.Fatalf("%s without a record answered %d %q", method, refused.Code, refused.Body.String())
		}
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER refuse_access_log ON access_logs`); err != nil {
		t.Fatal(err)
	}
	if got := uses(); got != 3 {
		t.Fatalf("uses without a record were spent: %d", got)
	}
}
