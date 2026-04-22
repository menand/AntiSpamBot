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

	// Clear via nil → back to NULL.
	if err := db.SetMaxAttempts(ctx, 1, nil); err != nil {
		t.Fatal(err)
	}
	s, _ = db.GetChatSettings(ctx, 1)
	if s.MaxAttempts.Valid {
		t.Errorf("expected NULL after clear, got %+v", s.MaxAttempts)
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

	// Setting one field should not wipe others.
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

func TestChatsNeedingDailyStats(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	_ = db.SetDailyStatsEnabled(ctx, 100, true)
	_ = db.SetDailyStatsEnabled(ctx, 200, true)
	_ = db.SetDailyStatsEnabled(ctx, 300, false) // not eligible
	_ = db.MarkDailyStatsSent(ctx, 100, "2026-04-22")

	ids, err := db.ChatsNeedingDailyStats(ctx, "2026-04-22")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != 200 {
		t.Errorf("got %v, want [200]", ids)
	}

	// Different day: 100 becomes eligible again.
	ids, _ = db.ChatsNeedingDailyStats(ctx, "2026-04-23")
	if len(ids) != 2 {
		t.Errorf("got %v, want 2 chats", ids)
	}
}
