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

	// Провалившие капчу.
	_ = db.RecordEvent(ctx, 1, 500, EventKick, now, "")
	_ = db.RecordEvent(ctx, 1, 500, EventBan, now, "")
	_ = db.RecordEvent(ctx, 1, 501, EventKick, now, "")
	_ = db.RecordEvent(ctx, 1, 502, EventPass, now, "") // не провал

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

	// Пустой список.
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
	_ = db.RecordEvent(ctx, 1, 300, EventPass, base, "")
	_ = db.RecordEvent(ctx, 1, 100, EventPass, base.Add(time.Minute), "")
	_ = db.RecordEvent(ctx, 1, 100, EventPass, base.Add(2*time.Minute), "")
	_ = db.RecordEvent(ctx, 1, 200, EventBan, base.Add(3*time.Minute), "")
	_ = db.RecordEvent(ctx, 2, 999, EventPass, base, "") // другой чат — не должен попасть

	passed, err := db.EventUsers(ctx, 1, base.Add(-time.Minute), time.Now(), EventPass)
	if err != nil {
		t.Fatal(err)
	}
	if len(passed) != 2 || passed[0].UserID != 300 || passed[1].UserID != 100 {
		t.Fatalf("expected [300 100] in join order, got %+v", passed)
	}
	if passed[1].Count != 2 {
		t.Fatalf("user 100 passed twice, got count %d", passed[1].Count)
	}

	banned, err := db.EventUsers(ctx, 1, base.Add(-time.Minute), time.Now(), EventBan)
	if err != nil {
		t.Fatal(err)
	}
	if len(banned) != 1 || banned[0].UserID != 200 {
		t.Fatalf("expected [200], got %+v", banned)
	}

	// Верхняя граница экслюзивна и по диапазону ничего лишнего.
	none, err := db.EventUsers(ctx, 1, base.Add(-2*time.Hour), base, EventPass)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("expected empty result before range, got %+v", none)
	}
}

func TestPassedUsers(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	// 300: join → pass за 12 сек.
	_ = db.RecordEvent(ctx, 1, 300, EventJoin, base, "")
	_ = db.RecordEvent(ctx, 1, 300, EventPass, base.Add(12*time.Second), "")
	// 100: два прохождения (30 и 8 сек) — берётся лучшее.
	_ = db.RecordEvent(ctx, 1, 100, EventJoin, base.Add(time.Minute), "")
	_ = db.RecordEvent(ctx, 1, 100, EventPass, base.Add(time.Minute+30*time.Second), "")
	_ = db.RecordEvent(ctx, 1, 100, EventJoin, base.Add(2*time.Minute), "")
	_ = db.RecordEvent(ctx, 1, 100, EventPass, base.Add(2*time.Minute+8*time.Second), "")
	// 200: pass без join (старые данные) — время неизвестно.
	_ = db.RecordEvent(ctx, 1, 200, EventPass, base.Add(3*time.Minute), "")

	got, err := db.PassedUsers(ctx, 1, base.Add(-time.Minute), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 users, got %+v", got)
	}
	if got[0].UserID != 300 || got[1].UserID != 100 || got[2].UserID != 200 {
		t.Fatalf("wrong chronological order: %+v", got)
	}
	if got[0].Secs != 12 {
		t.Fatalf("user 300: want 12 sec, got %d", got[0].Secs)
	}
	if got[1].Secs != 8 || got[1].Count != 2 {
		t.Fatalf("user 100: want best 8 sec of 2 passes, got %+v", got[1])
	}
	if got[2].Secs != -1 {
		t.Fatalf("user 200: want -1 (no join recorded), got %d", got[2].Secs)
	}
}

func TestRecentEventUsers(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	base := time.Now().Add(-time.Hour)

	// Юзер 1: два бана — в списке один раз, по свежему событию.
	_ = db.RecordEvent(ctx, 1, 1, EventBan, base.Add(1*time.Minute), ReasonCaptcha)
	_ = db.RecordEvent(ctx, 1, 1, EventBan, base.Add(5*time.Minute), ReasonCaptcha)
	// Юзер 2: спам-бан (для /unban оба вида в списке).
	_ = db.RecordEvent(ctx, 1, 2, EventSpamBan, base.Add(3*time.Minute), ReasonGlobal)
	// Юзер 3: кик за капчу — под фильтр kinds=ban|spamban не попадает.
	_ = db.RecordEvent(ctx, 1, 3, EventKick, base.Add(4*time.Minute), ReasonCaptcha)
	// Чужой чат не подмешивается.
	_ = db.RecordEvent(ctx, 2, 4, EventBan, base.Add(6*time.Minute), ReasonCaptcha)

	got, err := db.RecentEventUsers(ctx, 1, 10,
		[]EventKind{EventBan, EventSpamBan}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].UserID != 1 || got[1].UserID != 2 {
		t.Fatalf("want [1 2] freshest first, got %+v", got)
	}
	if !got[0].At.Equal(base.Add(5 * time.Minute).Truncate(time.Second)) {
		t.Fatalf("user 1: want freshest event time, got %v", got[0].At)
	}

	// Фильтр по причинам: только провалы проверки (captcha|noreply).
	_ = db.RecordEvent(ctx, 1, 5, EventKick, base.Add(7*time.Minute), ReasonNoReply)
	_ = db.RecordEvent(ctx, 1, 6, EventKick, base.Add(8*time.Minute), "mod:42")
	got, err = db.RecentEventUsers(ctx, 1, 10,
		[]EventKind{EventKick, EventBan}, []string{ReasonCaptcha, ReasonNoReply})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].UserID != 5 || got[1].UserID != 1 || got[2].UserID != 3 {
		t.Fatalf("reason filter: want [5 1 3] freshest first, got %+v", got)
	}

	// Лимит режет хвост, свежие остаются.
	got, _ = db.RecentEventUsers(ctx, 1, 1, []EventKind{EventBan, EventSpamBan}, nil)
	if len(got) != 1 || got[0].UserID != 1 {
		t.Fatalf("limit: want [1], got %+v", got)
	}
}

func TestTrusted(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	if ok, err := db.IsTrusted(ctx, 1, 10); err != nil || ok {
		t.Fatalf("empty: ok=%v err=%v, want false", ok, err)
	}
	if err := db.AddTrusted(ctx, 1, 10, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Повторное добавление — no-op.
	if err := db.AddTrusted(ctx, 1, 10, time.Now()); err != nil {
		t.Fatal(err)
	}
	if ok, _ := db.IsTrusted(ctx, 1, 10); !ok {
		t.Fatal("added user must be trusted")
	}
	// Доверие пер-чатовое.
	if ok, _ := db.IsTrusted(ctx, 2, 10); ok {
		t.Fatal("trust must not leak to another chat")
	}
}
