package storage

import (
	"context"
	"testing"
	"time"
)

// TestQuarantineRoundTrip — строка карантина переживает put/read, warned
// персистится; повторный put (пере-арм на новый срок) обновляет срок и
// перезаводит флаг пояснения (новый период карантина = новое разовое
// предупреждение уместно снова).
func TestQuarantineRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	now := time.Now()

	if err := db.PutQuarantine(ctx, 1, 2, now.Add(24*time.Hour), false); err != nil {
		t.Fatal(err)
	}
	if err := db.SetQuarantineWarned(ctx, 1, 2); err != nil {
		t.Fatal(err)
	}

	rows, err := db.AllQuarantines(ctx, 1, now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].ChatID != 1 || rows[0].UserID != 2 || !rows[0].Warned {
		t.Fatalf("unexpected row: %+v", rows[0])
	}
	if rows[0].UntilAt.Sub(now.Add(24*time.Hour)) > time.Minute {
		t.Fatalf("until_at mismatch: %v", rows[0].UntilAt)
	}

	// Повторный арм на ту же пару юзера — upsert.
	if err := db.PutQuarantine(ctx, 1, 2, now.Add(48*time.Hour), false); err != nil {
		t.Fatal(err)
	}
	rows, _ = db.AllQuarantines(ctx, 1, now.Unix())
	if len(rows) != 1 || rows[0].Warned {
		t.Fatalf("re-arm must keep one row (upsert): %+v", rows)
	}
}

func TestQuarantineScopesAndPrune(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	now := time.Now()

	// Чат 1: активная и истёкшая строки; чат 2: активная.
	_ = db.PutQuarantine(ctx, 1, 10, now.Add(time.Hour), false)
	_ = db.PutQuarantine(ctx, 1, 11, now.Add(-time.Hour), false)
	_ = db.PutQuarantine(ctx, 2, 20, now.Add(time.Hour), true)

	// AllQuarantines фильтрует истёкшие и чужие чаты.
	rows, err := db.AllQuarantines(ctx, 1, now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].UserID != 10 {
		t.Fatalf("active rows of chat 1: %+v", rows)
	}
	for _, r := range rows {
		if !r.UntilAt.After(now) {
			t.Fatalf("expired row leaked: %+v", r)
		}
	}

	// HasQuarantine — только активная, только в своём чате.
	ok, err := db.HasQuarantine(ctx, 1, 10, now.Unix())
	if err != nil || !ok {
		t.Fatalf("active quarantine: ok=%v err=%v", ok, err)
	}
	for _, q := range []struct {
		chat, user int64
	}{
		{1, 11}, // истёкшая
		{1, 20}, // чужой чат
		{2, 10}, // чужой чат (другая пара)
	} {
		ok, _ = db.HasQuarantine(ctx, q.chat, q.user, now.Unix())
		if ok {
			t.Fatalf("quarantine %d/%d must not be active", q.chat, q.user)
		}
	}

	// RestoreQuarantines сидит всех активных по всем чатам (для рестарт-сида).
	sane, err := db.RestoreQuarantines(ctx, now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	active := 0
	for _, r := range sane {
		if r.UntilAt.After(now) {
			active++
		}
	}
	if active != 2 {
		t.Fatalf("restore returned %d active rows, want 2", active)
	}

	// DeleteChatQuarantine чистит только свой чат.
	if err := db.DeleteChatQuarantine(ctx, 1); err != nil {
		t.Fatal(err)
	}
	rows, _ = db.AllQuarantines(ctx, 2, now.Unix())
	if len(rows) != 1 || rows[0].UserID != 20 {
		t.Fatalf("chat 2 rows after chat-1 delete: %+v", rows)
	}

	// PruneQuarantines убирает истёкшие, активные целы.
	_ = db.PutQuarantine(ctx, 3, 30, now.Add(-time.Minute), false)
	_ = db.PutQuarantine(ctx, 3, 31, now.Add(time.Hour), false)
	if err := db.PruneQuarantines(ctx, now.Unix()); err != nil {
		t.Fatal(err)
	}
	rows, _ = db.AllQuarantines(ctx, 3, now.Unix())
	if len(rows) != 1 || rows[0].UserID != 31 {
		t.Fatalf("after prune: %+v", rows)
	}
}
