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
	s, err := db.QueryStats(ctx, neu, now.Add(-24*time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if s.Joined != 1 || s.Passed != 1 {
		t.Errorf("events not migrated: %+v", s)
	}
	if s.MsgNewcomer != 1 {
		t.Errorf("message_counts not migrated: %d", s.MsgNewcomer)
	}
	greet, _ := db.GetGreetingEnabled(ctx, neu)
	if greet {
		t.Error("greeting_enabled=false did not migrate (still shows true default)")
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
	s, _ := db.QueryStats(ctx, neu, now.Add(-2*24*time.Hour), now.Add(time.Hour))
	if s.Joined != 2 {
		t.Errorf("joined events: got %d want 2", s.Joined)
	}

	// message_counts: summed (2 newcomer, 1 old).
	if s.MsgNewcomer != 2 || s.MsgOldtimer != 1 {
		t.Errorf("messages: %+v, want 2/1", s)
	}

	// user_activity merged (message_count summed).
	top, _ := db.TopWriters(ctx, neu, now.Add(-2*24*time.Hour), now.Add(time.Hour), 10)
	if len(top) != 1 {
		t.Fatalf("top writers: %+v", top)
	}
	if top[0].Count != 3 { // 1 from old + 2 from new
		t.Errorf("top writer count: got %d want 3", top[0].Count)
	}

	// old side fully clean.
	s2, _ := db.QueryStats(ctx, old, time.Unix(0, 0), now.Add(time.Hour))
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
