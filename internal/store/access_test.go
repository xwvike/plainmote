package store

import (
	"context"
	"testing"
	"time"
)

func countAccessLogs(t *testing.T, db *Store, outcome string) int {
	t.Helper()
	var total int
	query := `SELECT COUNT(*) FROM access_logs`
	args := []any{}
	if outcome != "" {
		query += ` WHERE outcome = $1`
		args = append(args, outcome)
	}
	if err := db.db.QueryRow(context.Background(), query, args...).Scan(&total); err != nil {
		t.Fatal(err)
	}
	return total
}

// TestRefusalsAreRecordedWhenTheyHaveAnOwner draws the line the log lives on:
// a refusal that belongs to a link is written every time, and a token nobody
// issued is not written at all, because no owner could ever read that row.
func TestRefusalsAreRecordedWhenTheyHaveAnOwner(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	meta := RequestMeta{Method: "GET", RemoteIP: "203.0.113.9"}

	unknown, err := generateToken()
	if err != nil {
		t.Fatal(err)
	}
	before := countAccessLogs(t, db, "")
	result, err := db.ConsumeToken(ctx, unknown, meta, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if result.Allowed || result.Reason != ReasonInvalid {
		t.Fatalf("an unissued token must be refused as invalid, got allowed=%v reason=%q", result.Allowed, result.Reason)
	}
	if after := countAccessLogs(t, db, ""); after != before {
		t.Fatalf("an unowned refusal must not reach access_logs, rows went %d -> %d", before, after)
	}

	share, err := db.CreateShare(ctx, user.ID, resource.ID, "临时", 30*time.Minute, 0)
	if err != nil {
		t.Fatal(err)
	}
	later := time.Now().UTC().Add(time.Hour)
	// One caller hammering a dead link folds into one row, and the row counts
	// every attempt: the earlier sampling attempt dropped them instead.
	// A second apart, so the run stays inside one fold window and the row has
	// to move its occurred_at forward as it absorbs each attempt.
	for i := 0; i < 50; i++ {
		if _, err := db.ConsumeToken(ctx, share.Token, meta, later.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if got := countAccessLogs(t, db, OutcomeExpired); got != 1 {
		t.Fatalf("one caller in one window is one row, found %d", got)
	}

	// A second caller is a different fact, so it gets its own row.
	if _, err := db.ConsumeToken(ctx, share.Token, RequestMeta{Method: "GET", RemoteIP: "198.51.100.7"}, later); err != nil {
		t.Fatal(err)
	}

	logs, err := db.ListAccess(ctx, user.ID, resource.ID, OutcomeExpired, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 2 {
		t.Fatalf("each caller must be recorded separately, found %d rows", len(logs))
	}
	total := 0
	for _, item := range logs {
		total += item.Hits
	}
	if total != 51 {
		t.Fatalf("folding must not lose attempts, counted %d of 51", total)
	}
	for _, item := range logs {
		if item.Hits > 1 && !item.FirstAt.Before(item.OccurredAt) {
			t.Fatalf("a folded row must keep when the run started, got %s -> %s", item.FirstAt, item.OccurredAt)
		}
	}
}

// TestFoldingIsBoundedByItsWindow keeps the fold from becoming an unbounded
// bucket: once the window passes, the next attempt starts a new row, so the
// log still shows when something came back.
func TestFoldingIsBoundedByItsWindow(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	meta := RequestMeta{Method: "GET", RemoteIP: "203.0.113.9"}

	share, err := db.CreateShare(ctx, user.ID, resource.ID, "临时", time.Minute, 0)
	if err != nil {
		t.Fatal(err)
	}
	first := time.Now().UTC().Add(time.Hour)
	if _, err := db.ConsumeToken(ctx, share.Token, meta, first); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ConsumeToken(ctx, share.Token, meta, first.Add(2*accessFoldWindow)); err != nil {
		t.Fatal(err)
	}
	if got := countAccessLogs(t, db, OutcomeExpired); got != 2 {
		t.Fatalf("a hit past the window starts a new row, found %d", got)
	}
}

func TestListAccessPageReturnsTotalAndStablePages(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	event := AccessEvent{
		OwnerID: user.ID, ResourceID: resource.ID, ResourceName: resource.Name,
		Outcome: OutcomeSuccess, Status: 200,
	}
	for i := 0; i < 25; i++ {
		if err := db.RecordAccess(ctx, event, RequestMeta{Method: "GET", Path: "/d/example"}); err != nil {
			t.Fatal(err)
		}
	}

	first, total, err := db.ListAccessPage(ctx, user.ID, resource.ID, OutcomeSuccess, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 25 || len(first) != 10 {
		t.Fatalf("first page: total=%d rows=%d, want 25 and 10", total, len(first))
	}
	last, total, err := db.ListAccessPage(ctx, user.ID, resource.ID, OutcomeSuccess, 10, 20)
	if err != nil {
		t.Fatal(err)
	}
	if total != 25 || len(last) != 5 {
		t.Fatalf("last page: total=%d rows=%d, want 25 and 5", total, len(last))
	}
	for _, older := range last {
		for _, newer := range first {
			if older.ID == newer.ID {
				t.Fatalf("row %s appeared on two pages", older.ID)
			}
		}
	}
}

// TestDeliveriesAreNeverFolded keeps the fold off the outcome the owner
// published the link to get.
func TestDeliveriesAreNeverFolded(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	meta := RequestMeta{Method: "GET", RemoteIP: "203.0.113.9"}
	event := AccessEvent{
		OwnerID: user.ID, ResourceID: resource.ID, ResourceName: resource.Name,
		LinkID: "", LinkName: "长期", Outcome: OutcomeSuccess, Status: 200, Detail: "link accepted",
	}
	share, err := db.CreateShare(ctx, user.ID, resource.ID, "长期", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	event.LinkID = share.ID
	for i := 0; i < 3; i++ {
		if err := db.RecordAccess(ctx, event, meta); err != nil {
			t.Fatal(err)
		}
	}
	if got := countAccessLogs(t, db, OutcomeSuccess); got != 3 {
		t.Fatalf("every delivery must be its own row, found %d", got)
	}
}
