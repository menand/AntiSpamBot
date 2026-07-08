package storage

import (
	"context"
	"testing"
	"time"
)

func TestRecordMessageFirstEver(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	now := time.Now().Truncate(time.Second)
	rec, err := db.RecordMessage(ctx, 1, 100, now)
	if err != nil {
		t.Fatal(err)
	}
	if rec.HasBaseline {
		t.Fatalf("expected no baseline for first-ever sighting, got %+v", rec)
	}
}

func TestRecordMessageFirstAfterJoin(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	joined := time.Now().Add(-45 * 24 * time.Hour)
	_ = db.UpsertMember(ctx, 1, 100, joined)

	now := time.Now()
	rec, err := db.RecordMessage(ctx, 1, 100, now)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.HasBaseline || !rec.WasFirstMessage {
		t.Fatalf("expected baseline + first message, got %+v", rec)
	}
	if rec.Silence < 44*24*time.Hour {
		t.Fatalf("silence too small: %v", rec.Silence)
	}
}

func TestRecordMessageReturnAfterSilence(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	long := time.Now().Add(-200 * 24 * time.Hour)
	if _, err := db.RecordMessage(ctx, 1, 100, long); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	rec, err := db.RecordMessage(ctx, 1, 100, now)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.HasBaseline || rec.WasFirstMessage {
		t.Fatalf("expected baseline + non-first, got %+v", rec)
	}
	if rec.Silence < 199*24*time.Hour {
		t.Fatalf("silence too small: %v", rec.Silence)
	}
}

func TestTopWritersAndFailers(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	now := time.Now()
	for i, n := range []int{15, 10, 7, 3, 1, 1} {
		uid := int64(100 + i)
		for j := 0; j < n; j++ {
			_, _ = db.RecordMessage(ctx, 1, uid, now)
		}
	}

	top, err := db.TopWriters(ctx, 1, now.Add(-time.Hour), now.AddDate(0, 0, 1), 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 3 {
		t.Fatalf("got %d, want 3", len(top))
	}
	if top[0].UserID != 100 || top[0].Count != 15 {
		t.Errorf("#1: %+v", top[0])
	}
	if top[1].UserID != 101 || top[1].Count != 10 {
		t.Errorf("#2: %+v", top[1])
	}
	if top[2].UserID != 102 || top[2].Count != 7 {
		t.Errorf("#3: %+v", top[2])
	}

	// Failers
	_ = db.RecordEvent(ctx, 1, 500, EventKick, now)
	_ = db.RecordEvent(ctx, 1, 500, EventBan, now)
	_ = db.RecordEvent(ctx, 1, 501, EventKick, now)
	_ = db.RecordEvent(ctx, 1, 502, EventPass, now) // not a fail

	fails, err := db.TopFailers(ctx, 1, now.Add(-time.Hour), now.Add(time.Hour), 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(fails) != 2 {
		t.Fatalf("got %d, want 2", len(fails))
	}
	if fails[0].UserID != 500 || fails[0].Count != 2 {
		t.Errorf("fails #1: %+v", fails[0])
	}
	if fails[1].UserID != 501 || fails[1].Count != 1 {
		t.Errorf("fails #2: %+v", fails[1])
	}
}

func TestGetUserInfos(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	_ = db.RememberUser(ctx, UserInfo{UserID: 1, FirstName: "Vasya", Username: "vasya"})
	_ = db.RememberUser(ctx, UserInfo{UserID: 2, FirstName: "", LastName: "", Username: ""})

	got, err := db.GetUserInfos(ctx, []int64{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
	if got[1].FirstName != "Vasya" || got[1].Username != "vasya" {
		t.Errorf("user 1: %+v", got[1])
	}
	if _, ok := got[3]; ok {
		t.Error("user 3 should be absent")
	}

	// Empty list
	empty, err := db.GetUserInfos(ctx, nil)
	if err != nil || len(empty) != 0 {
		t.Errorf("empty: %v %v", empty, err)
	}
}

func TestEventUsers(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	base := time.Now().Add(-time.Hour)
	// Хронология: 300 прошёл первым, 100 — вторым (дважды), 200 забанен.
	_ = db.RecordEvent(ctx, 1, 300, EventPass, base)
	_ = db.RecordEvent(ctx, 1, 100, EventPass, base.Add(time.Minute))
	_ = db.RecordEvent(ctx, 1, 100, EventPass, base.Add(2*time.Minute))
	_ = db.RecordEvent(ctx, 1, 200, EventBan, base.Add(3*time.Minute))
	_ = db.RecordEvent(ctx, 2, 999, EventPass, base) // другой чат — не должен попасть

	passed, err := db.EventUsers(ctx, 1, EventPass, base.Add(-time.Minute), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(passed) != 2 || passed[0].UserID != 300 || passed[1].UserID != 100 {
		t.Fatalf("expected [300 100] in join order, got %+v", passed)
	}
	if passed[1].Count != 2 {
		t.Fatalf("user 100 passed twice, got count %d", passed[1].Count)
	}

	banned, err := db.EventUsers(ctx, 1, EventBan, base.Add(-time.Minute), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(banned) != 1 || banned[0].UserID != 200 {
		t.Fatalf("expected [200], got %+v", banned)
	}

	// Верхняя граница экслюзивна и по диапазону ничего лишнего.
	none, err := db.EventUsers(ctx, 1, EventPass, base.Add(-2*time.Hour), base)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("expected empty result before range, got %+v", none)
	}
}
