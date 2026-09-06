package storage

import (
	"context"
	"testing"
	"time"
)

// TestUserMessageWindowCounts — окна /info совпадают с семантикой statsRange:
// календарные дни МСК, «вчера» с верхней границей сегодня, скользящие окна
// включают сегодня.
func TestUserMessageWindowCounts(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	const chat, user = int64(-77), int64(42)
	now := time.Now()
	day := func(offset int) time.Time {
		// Полночь МСК дня со сдвигом offset — как режет DayOf.
		d := now.In(StatsLocation).AddDate(0, 0, -offset)
		return time.Date(d.Year(), d.Month(), d.Day(), 12, 0, 0, 0, StatsLocation)
	}
	seed := []struct {
		offset int
		n      int
	}{
		{0, 2},  // сегодня
		{1, 1},  // вчера
		{10, 3}, // вне недельного окна, в месячном
		{40, 5}, // только «всего»
	}
	for _, s := range seed {
		for range s.n {
			if _, err := db.RecordMessage(ctx, chat, user, day(s.offset)); err != nil {
				t.Fatalf("seed +%dd: %v", s.offset, err)
			}
		}
	}

	w, err := db.UserMessageWindowCounts(ctx, chat, user,
		DayOf(day(0)), DayOf(day(1)), DayOf(day(6)), DayOf(day(29)))
	if err != nil {
		t.Fatal(err)
	}
	if w.Today != 2 || w.Yesterday != 1 || w.Week != 3 || w.Month != 6 || w.Total != 11 {
		t.Errorf("windows mismatch: %+v", w)
	}

	// Чужой юзер — нули, а не чужие счётчики.
	if w, _ := db.UserMessageWindowCounts(ctx, chat, user+1,
		DayOf(day(0)), DayOf(day(1)), DayOf(day(6)), DayOf(day(29))); w.Total != 0 {
		t.Errorf("foreign user must see zeros, got %+v", w)
	}
}

func TestUserEventCounts(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	const chat, user = int64(-78), int64(43)
	now := time.Now()
	record := func(kind EventKind, reason string) {
		if err := db.RecordEvent(ctx, chat, user, kind, now, reason); err != nil {
			t.Fatal(err)
		}
	}
	record(EventKick, ReasonCaptcha)
	record(EventKick, ReasonCaptcha)
	record(EventBan, ReasonCaptcha)
	record(EventKick, ReasonNoReply)
	record(EventBan, ReasonNoReply)
	record(EventKick, ReasonModPrefix+"5")
	record(EventBan, ReasonModPrefix+"5")
	record(EventBan, ReasonModPrefix+"5")
	record(EventSpamBan, ReasonVotePrefix+"1,7")
	record(EventMute, ReasonModPrefix+"9")
	record(EventMute, ReasonModPrefix+"9")
	record(EventSuspect, "")
	record(EventSuspect, "")
	record(EventSuspect, "")
	record(EventSuspect, "")
	record(EventJoin, "")
	record(EventPass, "")

	c, err := db.UserEventCounts(ctx, chat, user)
	if err != nil {
		t.Fatal(err)
	}
	want := EventCounts{
		CaptchaFails: 3,
		NoReply:      2,
		ModKicked:    1,
		ModBanned:    2,
		SpamBanned:   1,
		Mutes:        2,
		Suspects:     4,
	}
	if c != want {
		t.Errorf("counts mismatch:\n got %+v\nwant %+v", c, want)
	}

	// Другой юзер того же чата — пусто.
	if c, _ := db.UserEventCounts(ctx, chat, user+1); c != (EventCounts{}) {
		t.Errorf("foreign user must have empty counts, got %+v", c)
	}
}

