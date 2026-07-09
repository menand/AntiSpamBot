package storage

import (
	"context"
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
	_ = db.RecordEvent(ctx, old, 1, EventJoin, now)
	_ = db.RecordEvent(ctx, old, 1, EventPass, now)
	_, _ = db.RecordMessage(ctx, old, 1, now)
	_ = db.IncMessage(ctx, old, now, true)
	_ = db.SetGreetingEnabled(ctx, old, false)
	maxAtt, tmo, hour, mode, greet := 5, 45, 21, "emoji", "Привет, {name}!"
	_ = db.SetMaxAttempts(ctx, old, &maxAtt)
	_ = db.SetCaptchaTimeoutSec(ctx, old, &tmo)
	_ = db.SetDailyStatsEnabled(ctx, old, true)
	_ = db.SetDailyStatsHour(ctx, old, &hour)
	_ = db.SetCaptchaMode(ctx, old, &mode)
	_ = db.SetGreetingText(ctx, old, &greet)
	_ = db.SetSilentAnnounceEnabled(ctx, old, false)
	sthr, swl, svm := 75, 10, 2
	_ = db.SetSpamCheckEnabled(ctx, old, true)
	_ = db.SetSpamThreshold(ctx, old, &sthr)
	_ = db.SetSpamWhitelistMsgs(ctx, old, &swl)
	_ = db.SetSpamVoteMargin(ctx, old, &svm)
	_ = db.PutGreeting(ctx, old, 1, 777, now)

	if err := db.MigrateChat(ctx, old, neu); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Old chat should have no traces left.
	chats, _ := db.ListChats(ctx)
	for _, c := range chats {
		if c.ChatID == old {
			t.Errorf("old chat still in chats table: %+v", c)
		}
	}
	if _, ok, _ := db.MemberJoinedAt(ctx, old, 1); ok {
		t.Error("old member still present")
	}

	// New chat should have the migrated data.
	if _, ok, _ := db.MemberJoinedAt(ctx, neu, 1); !ok {
		t.Error("member not migrated to new chat")
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
	// ALL settings columns must survive the migration, not just
	// greeting_enabled — this regressed once when new columns were added to
	// chat_settings but not to the MigrateChat INSERT.
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
	if !ms.CaptchaTimeoutSeconds.Valid || ms.CaptchaTimeoutSeconds.Int64 != 45 {
		t.Errorf("captcha_timeout_seconds did not migrate: %+v", ms.CaptchaTimeoutSeconds)
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

	// Pre-existing data on BOTH sides for same user/day.
	_ = db.UpsertMember(ctx, old, 1, now.Add(-10*24*time.Hour)) // earlier join
	_ = db.UpsertMember(ctx, neu, 1, now.Add(-5*24*time.Hour))  // later join

	_ = db.RecordEvent(ctx, old, 1, EventJoin, now)
	_ = db.RecordEvent(ctx, neu, 1, EventJoin, now)

	_ = db.IncMessage(ctx, old, now, true) // old: 1 newcomer, 0 old
	_ = db.IncMessage(ctx, neu, now, true) // new: 1 newcomer, 0 old
	_ = db.IncMessage(ctx, neu, now, false)

	_, _ = db.RecordMessage(ctx, old, 1, now.Add(-1*time.Hour))
	_, _ = db.RecordMessage(ctx, neu, 1, now)
	_, _ = db.RecordMessage(ctx, neu, 1, now.Add(time.Minute))

	if err := db.MigrateChat(ctx, old, neu); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// member joined_at should be the EARLIER one (from old chat).
	joinedAt, ok, _ := db.MemberJoinedAt(ctx, neu, 1)
	if !ok {
		t.Fatal("member missing after merge")
	}
	expected := now.Add(-10 * 24 * time.Hour).Unix()
	if joinedAt.Unix() != expected {
		t.Errorf("joined_at should be earlier one: got %d want %d", joinedAt.Unix(), expected)
	}

	// events: summed (2 joins).
	s, _ := db.QueryStats(ctx, neu, now.Add(-2*24*time.Hour), now.AddDate(0, 0, 1))
	if s.Joined != 2 {
		t.Errorf("joined events: got %d want 2", s.Joined)
	}

	// message_counts: summed (2 newcomer, 1 old).
	if s.MsgNewcomer != 2 || s.MsgOldtimer != 1 {
		t.Errorf("messages: %+v, want 2/1", s)
	}

	// user_activity merged (message_count summed).
	top, _ := db.TopWriters(ctx, neu, now.Add(-2*24*time.Hour), now.AddDate(0, 0, 1), 10)
	if len(top) != 1 {
		t.Fatalf("top writers: %+v", top)
	}
	if top[0].Count != 3 { // 1 from old + 2 from new
		t.Errorf("top writer count: got %d want 3", top[0].Count)
	}

	// old side fully clean.
	s2, _ := db.QueryStats(ctx, old, time.Unix(0, 0), now.AddDate(0, 0, 1))
	if s2.Joined != 0 || s2.MsgNewcomer != 0 {
		t.Errorf("old chat still has data: %+v", s2)
	}
}

func TestMigrateChat_Idempotent(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	// Running migrate twice should be safe (second run is a no-op).
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

func TestDeleteChat(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	_ = db.RememberChat(ctx, ChatInfo{ChatID: 1, Title: "A", Type: "group"})
	_ = db.UpsertMember(ctx, 1, 100, time.Now())

	if err := db.DeleteChat(ctx, 1); err != nil {
		t.Fatal(err)
	}

	// chat removed from registry
	chats, _ := db.ListChats(ctx)
	for _, c := range chats {
		if c.ChatID == 1 {
			t.Error("chat not removed from chats table")
		}
	}
	// but historical data stays
	if _, ok, _ := db.MemberJoinedAt(ctx, 1, 100); !ok {
		t.Error("DeleteChat should keep historical data intact")
	}
}
