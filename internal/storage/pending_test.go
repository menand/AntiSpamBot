package storage

import (
	"context"
	"testing"
	"time"
)

// TestPendingReplyExtendGuard — guard-UPDATE продления ожидания ответа:
// продлевает только СВОЮ стадию (переход серии персистится заранее — строка
// N+1 не должна быть переписана гвардом старой стадии), а удалённую строку
// (ответ/выход/кара уже разобрались) НЕ воскрешает — иначе рестарт зарядил бы
// фантомную серию ответившему.
func TestPendingReplyExtendGuard(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	stage2 := time.Now().Add(time.Minute).Truncate(time.Second)
	if err := db.PutPendingReply(ctx, PendingReply{
		ChatID: 1, UserID: 2, ExpiresAt: stage2, Stage: 2,
	}); err != nil {
		t.Fatal(err)
	}

	// Чужая стадия (1): строка stage 2 не тронута.
	if err := db.ExtendPendingReplyIf(ctx, 1, 2, 1, stage2.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	rows, err := db.LoadAllPendingReplies(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %v err=%v, want 1", rows, err)
	}
	if !rows[0].ExpiresAt.Equal(stage2) {
		t.Fatalf("guard by another stage must not touch the row: %v", rows[0].ExpiresAt)
	}

	// Своя стадия — продлевает.
	extended := stage2.Add(5 * time.Minute)
	if err := db.ExtendPendingReplyIf(ctx, 1, 2, 2, extended); err != nil {
		t.Fatal(err)
	}
	rows, err = db.LoadAllPendingReplies(ctx)
	if err != nil || len(rows) != 1 || !rows[0].ExpiresAt.Equal(extended) {
		t.Fatalf("own-stage extend must land: %v err=%v", rows, err)
	}

	// Строка удалена победителем — продление не воскрешает её.
	if err := db.DeletePendingReplyIf(ctx, 1, 2, 2); err != nil {
		t.Fatal(err)
	}
	if err := db.ExtendPendingReplyIf(ctx, 1, 2, 2, extended.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if rows, err := db.LoadAllPendingReplies(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("extend must not resurrect a deleted row: %v err=%v", rows, err)
	}
}