func TestChatEarliestEventAt(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	const chat = int64(-79)
	if _, ok, err := db.ChatEarliestEventAt(ctx, chat); ok || err != nil {
		t.Fatalf("empty chat: ok=%v err=%v", ok, err)
	}

	now := time.Now()
	_ = db.RecordEvent(ctx, chat, 1, EventJoin, now.Add(-48*time.Hour), "")
	_ = db.RecordEvent(ctx, chat, 1, EventPass, now, "")
	got, ok, err := db.ChatEarliestEventAt(ctx, chat)
	if err != nil || !ok {
		t.Fatalf("want earliest event, ok=%v err=%v", ok, err)
	}
	// Допуск в секунду на округление unix.
	if diff := got.Sub(now.Add(-48 * time.Hour)); diff < -time.Second || diff > time.Second {
		t.Errorf("earliest = %v, want %v", got, now.Add(-48*time.Hour))
	}
}

func TestChatBotAddedAt(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	const chat = int64(-80)
	if _, ok, err := db.GetChatBotAddedAt(ctx, chat); ok || err != nil {
		t.Fatalf("unknown chat: ok=%v err=%v", ok, err)
	}

	// Чат без даты (легаси-строка): ok=false, ошибки нет.
	if err := db.RememberChat(ctx, ChatInfo{ChatID: chat, Title: "L", Type: "group"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := db.GetChatBotAddedAt(ctx, chat); ok || err != nil {
		t.Fatalf("legacy chat: ok=%v err=%v", ok, err)
	}

	first := time.Now().Add(-72 * time.Hour).Truncate(time.Second) // колонка хранит unix-секунды
	if err := db.SetChatBotAddedAtIfEmpty(ctx, chat, first); err != nil {
		t.Fatal(err)
	}
	// Write-once: повторный вызов не сдвигает дату.
	if err := db.SetChatBotAddedAtIfEmpty(ctx, chat, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, ok, err := db.GetChatBotAddedAt(ctx, chat)
	if err != nil || !ok {
		t.Fatalf("want added at, ok=%v err=%v", ok, err)
	}
	if !got.Equal(first) {
		t.Errorf("added at drifted: got %v want %v", got, first)
	}

	// Нет строки реестра — UPDATE молча ничего не пишет (задокументированное
	// поведение: дата появится от следующего my_chat_member).
	const ghost = int64(-81)
	if err := db.SetChatBotAddedAtIfEmpty(ctx, ghost, first); err != nil {
		t.Fatalf("ghost chat write must be silent no-op, got %v", err)
	}
	if _, ok, _ := db.GetChatBotAddedAt(ctx, ghost); ok {
		t.Error("ghost chat must stay unknown")
	}
}

func TestSpamVoteInitiatorRoundtrip(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	now := time.Now()
	v := SpamVote{ChatID: -1, BotMsgID: 200, TargetMsgID: 199, AuthorID: 42,
		InitiatorID: 7, Prob: 100, CreatedAt: now}
	if err := db.PutSpamVote(ctx, v); err != nil {
		t.Fatal(err)
	}
	got, found, err := db.GetSpamVote(ctx, -1, 200)
	if err != nil || !found {
		t.Fatalf("vote not found: %v", err)
	}
	if got.InitiatorID != 7 {
		t.Errorf("initiator lost on get: %+v", got)
	}
	young, err := db.YoungSpamVotes(ctx, now.Add(-time.Minute))
	if err != nil || len(young) != 1 || young[0].InitiatorID != 7 {
		t.Errorf("initiator lost on young scan: %+v err=%v", young, err)
	}
	old, err := db.ExpiredSpamVotes(ctx, now.Add(time.Minute))
	if err != nil || len(old) != 1 || old[0].InitiatorID != 7 {
		t.Errorf("initiator lost on expired scan: %+v err=%v", old, err)
	}
	// Плашка ИИ: инициатор по умолчанию 0.
	ai := SpamVote{ChatID: -1, BotMsgID: 201, TargetMsgID: 198, AuthorID: 43,
		Prob: 100, CreatedAt: now}
	_ = db.PutSpamVote(ctx, ai)
	got, _, _ = db.GetSpamVote(ctx, -1, 201)
	if got.InitiatorID != 0 {
		t.Errorf("AI plashka must have zero initiator: %+v", got)
	}
}
