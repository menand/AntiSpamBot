package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestPendingRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	exp := time.Now().Add(30 * time.Second).Truncate(time.Second)
	p := PendingRow{ChatID: 1, UserID: 2, MessageID: 100, CorrectIdx: 3, ExpiresAt: exp}
	if err := db.PutPending(ctx, p); err != nil {
		t.Fatal(err)
	}

	loaded, err := db.LoadAllPending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 {
		t.Fatalf("got %d rows, want 1", len(loaded))
	}
	got := loaded[0]
	if got.ChatID != 1 || got.UserID != 2 || got.MessageID != 100 || got.CorrectIdx != 3 {
		t.Fatalf("unexpected row: %+v", got)
	}
	if !got.ExpiresAt.Equal(exp) {
		t.Fatalf("expires_at mismatch: got %v, want %v", got.ExpiresAt, exp)
	}

	if err := db.DeletePending(ctx, 1, 2); err != nil {
		t.Fatal(err)
	}
	loaded, _ = db.LoadAllPending(ctx)
	if len(loaded) != 0 {
		t.Fatalf("after delete: got %d rows, want 0", len(loaded))
	}
}

func TestAttempts(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	n, err := db.IncrementAttempt(ctx, 1, 10, time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("first: n=%d err=%v", n, err)
	}
	n, _ = db.IncrementAttempt(ctx, 1, 10, time.Hour)
	if n != 2 {
		t.Fatalf("second: n=%d want 2", n)
	}
	_ = db.ResetAttempts(ctx, 1, 10)
	n, _ = db.IncrementAttempt(ctx, 1, 10, time.Hour)
	if n != 1 {
		t.Fatalf("after reset: n=%d want 1", n)
	}

	// TTL reset: directly poke the table backwards in time, then increment
	// again — count should reset to 1.
	n, _ = db.IncrementAttempt(ctx, 2, 20, time.Hour)
	if n != 1 {
		t.Fatalf("fresh: n=%d want 1", n)
	}
	pastUnix := time.Now().Add(-2 * time.Hour).Unix()
	if _, err := db.sql.ExecContext(ctx,
		`UPDATE attempts SET updated_at = ? WHERE chat_id = 2 AND user_id = 20`,
		pastUnix); err != nil {
		t.Fatal(err)
	}
	n, _ = db.IncrementAttempt(ctx, 2, 20, time.Hour)
	if n != 1 {
		t.Fatalf("ttl-reset: n=%d want 1", n)
	}

	// SweepAttempts also clears records older than ttl.
	if _, err := db.sql.ExecContext(ctx,
		`UPDATE attempts SET updated_at = ? WHERE chat_id = 2 AND user_id = 20`,
		pastUnix); err != nil {
		t.Fatal(err)
	}
	if err := db.SweepAttempts(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	n, _ = db.IncrementAttempt(ctx, 2, 20, time.Hour)
	if n != 1 {
		t.Fatalf("after sweep: n=%d want 1", n)
	}
}

func TestStats(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	now := time.Now()
	_ = db.RecordEvent(ctx, 100, 1, EventJoin, now)
	_ = db.RecordEvent(ctx, 100, 1, EventPass, now)
	_ = db.RecordEvent(ctx, 100, 2, EventJoin, now)
	_ = db.RecordEvent(ctx, 100, 2, EventKick, now)
	_ = db.RecordEvent(ctx, 100, 3, EventJoin, now)
	_ = db.RecordEvent(ctx, 100, 3, EventBan, now)

	_ = db.UpsertMember(ctx, 100, 1, now.Add(-1*time.Hour))
	_ = db.IncMessage(ctx, 100, now, true)
	_ = db.IncMessage(ctx, 100, now, true)
	_ = db.IncMessage(ctx, 100, now, false)

	s, err := db.QueryStats(ctx, 100, now.Add(-24*time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if s.Joined != 3 {
		t.Errorf("Joined=%d want 3", s.Joined)
	}
	if s.Passed != 1 {
		t.Errorf("Passed=%d want 1", s.Passed)
	}
	if s.Kicked != 1 {
		t.Errorf("Kicked=%d want 1", s.Kicked)
	}
	if s.Banned != 1 {
		t.Errorf("Banned=%d want 1", s.Banned)
	}
	if s.MsgNewcomer != 2 {
		t.Errorf("MsgNewcomer=%d want 2", s.MsgNewcomer)
	}
	if s.MsgOldtimer != 1 {
		t.Errorf("MsgOldtimer=%d want 1", s.MsgOldtimer)
	}

	// Different chat — isolation
	s2, _ := db.QueryStats(ctx, 999, now.Add(-24*time.Hour), now.Add(time.Hour))
	if s2.Joined != 0 || s2.MsgNewcomer != 0 {
		t.Errorf("chat isolation broken: %+v", s2)
	}
}

func TestMemberJoinedAt(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	if _, ok, _ := db.MemberJoinedAt(ctx, 1, 2); ok {
		t.Fatal("unexpected: new db returned member")
	}
	ts := time.Now().Truncate(time.Second)
	_ = db.UpsertMember(ctx, 1, 2, ts)
	got, ok, err := db.MemberJoinedAt(ctx, 1, 2)
	if err != nil || !ok {
		t.Fatalf("get after upsert: ok=%v err=%v", ok, err)
	}
	if !got.Equal(ts) {
		t.Fatalf("ts mismatch: got %v want %v", got, ts)
	}
}
