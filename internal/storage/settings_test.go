package storage

import (
	"context"
	"testing"
)

func TestChatSettingsDefaults(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	s, err := db.GetChatSettings(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !s.GreetingEnabled {
		t.Error("default greeting should be ON")
	}
	if s.MaxAttempts.Valid {
		t.Error("default max_attempts should be NULL")
	}
	if s.CaptchaTimeoutSeconds.Valid {
		t.Error("default captcha_timeout_seconds should be NULL")
	}
	if s.DailyStatsEnabled {
		t.Error("default daily_stats_enabled should be OFF")
	}
}

func TestSetMaxAttempts(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	five := 5
	if err := db.SetMaxAttempts(ctx, 1, &five); err != nil {
		t.Fatal(err)
	}
	s, _ := db.GetChatSettings(ctx, 1)
	if !s.MaxAttempts.Valid || s.MaxAttempts.Int64 != 5 {
		t.Errorf("got %+v, want 5", s.MaxAttempts)
	}

	// Сброс через nil → обратно в NULL.
	if err := db.SetMaxAttempts(ctx, 1, nil); err != nil {
		t.Fatal(err)
	}
	s, _ = db.GetChatSettings(ctx, 1)
	if s.MaxAttempts.Valid {
		t.Errorf("expected NULL after clear, got %+v", s.MaxAttempts)
	}
}

func TestSetCaptchaMode(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	s, _ := db.GetChatSettings(ctx, 1)
	if s.CaptchaMode.Valid {
		t.Error("default captcha_mode should be NULL")
	}

	v := "emoji"
	if err := db.SetCaptchaMode(ctx, 1, &v); err != nil {
		t.Fatal(err)
	}
	s, _ = db.GetChatSettings(ctx, 1)
	if !s.CaptchaMode.Valid || s.CaptchaMode.String != "emoji" {
		t.Errorf("got %+v, want emoji", s.CaptchaMode)
	}

	// Сброс.
	if err := db.SetCaptchaMode(ctx, 1, nil); err != nil {
		t.Fatal(err)
	}
	s, _ = db.GetChatSettings(ctx, 1)
	if s.CaptchaMode.Valid {
		t.Errorf("expected NULL after clear, got %+v", s.CaptchaMode)
	}
}

func TestSetCaptchaTimeoutSec(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	sec := 60
	if err := db.SetCaptchaTimeoutSec(ctx, 1, &sec); err != nil {
		t.Fatal(err)
	}
	s, _ := db.GetChatSettings(ctx, 1)
	if !s.CaptchaTimeoutSeconds.Valid || s.CaptchaTimeoutSeconds.Int64 != 60 {
		t.Errorf("got %+v, want 60", s.CaptchaTimeoutSeconds)
	}
}

func TestSettingsAreIndependent(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	// Установка одного поля не должна затирать остальные.
	_ = db.SetGreetingEnabled(ctx, 1, false)
	five := 5
	_ = db.SetMaxAttempts(ctx, 1, &five)
	_ = db.SetDailyStatsEnabled(ctx, 1, true)

	s, _ := db.GetChatSettings(ctx, 1)
	if s.GreetingEnabled {
		t.Error("greeting wiped by MaxAttempts/Daily upserts")
	}
	if !s.MaxAttempts.Valid || s.MaxAttempts.Int64 != 5 {
		t.Error("MaxAttempts wiped")
	}
	if !s.DailyStatsEnabled {
		t.Error("DailyStats wiped")
	}
}

func TestSetGreetingText(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	s, _ := db.GetChatSettings(ctx, 1)
	if s.GreetingText.Valid {
		t.Error("default greeting_text should be NULL")
	}

	text := "Привет, {name}! Прочти правила."
	if err := db.SetGreetingText(ctx, 1, &text, nil); err != nil {
		t.Fatal(err)
	}
	s, _ = db.GetChatSettings(ctx, 1)
	if !s.GreetingText.Valid || s.GreetingText.String != text {
		t.Errorf("got %+v, want %q", s.GreetingText, text)
	}

	// Сброс через nil → обратно в NULL (встроенный дефолт).
	if err := db.SetGreetingText(ctx, 1, nil, nil); err != nil {
		t.Fatal(err)
	}
	s, _ = db.GetChatSettings(ctx, 1)
	if s.GreetingText.Valid {
		t.Errorf("expected NULL after clear, got %+v", s.GreetingText)
	}
}

func TestChatsNeedingDailyStats(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	_ = db.SetDailyStatsEnabled(ctx, 100, true)
	_ = db.SetDailyStatsEnabled(ctx, 200, true)
	_ = db.SetDailyStatsEnabled(ctx, 300, false) // не участвует
	_ = db.MarkDailyStatsSent(ctx, 100, "2026-04-22")

	// Час ХРАНИТСЯ в UTC, гейт сравнивается в МСК: (utc+3)%24. Дефолт 6 UTC =
	// 9 МСК. Спрашиваем в 12 МСК — оба включённых чата проходят проверку
	// часа, но 100 уже получил сегодняшний дайджест.
	ids, err := db.ChatsNeedingDailyStats(ctx, 12, 6, "2026-04-22")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != 200 {
		t.Errorf("got %v, want [200]", ids)
	}

	// В 7 МСК (до дефолтных 9 МСК) ни один чат ещё не готов.
	ids, _ = db.ChatsNeedingDailyStats(ctx, 7, 6, "2026-04-22")
	if len(ids) != 0 {
		t.Errorf("got %v, want 0 chats before default hour", ids)
	}

	// Пер-чатовый override: чат 200 хочет 21 UTC = 0 МСК. В 0 МСК готов —
	// граница «полночь МСК» работает без сползания.
	v := 21
	_ = db.SetDailyStatsHour(ctx, 200, &v)
	ids, _ = db.ChatsNeedingDailyStats(ctx, 0, 6, "2026-04-23")
	if len(ids) != 1 || ids[0] != 200 {
		t.Errorf("at 0 MSK chat 200 (21 UTC override) should be ready, got %v", ids)
	}

	// Override 10 UTC = 13 МСК: в 12 МСК ещё не готов, в 13 МСК — готов.
	v = 10
	_ = db.SetDailyStatsHour(ctx, 200, &v)
	ids, _ = db.ChatsNeedingDailyStats(ctx, 12, 6, "2026-04-22")
	if len(ids) != 0 {
		t.Errorf("chat 200 should wait until 13 MSK (10 UTC), got %v", ids)
	}
	ids, _ = db.ChatsNeedingDailyStats(ctx, 13, 6, "2026-04-22")
	if len(ids) != 1 || ids[0] != 200 {
		t.Errorf("at 13 MSK chat 200 should be ready, got %v", ids)
	}

	// Следующий день в 23 МСК: оба чата снова в очереди (100 по дефолтным
	// 9 МСК, 200 по override 13 МСК, за 23 апреля ещё никому не отправляли).
	ids, _ = db.ChatsNeedingDailyStats(ctx, 23, 6, "2026-04-23")
	if len(ids) != 2 {
		t.Errorf("new day at 23 MSK: got %v, want 2 chats", ids)
	}
}

func TestSetSpamSettings(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	s, err := db.GetChatSettings(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if s.SpamCheckEnabled || s.SpamThreshold.Valid || s.SpamWhitelistMsgs.Valid || s.SpamVoteMargin.Valid {
		t.Fatalf("defaults must be off/NULL/NULL/NULL, got %+v", s)
	}

	wl, vm := 20, 5
	if err := db.SetSpamCheckEnabled(ctx, 1, true); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSpamWhitelistMsgs(ctx, 1, &wl); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSpamVoteMargin(ctx, 1, &vm); err != nil {
		t.Fatal(err)
	}

	s, _ = db.GetChatSettings(ctx, 1)
	if !s.SpamCheckEnabled ||
		s.SpamWhitelistMsgs.Int64 != 20 || s.SpamVoteMargin.Int64 != 5 {
		t.Fatalf("round-trip mismatch: %+v", s)
	}

	// nil сбрасывает override на NULL.
	if err := db.SetSpamWhitelistMsgs(ctx, 1, nil); err != nil {
		t.Fatal(err)
	}
	s, _ = db.GetChatSettings(ctx, 1)
	if s.SpamWhitelistMsgs.Valid {
		t.Fatalf("nil must clear the whitelist override: %+v", s.SpamWhitelistMsgs)
	}
}
