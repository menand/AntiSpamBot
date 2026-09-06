package storage

import (
	"context"
	"testing"
	"time"
)

func TestSpamVoteLifecycle(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	v := SpamVote{ChatID: -1, BotMsgID: 100, TargetMsgID: 99, AuthorID: 42, Prob: 95, CreatedAt: time.Now()}
	if err := db.PutSpamVote(ctx, v); err != nil {
		t.Fatal(err)
	}

	got, found, err := db.GetSpamVote(ctx, -1, 100)
	if err != nil || !found {
		t.Fatalf("vote not found: %v", err)
	}
	if got.AuthorID != 42 || got.TargetMsgID != 99 || got.Prob != 95 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	pending, err := db.HasPendingVoteForAuthor(ctx, -1, 42)
	if err != nil || !pending {
		t.Fatalf("want pending vote for author, got %v %v", pending, err)
	}
	if pending, _ := db.HasPendingVoteForAuthor(ctx, -1, 43); pending {
		t.Fatal("author 43 has no vote")
	}

	// Голоса: upsert + переголосование. Все — при живом голосовании.
	if ok, err := db.UpsertBallot(ctx, -1, 100, 7, true); err != nil || !ok {
		t.Fatalf("ballot on live vote: ok=%v err=%v", ok, err)
	}
	_, _ = db.UpsertBallot(ctx, -1, 100, 8, true)
	_, _ = db.UpsertBallot(ctx, -1, 100, 9, false)
	_, _ = db.UpsertBallot(ctx, -1, 100, 9, true) // передумал
	yes, no, err := db.CountBallots(ctx, -1, 100)
	if err != nil || yes != 3 || no != 0 {
		t.Fatalf("want 3:0 after re-vote, got %d:%d err=%v", yes, no, err)
	}

	// Первый Take забирает, второй — нет (гонка вердиктов).
	taken, err := db.TakeSpamVote(ctx, -1, 100)
	if err != nil || !taken {
		t.Fatalf("first take must win: %v", err)
	}
	taken, err = db.TakeSpamVote(ctx, -1, 100)
	if err != nil || taken {
		t.Fatalf("second take must lose: taken=%v err=%v", taken, err)
	}
	// Бюллетени вычищены.
	yes, no, _ = db.CountBallots(ctx, -1, 100)
	if yes != 0 || no != 0 {
		t.Fatalf("ballots must be cleared on take, got %d:%d", yes, no)
	}
	if pending, _ := db.HasPendingVoteForAuthor(ctx, -1, 42); pending {
		t.Fatal("no pending vote after take")
	}
}

func TestExpiredSpamVotes(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	old := SpamVote{ChatID: -1, BotMsgID: 1, TargetMsgID: 0, AuthorID: 1, Prob: 90,
		CreatedAt: time.Now().Add(-25 * time.Hour)}
	fresh := SpamVote{ChatID: -1, BotMsgID: 2, TargetMsgID: 0, AuthorID: 2, Prob: 90,
		CreatedAt: time.Now()}
	_ = db.PutSpamVote(ctx, old)
	_ = db.PutSpamVote(ctx, fresh)

	expired, err := db.ExpiredSpamVotes(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0].BotMsgID != 1 {
		t.Fatalf("want only the 25h-old vote, got %+v", expired)
	}
}

