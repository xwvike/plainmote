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
	// Every attributable refusal is kept, not sampled: the owner is the one who
	// needs to see that their link is being hit after it expired.
	for i := 1; i <= 3; i++ {
		if _, err := db.ConsumeToken(ctx, share.Token, meta, later); err != nil {
			t.Fatal(err)
		}
		if got := countAccessLogs(t, db, OutcomeExpired); got != i {
			t.Fatalf("expected %d expired rows, found %d", i, got)
		}
	}

	logs, err := db.ListAccess(ctx, user.ID, resource.ID, OutcomeExpired, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 3 {
		t.Fatalf("the owner must be able to read every refusal, found %d", len(logs))
	}
}
