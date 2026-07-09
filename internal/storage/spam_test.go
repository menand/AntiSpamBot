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

	// Голоса: upsert + переголосование.
	_ = db.UpsertBallot(ctx, -1, 100, 7, true)
	_ = db.UpsertBallot(ctx, -1, 100, 8, true)
	_ = db.UpsertBallot(ctx, -1, 100, 9, false)
	_ = db.UpsertBallot(ctx, -1, 100, 9, true) // передумал
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
	for i := 0; i < 3; i++ {
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