func TestUserMessageTotal(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	now := time.Now()
	for i := range 3 {
		if _, err := db.RecordMessage(ctx, -1, 42, now.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = db.RecordMessage(ctx, -1, 43, now) // другой юзер
	_, _ = db.RecordMessage(ctx, -2, 42, now) // другой чат

	n, err := db.UserMessageTotal(ctx, -1, 42)
	if err != nil || n != 3 {
		t.Fatalf("want 3 messages, got %d err=%v", n, err)
	}
	n, _ = db.UserMessageTotal(ctx, -1, 999)
	if n != 0 {
		t.Fatalf("unknown user must have 0, got %d", n)
	}
}

func TestUserMessageTotalsByChat(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	now := time.Now()
	// 3 сообщения в чате -1, 2 в чате -2 — по-чатовые суммы, не общая.
	for i := range 3 {
		_, _ = db.RecordMessage(ctx, -1, 42, now.Add(time.Duration(i)*time.Minute))
	}
	for i := range 2 {
		_, _ = db.RecordMessage(ctx, -2, 42, now.Add(time.Duration(i)*time.Minute))
	}
	_, _ = db.RecordMessage(ctx, -1, 43, now) // чужие сообщения не попадают

	totals, err := db.UserMessageTotalsByChat(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(totals) != 2 || totals[-1] != 3 || totals[-2] != 2 {
		t.Fatalf("want {-1:3 -2:2}, got %v", totals)
	}
	if totals, _ := db.UserMessageTotalsByChat(ctx, 999); len(totals) != 0 {
		t.Fatalf("unknown user must have no rows, got %v", totals)
	}
}

func TestListBallots(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	v := SpamVote{ChatID: -1, BotMsgID: 100, TargetMsgID: 99, AuthorID: 42, Prob: 95, CreatedAt: time.Now()}
	_ = db.PutSpamVote(ctx, v)
	_, _ = db.UpsertBallot(ctx, -1, 100, 7, true)
	_, _ = db.UpsertBallot(ctx, -1, 100, 8, false)

	ballots, err := db.ListBallots(ctx, -1, 100)
	if err != nil || len(ballots) != 2 {
		t.Fatalf("want 2 ballots, got %v err=%v", ballots, err)
	}
	byVoter := map[int64]bool{}
	for _, bl := range ballots {
		byVoter[bl.VoterID] = bl.IsSpam
	}
	if !byVoter[7] || byVoter[8] {
		t.Fatalf("ballot values mismatch: %v", byVoter)
	}

	// После Take бюллетеней нет.
	_, _ = db.TakeSpamVote(ctx, -1, 100)
	ballots, _ = db.ListBallots(ctx, -1, 100)
	if len(ballots) != 0 {
		t.Fatalf("ballots must be gone after take, got %v", ballots)
	}
}

func TestSpamBanned(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	if banned, err := db.IsSpamBanned(ctx, 42); err != nil || banned {
		t.Fatalf("fresh user must not be banned: %v %v", banned, err)
	}
	if err := db.AddSpamBanned(ctx, 42, -1, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Повторный вердикт в другом чате — идемпотентно.
	if err := db.AddSpamBanned(ctx, 42, -2, time.Now()); err != nil {
		t.Fatal(err)
	}
	if banned, err := db.IsSpamBanned(ctx, 42); err != nil || !banned {
		t.Fatalf("user must be banned: %v %v", banned, err)
	}

	// Прощение: ручной разбан админом снимает флаг.
	removed, err := db.DeleteSpamBanned(ctx, 42)
	if err != nil || !removed {
		t.Fatalf("delete must remove existing row: %v %v", removed, err)
	}
	if banned, _ := db.IsSpamBanned(ctx, 42); banned {
		t.Fatal("user must be forgiven after delete")
	}
	if removed, _ := db.DeleteSpamBanned(ctx, 42); removed {
		t.Fatal("second delete must be a no-op")
	}
}

func TestSpamNotify(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	if on, err := db.SpamNotifyEnabled(ctx, 1); err != nil || on {
		t.Fatalf("default must be off: %v %v", on, err)
	}
	if owners, _ := db.SpamNotifyOwners(ctx); len(owners) != 0 {
		t.Fatalf("no subscribers expected, got %v", owners)
	}
	if err := db.SetSpamNotify(ctx, 1, true); err != nil {
		t.Fatal(err)
	}
	_ = db.SetSpamNotify(ctx, 2, false)
	if on, _ := db.SpamNotifyEnabled(ctx, 1); !on {
		t.Fatal("must be on after enable")
	}
	owners, err := db.SpamNotifyOwners(ctx)
	if err != nil || len(owners) != 1 || owners[0] != 1 {
		t.Fatalf("want [1], got %v err=%v", owners, err)
	}
	if err := db.SetSpamNotify(ctx, 1, false); err != nil {
		t.Fatal(err)
	}
	if on, _ := db.SpamNotifyEnabled(ctx, 1); on {
		t.Fatal("must be off after disable")
	}
}

func TestLastStatsPeriod(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	if p, err := db.LastStatsPeriod(ctx, 1); err != nil || p != "" {
		t.Fatalf("no row must give empty period: %q %v", p, err)
	}
	if err := db.SetLastStatsPeriod(ctx, 1, "day"); err != nil {
		t.Fatal(err)
	}
	if p, err := db.LastStatsPeriod(ctx, 1); err != nil || p != "day" {
		t.Fatalf("want day, got %q err=%v", p, err)
	}
	// Перезапись и независимость юзеров.
	_ = db.SetLastStatsPeriod(ctx, 1, "month")
	if p, _ := db.LastStatsPeriod(ctx, 1); p != "month" {
		t.Fatalf("want month, got %q", p)
	}
	if p, _ := db.LastStatsPeriod(ctx, 2); p != "" {
		t.Fatalf("user 2 must be untouched, got %q", p)
	}
}

func TestDailyReportSettings(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	if on, err := db.DailyReportEnabled(ctx, 1); err != nil || on {
		t.Fatalf("default must be off: %v %v", on, err)
	}
	if subs, _ := db.DailyReportSubscribers(ctx); len(subs) != 0 {
		t.Fatalf("no subscribers expected, got %v", subs)
	}
	if err := db.SetDailyReport(ctx, 1, true); err != nil {
		t.Fatal(err)
	}
	_ = db.SetDailyReport(ctx, 2, false)
	subs, err := db.DailyReportSubscribers(ctx)
	if err != nil || len(subs) != 1 || subs[0].UserID != 1 || subs[0].LastDay != "" {
		t.Fatalf("want [{1 \"\"}], got %v err=%v", subs, err)
	}
	if err := db.MarkDailyReportSent(ctx, 1, "2026-07-16"); err != nil {
		t.Fatal(err)
	}
	subs, _ = db.DailyReportSubscribers(ctx)
	if len(subs) != 1 || subs[0].LastDay != "2026-07-16" {
		t.Fatalf("mark must update LastDay, got %v", subs)
	}
	// Маркер без подписки не создаёт подписчика.
	if err := db.MarkDailyReportSent(ctx, 3, "2026-07-16"); err != nil {
		t.Fatal(err)
	}
	if subs, _ = db.DailyReportSubscribers(ctx); len(subs) != 1 {
		t.Fatalf("mark alone must not subscribe, got %v", subs)
	}
	if err := db.SetDailyReport(ctx, 1, false); err != nil {
		t.Fatal(err)
	}
	if subs, _ = db.DailyReportSubscribers(ctx); len(subs) != 0 {
		t.Fatalf("must be gone after disable, got %v", subs)
	}
}

func TestGreetingsLifecycle(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	_ = db.PutGreeting(ctx, -1, 42, 500, time.Now())
	_ = db.PutGreeting(ctx, -1, 42, 501, time.Now()) // перевход перезаписывает

	msgID, ok, err := db.TakeGreetingMsg(ctx, -1, 42)
	if err != nil || !ok || msgID != 501 {
		t.Fatalf("want latest greeting 501, got %d ok=%v err=%v", msgID, ok, err)
	}
	if _, ok, _ := db.TakeGreetingMsg(ctx, -1, 42); ok {
		t.Fatal("second take must find nothing")
	}
	if _, ok, _ := db.TakeGreetingMsg(ctx, -1, 999); ok {
		t.Fatal("unknown user must have no greeting")
	}
}

func TestPruneGreetings(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	_ = db.PutGreeting(ctx, -1, 1, 10, time.Now().Add(-49*time.Hour))
	_ = db.PutGreeting(ctx, -1, 2, 20, time.Now())

	if err := db.PruneGreetings(ctx, time.Now().Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := db.TakeGreetingMsg(ctx, -1, 1); ok {
		t.Fatal("stale greeting must be pruned")
	}
	if _, ok, _ := db.TakeGreetingMsg(ctx, -1, 2); !ok {
		t.Fatal("fresh greeting must survive prune")
	}
}

func TestVersionNotify(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	// Дефолт — ВКЛЮЧЕНО: строки нет, но юзер считается подписанным (opt-out).
	if on, err := db.VersionNotifyEnabled(ctx, 1); err != nil || !on {
		t.Fatalf("no row: on=%v err=%v, want true", on, err)
	}
	// Строка, созданная другим тумблером, тоже читается как включено (DEFAULT 1).
	_ = db.SetSpamNotify(ctx, 2, true)
	if on, _ := db.VersionNotifyEnabled(ctx, 2); !on {
		t.Fatal("row created by another toggle must default to enabled")
	}
	// Явное выключение → Enabled=false, юзер в списке отказников.
	if err := db.SetVersionNotify(ctx, 1, false); err != nil {
		t.Fatal(err)
	}
	if on, _ := db.VersionNotifyEnabled(ctx, 1); on {
		t.Fatal("opt-out must disable")
	}
	outs, err := db.VersionNotifyOptOuts(ctx)
	if err != nil || len(outs) != 1 || outs[0] != 1 {
		t.Fatalf("opt-outs = %v (err %v), want [1]", outs, err)
	}
	// Включил обратно — из отказников пропал.
	_ = db.SetVersionNotify(ctx, 1, true)
	if outs, _ := db.VersionNotifyOptOuts(ctx); len(outs) != 0 {
		t.Fatalf("after re-enable opt-outs = %v, want empty", outs)
	}
}

func TestUpsertBallotOnClosedVote(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	// Голосование не создавалось: бюллетень-сироту не пишем, ok=false.
	ok, err := db.UpsertBallot(ctx, -5, 77, 9, true)
	if err != nil {
		t.Fatalf("upsert on missing vote: %v", err)
	}
	if ok {
		t.Fatal("ballot on closed vote must be rejected")
	}
	yes, no, err := db.CountBallots(ctx, -5, 77)
	if err != nil || yes != 0 || no != 0 {
		t.Fatalf("no orphan ballots expected, got %d:%d err=%v", yes, no, err)
	}

	// Живое голосование принимает бюллетень, закрытое — больше нет.
	if err := db.PutSpamVote(ctx, SpamVote{ChatID: -5, BotMsgID: 78, AuthorID: 42, Prob: 100, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.UpsertBallot(ctx, -5, 78, 9, true); err != nil || !ok {
		t.Fatalf("ballot on live vote: ok=%v err=%v", ok, err)
	}
	taken, err := db.TakeSpamVote(ctx, -5, 78)
	if err != nil || !taken {
		t.Fatalf("take: %v %v", taken, err)
	}
	if ok, _ := db.UpsertBallot(ctx, -5, 78, 9, false); ok {
		t.Fatal("ballot after take must be rejected")
	}
}

func TestYoungSpamVotes(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	now := time.Now()
	_ = db.PutSpamVote(ctx, SpamVote{ChatID: -1, BotMsgID: 1, AuthorID: 42, Prob: 100, CreatedAt: now})
	_ = db.PutSpamVote(ctx, SpamVote{ChatID: -1, BotMsgID: 2, AuthorID: 43, Prob: 100, CreatedAt: now.Add(-25 * time.Hour)})

	young, err := db.YoungSpamVotes(ctx, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("young: %v", err)
	}
	if len(young) != 1 || young[0].BotMsgID != 1 {
		t.Fatalf("want only the fresh vote, got %+v", young)
	}
}

func TestPutSpamVoteOnceOnePerAuthor(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	vote := func(botMsg int, author int64) SpamVote {
		return SpamVote{ChatID: -5, BotMsgID: botMsg, TargetMsgID: 1,
			AuthorID: author, Prob: 100, CreatedAt: time.Now()}
	}

	if ok, err := db.PutSpamVoteOnce(ctx, vote(10, 42)); err != nil || !ok {
		t.Fatalf("first plashka must win: ok=%v err=%v", ok, err)
	}
	// Вторая плашка на того же автора не проходит, строка остаётся одна.
	ok, err := db.PutSpamVoteOnce(ctx, vote(11, 42))
	if err != nil || ok {
		t.Fatalf("second plashka for same author must lose: ok=%v err=%v", ok, err)
	}
	if pending, _ := db.HasPendingVoteForAuthor(ctx, -5, 42); !pending {
		t.Fatal("original vote must survive the lost race")
	}
	// Другой автор независим.
	if ok, err := db.PutSpamVoteOnce(ctx, vote(13, 43)); err != nil || !ok {
		t.Fatalf("other author must not be blocked: ok=%v err=%v", ok, err)
	}
	// После Take плашка снова возможна.
	if taken, err := db.TakeSpamVote(ctx, -5, 10); err != nil || !taken {
		t.Fatalf("take must win: %v", err)
	}
	if ok, err := db.PutSpamVoteOnce(ctx, vote(12, 42)); err != nil || !ok {
		t.Fatalf("re-arm after take must succeed: ok=%v err=%v", ok, err)
	}
}
