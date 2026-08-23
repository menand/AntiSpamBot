package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestMigrateChat_FreshNewSide(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	old := int64(-5000)
	neu := int64(-100001)
	now := time.Now()

	_ = db.RememberChat(ctx, ChatInfo{ChatID: old, Title: "Old", Type: "group"})
	_ = db.UpsertMember(ctx, old, 1, now.Add(-48*time.Hour))
	_ = db.RecordEvent(ctx, old, 1, EventJoin, now, "")
	_ = db.RecordEvent(ctx, old, 1, EventPass, now, "")
	_, _ = db.RecordMessage(ctx, old, 1, now)
	_ = db.IncMessage(ctx, old, now, true)
	_ = db.SetGreetingEnabled(ctx, old, false)
	maxAtt, hour, mode, greet := 5, 21, "emoji", "Привет, {name}!"
	_ = db.SetMaxAttempts(ctx, old, &maxAtt)
	intervalMin := 3
	_ = db.SetCaptchaIntervalMin(ctx, old, &intervalMin)
	// Легаси-колонка таймаута в секундах: сеттера больше нет, сеем сырым
	// UPDATE — миграция данных при открытии обязана была сконвертировать её
	// в минуты интервала (см. TestLegacyCaptchaTimeoutConverted).
	_, _ = db.sql.ExecContext(ctx, `UPDATE chat_settings SET captcha_timeout_seconds = 90 WHERE chat_id = ?`, old)
	_ = db.SetDailyStatsEnabled(ctx, old, true)
	_ = db.SetDailyStatsHour(ctx, old, &hour)
	_ = db.SetCaptchaMode(ctx, old, &mode)
	greetEnts := `[{"type":"bold","offset":0,"length":6}]`
	_ = db.SetGreetingText(ctx, old, &greet, &greetEnts)
	_ = db.SetSilentAnnounceEnabled(ctx, old, false)
	swl, svm := 10, 2
	_ = db.SetSpamCheckEnabled(ctx, old, true)
	// Легаси-колонка (сеттер выпилен вместе с порогом), но MigrateChat обязан
	// переносить и её — сеем сырым UPDATE.
	_, _ = db.sql.ExecContext(ctx, `UPDATE chat_settings SET spam_threshold = 75 WHERE chat_id = ?`, old)
	_ = db.SetSpamWhitelistMsgs(ctx, old, &swl)
	_ = db.SetSpamVoteMargin(ctx, old, &svm)
	rpls := 90
	_ = db.SetReplyCheckEnabled(ctx, old, true)
	// reply_check_seconds — легаси (серия напоминаний живёт на общем
	// интервале): колонка переносится, но в Go-структуре её больше нет.
	_, _ = db.sql.ExecContext(ctx, `UPDATE chat_settings SET reply_check_seconds = ? WHERE chat_id = ?`, rpls, old)
	_ = db.SetEphemeralEnabled(ctx, old, true)
	_ = db.PutGreeting(ctx, old, 1, 777, now)
	_ = db.AddTrusted(ctx, old, 1, now)

	if err := db.MigrateChat(ctx, old, neu); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// От старого чата не должно остаться следов.
	chats, _ := db.ListChats(ctx)
	for _, c := range chats {
		if c.ChatID == old {
			t.Errorf("old chat still in chats table: %+v", c)
		}
	}
	if _, ok, _ := db.MemberJoinedAt(ctx, old, 1); ok {
		t.Error("old member still present")
	}

	// В новом чате должны оказаться перенесённые данные.
	if _, ok, _ := db.MemberJoinedAt(ctx, neu, 1); !ok {
		t.Error("member not migrated to new chat")
	}
	if ok, _ := db.IsTrusted(ctx, neu, 1); !ok {
		t.Error("trusted user not migrated to new chat")
	}
	if ok, _ := db.IsTrusted(ctx, old, 1); ok {
		t.Error("trusted row still on old chat")
	}
	s, err := db.QueryStats(ctx, neu, now.Add(-24*time.Hour), now.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if s.Joined != 1 || s.Passed != 1 {
		t.Errorf("events not migrated: %+v", s)
	}
	if s.MsgNewcomer != 1 {
		t.Errorf("message_counts not migrated: %d", s.MsgNewcomer)
	}
	// Миграцию обязаны переживать ВСЕ колонки настроек, а не только
	// greeting_enabled — это однажды регрессировало, когда новые колонки
	// добавили в chat_settings, но не в INSERT внутри MigrateChat.
	ms, err := db.GetChatSettings(ctx, neu)
	if err != nil {
		t.Fatal(err)
	}
	if ms.GreetingEnabled {
		t.Error("greeting_enabled=false did not migrate (still shows true default)")
	}
	if !ms.MaxAttempts.Valid || ms.MaxAttempts.Int64 != 5 {
		t.Errorf("max_attempts did not migrate: %+v", ms.MaxAttempts)
	}
	if !ms.CaptchaIntervalMinutes.Valid || ms.CaptchaIntervalMinutes.Int64 != 3 {
		t.Errorf("captcha_interval_minutes did not migrate: %+v", ms.CaptchaIntervalMinutes)
	}
	if !ms.DailyStatsEnabled {
		t.Error("daily_stats_enabled did not migrate")
	}
	if !ms.DailyStatsUTCHour.Valid || ms.DailyStatsUTCHour.Int64 != 21 {
		t.Errorf("daily_stats_utc_hour did not migrate: %+v", ms.DailyStatsUTCHour)
	}
	if !ms.CaptchaMode.Valid || ms.CaptchaMode.String != "emoji" {
		t.Errorf("captcha_mode did not migrate: %+v", ms.CaptchaMode)
	}
	if !ms.GreetingText.Valid || ms.GreetingText.String != "Привет, {name}!" {
		t.Errorf("greeting_text did not migrate: %+v", ms.GreetingText)
	}
	if !ms.GreetingEntities.Valid || ms.GreetingEntities.String != greetEnts {
		t.Errorf("greeting_entities did not migrate: %+v", ms.GreetingEntities)
	}
	if ms.SilentAnnounceEnabled {
		t.Error("silent_announce_enabled=false did not migrate (still shows true default)")
	}
	if !ms.SpamCheckEnabled {
		t.Error("spam_check_enabled did not migrate")
	}
	if !ms.SpamThreshold.Valid || ms.SpamThreshold.Int64 != 75 {
		t.Errorf("spam_threshold did not migrate: %+v", ms.SpamThreshold)
	}
	if !ms.SpamWhitelistMsgs.Valid || ms.SpamWhitelistMsgs.Int64 != 10 {
		t.Errorf("spam_whitelist_msgs did not migrate: %+v", ms.SpamWhitelistMsgs)
	}
	if !ms.SpamVoteMargin.Valid || ms.SpamVoteMargin.Int64 != 2 {
		t.Errorf("spam_vote_margin did not migrate: %+v", ms.SpamVoteMargin)
	}
	if !ms.ReplyCheckEnabled {
		t.Error("reply_check_enabled did not migrate")
	}
	if !ms.EphemeralEnabled {
		t.Error("ephemeral_enabled did not migrate")
	}
	// greetings старого чата чистятся: message id мертвы вместе с чатом.
	if _, ok, _ := db.TakeGreetingMsg(ctx, old, 1); ok {
		t.Error("old-chat greeting must be dropped on migration")
	}
}

func TestMigrateChat_MergesIntoExistingNewSide(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	old := int64(-5000)
	neu := int64(-100001)
	now := time.Now()

	// Данные уже есть с ОБЕИХ сторон для одного юзера/дня.
	_ = db.UpsertMember(ctx, old, 1, now.Add(-10*24*time.Hour)) // более ранний вход
	_ = db.UpsertMember(ctx, neu, 1, now.Add(-5*24*time.Hour))  // более поздний вход

	_ = db.RecordEvent(ctx, old, 1, EventJoin, now, "")
	_ = db.RecordEvent(ctx, neu, 1, EventJoin, now, "")

	_ = db.IncMessage(ctx, old, now, true) // старый чат: 1 newcomer, 0 oldtimer
	_ = db.IncMessage(ctx, neu, now, true) // новый чат: 1 newcomer, 0 oldtimer
	_ = db.IncMessage(ctx, neu, now, false)

	_, _ = db.RecordMessage(ctx, old, 1, now.Add(-1*time.Hour))
	_, _ = db.RecordMessage(ctx, neu, 1, now)
	_, _ = db.RecordMessage(ctx, neu, 1, now.Add(time.Minute))

	if err := db.MigrateChat(ctx, old, neu); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// joined_at участника должен быть БОЛЕЕ РАННИМ (из старого чата).
	joinedAt, ok, _ := db.MemberJoinedAt(ctx, neu, 1)
	if !ok {
		t.Fatal("member missing after merge")
	}
	expected := now.Add(-10 * 24 * time.Hour).Unix()
	if joinedAt.Unix() != expected {
		t.Errorf("joined_at should be earlier one: got %d want %d", joinedAt.Unix(), expected)
	}

	// events: суммируются (2 входа).
	s, _ := db.QueryStats(ctx, neu, now.Add(-2*24*time.Hour), now.AddDate(0, 0, 1))
	if s.Joined != 2 {
		t.Errorf("joined events: got %d want 2", s.Joined)
	}

	// message_counts: суммируются (2 newcomer, 1 oldtimer).
	if s.MsgNewcomer != 2 || s.MsgOldtimer != 1 {
		t.Errorf("messages: %+v, want 2/1", s)
	}

	// user_activity сливается (message_count суммируется).
	top, _ := db.TopWriters(ctx, neu, now.Add(-2*24*time.Hour), now.AddDate(0, 0, 1), 10)
	if len(top) != 1 {
		t.Fatalf("top writers: %+v", top)
	}
	if top[0].Count != 3 { // 1 из старого + 2 из нового
		t.Errorf("top writer count: got %d want 3", top[0].Count)
	}

	// Старая сторона полностью чиста.
	s2, _ := db.QueryStats(ctx, old, time.Unix(0, 0), now.AddDate(0, 0, 1))
	if s2.Joined != 0 || s2.MsgNewcomer != 0 {
		t.Errorf("old chat still has data: %+v", s2)
	}
}

func TestMigrateChat_Idempotent(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	// Повторный запуск миграции должен быть безопасен (второй прогон — no-op).
	old := int64(-5000)
	neu := int64(-100001)
	_ = db.UpsertMember(ctx, old, 1, time.Now())

	if err := db.MigrateChat(ctx, old, neu); err != nil {
		t.Fatal(err)
	}
	if err := db.MigrateChat(ctx, old, neu); err != nil {
		t.Fatalf("second migrate: %v", err)
	}

	if _, ok, _ := db.MemberJoinedAt(ctx, neu, 1); !ok {
		t.Error("member missing after double migrate")
	}
}

func TestMigrateChat_SameIDNoop(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	_ = db.UpsertMember(ctx, 42, 1, time.Now())
	if err := db.MigrateChat(ctx, 42, 42); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := db.MemberJoinedAt(ctx, 42, 1); !ok {
		t.Error("self-migrate wiped data")
	}
}

// TestLegacyCaptchaTimeoutConverted — разовая data-миграция при открытии БД:
// легаси captcha_timeout_seconds (секунды одиночного таймаута) конвертируется
// в captcha_interval_minutes (ceil до целых минут, минимум 1). Идемпотентно:
// непустое новое значение не перетирается.
func TestLegacyCaptchaTimeoutConverted(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")

	// Первый Open: сеем легаси-значения сырыми строками chat_settings.
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.sql.ExecContext(ctx, `
		INSERT INTO chat_settings (chat_id, captcha_timeout_seconds) VALUES
			(-1, 30), (-2, 60), (-3, 90), (-4, NULL)
	`)
	if err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	_ = db.Close()

	// Второй Open: миграция конвертации обязана отработать.
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	want := map[int64]int64{-1: 1, -2: 1, -3: 2, -4: 0} // 0 = остался NULL
	for chatID, w := range want {
		s, err := db.GetChatSettings(ctx, chatID)
		if err != nil {
			t.Fatal(err)
		}
		if w == 0 {
			if s.CaptchaIntervalMinutes.Valid {
				t.Errorf("chat %d: interval must stay NULL, got %+v", chatID, s.CaptchaIntervalMinutes)
			}
			continue
		}
		if !s.CaptchaIntervalMinutes.Valid || s.CaptchaIntervalMinutes.Int64 != w {
			t.Errorf("chat %d: interval = %+v, want %d", chatID, s.CaptchaIntervalMinutes, w)
		}
	}

	// Идемпотентность: третий Open не перетирает уже выставленное значение.
	intervalMin := 7
	if err := db.SetCaptchaIntervalMin(ctx, -1, &intervalMin); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, _ := db.GetChatSettings(ctx, -1)
	if !s.CaptchaIntervalMinutes.Valid || s.CaptchaIntervalMinutes.Int64 != 7 {
		t.Errorf("re-open must not overwrite explicit interval: %+v", s.CaptchaIntervalMinutes)
	}
}

func TestDeleteChat(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	_ = db.RememberChat(ctx, ChatInfo{ChatID: 1, Title: "A", Type: "group"})
	_ = db.UpsertMember(ctx, 1, 100, time.Now())

	if err := db.DeleteChat(ctx, 1); err != nil {
		t.Fatal(err)
	}

	// Чат удалён из реестра.
	chats, _ := db.ListChats(ctx)
	for _, c := range chats {
		if c.ChatID == 1 {
			t.Error("chat not removed from chats table")
		}
	}
	// Но исторические данные остаются.
	if _, ok, _ := db.MemberJoinedAt(ctx, 1, 100); !ok {
		t.Error("DeleteChat should keep historical data intact")
	}
}
