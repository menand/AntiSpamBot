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

func TestPendingThreadIDAndDeleteChat(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	exp := time.Now().Add(30 * time.Second).Truncate(time.Second)
	_ = db.PutPending(ctx, PendingRow{ChatID: 1, UserID: 2, MessageID: 10, CorrectIdx: 0, ExpiresAt: exp, ThreadID: 77})
	_ = db.PutPending(ctx, PendingRow{ChatID: 1, UserID: 3, MessageID: 11, CorrectIdx: 1, ExpiresAt: exp})
	_ = db.PutPending(ctx, PendingRow{ChatID: 2, UserID: 4, MessageID: 12, CorrectIdx: 2, ExpiresAt: exp})

	loaded, err := db.LoadAllPending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byUser := map[int64]PendingRow{}
	for _, p := range loaded {
		byUser[p.UserID] = p
	}
	if byUser[2].ThreadID != 77 {
		t.Errorf("thread_id lost in roundtrip: %+v", byUser[2])
	}
	if byUser[3].ThreadID != 0 {
		t.Errorf("default thread_id should be 0: %+v", byUser[3])
	}

	// DeletePendingChat вычищает только чат 1.
	if err := db.DeletePendingChat(ctx, 1); err != nil {
		t.Fatal(err)
	}
	loaded, _ = db.LoadAllPending(ctx)
	if len(loaded) != 1 || loaded[0].ChatID != 2 {
		t.Fatalf("after DeletePendingChat: %+v", loaded)
	}
}

func TestGetChat(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	if _, ok, _ := db.GetChat(ctx, 5); ok {
		t.Fatal("unknown chat should not be found")
	}
	_ = db.RememberChat(ctx, ChatInfo{ChatID: 5, Title: "Тестовый чат", Type: "supergroup", Username: "testchat"})
	c, ok, err := db.GetChat(ctx, 5)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if c.Title != "Тестовый чат" || c.Type != "supergroup" || c.Username != "testchat" {
		t.Errorf("unexpected chat: %+v", c)
	}
}

func TestQueryStatsExcludesUntilDay(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	// Сообщения в два соседних дня; запрос [day1, day2) должен посчитать
	// только day1 — на этом держится окно «вчера» у дайджеста.
	day1 := time.Date(2026, 6, 10, 15, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 6, 11, 1, 0, 0, 0, time.UTC)
	_ = db.IncMessage(ctx, 1, day1, false)
	_ = db.IncMessage(ctx, 1, day2, false)

	from := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
	s, err := db.QueryStats(ctx, 1, from, until)
	if err != nil {
		t.Fatal(err)
	}
	if s.MsgOldtimer != 1 {
		t.Errorf("got %d messages, want 1 (day2 must be excluded)", s.MsgOldtimer)
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

	// Сброс по TTL: руками отматываем запись в таблице назад во времени,
	// затем инкрементируем снова — счётчик должен сброситься в 1.
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

	// SweepAttempts тоже вычищает записи старше ttl.
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

func TestAttemptCount(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	// Нет записи — 0 без ошибки.
	if n, err := db.AttemptCount(ctx, 1, 10, time.Hour); err != nil || n != 0 {
		t.Fatalf("empty: n=%d err=%v, want 0", n, err)
	}
	// Чтение не инкрементит.
	_, _ = db.IncrementAttempt(ctx, 1, 10, time.Hour)
	_, _ = db.IncrementAttempt(ctx, 1, 10, time.Hour)
	if n, _ := db.AttemptCount(ctx, 1, 10, time.Hour); n != 2 {
		t.Fatalf("fresh: n=%d want 2", n)
	}
	if n, _ := db.AttemptCount(ctx, 1, 10, time.Hour); n != 2 {
		t.Fatalf("read must not increment: n=%d want 2", n)
	}
	// Протухшая запись читается как 0.
	if _, err := db.sql.ExecContext(ctx,
		`UPDATE attempts SET updated_at = ? WHERE chat_id = 1 AND user_id = 10`,
		time.Now().Add(-2*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.AttemptCount(ctx, 1, 10, time.Hour); n != 0 {
		t.Fatalf("stale: n=%d want 0", n)
	}
}

func TestDayOf(t *testing.T) {
	// День режется по МСК (UTC+3): поздний вечер UTC — уже следующие
	// московские сутки, полночь МСК — их начало.
	tests := []struct {
		in   time.Time
		want string
	}{
		{time.Date(2026, 7, 11, 23, 30, 0, 0, time.UTC), "2026-07-12"},
		{time.Date(2026, 7, 11, 20, 59, 0, 0, time.UTC), "2026-07-11"},
		{time.Date(2026, 7, 11, 21, 0, 0, 0, time.UTC), "2026-07-12"},
		{time.Date(2026, 7, 12, 0, 0, 0, 0, StatsLocation), "2026-07-12"},
	}
	for _, tc := range tests {
		if got := DayOf(tc.in); got != tc.want {
			t.Errorf("DayOf(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestStats(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	now := time.Now()
	_ = db.RecordEvent(ctx, 100, 1, EventJoin, now, "")
	_ = db.RecordEvent(ctx, 100, 1, EventPass, now, "")
	_ = db.RecordEvent(ctx, 100, 2, EventJoin, now, "")
	_ = db.RecordEvent(ctx, 100, 2, EventKick, now, "")
	_ = db.RecordEvent(ctx, 100, 3, EventJoin, now, "")
	_ = db.RecordEvent(ctx, 100, 3, EventBan, now, "")
	_ = db.RecordEvent(ctx, 100, 4, EventJoin, now, "")
	_ = db.RecordEvent(ctx, 100, 4, EventLeft, now, "")

	_ = db.UpsertMember(ctx, 100, 1, now.Add(-1*time.Hour))
	_ = db.IncMessage(ctx, 100, now, true)
	_ = db.IncMessage(ctx, 100, now, true)
	_ = db.IncMessage(ctx, 100, now, false)

	s, err := db.QueryStats(ctx, 100, now.Add(-24*time.Hour), now.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if s.Joined != 4 {
		t.Errorf("Joined=%d want 4", s.Joined)
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
	if s.Left != 1 {
		t.Errorf("Left=%d want 1", s.Left)
	}
	if s.MsgNewcomer != 2 {
		t.Errorf("MsgNewcomer=%d want 2", s.MsgNewcomer)
	}
	if s.MsgOldtimer != 1 {
		t.Errorf("MsgOldtimer=%d want 1", s.MsgOldtimer)
	}

	// Другой чат — изоляция.
	s2, _ := db.QueryStats(ctx, 999, now.Add(-24*time.Hour), now.AddDate(0, 0, 1))
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
