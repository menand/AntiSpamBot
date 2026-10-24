package storage

import (
	"context"
	"testing"
	"time"
)

func TestReplyWatchRoundTripAndDue(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	now := time.Now().Truncate(time.Second)
	w := ReplyWatch{
		ChatID: 10, UserID: 20, MessageID: 30, Text: "ответ",
		AnsweredAt: now, Stage: 1, NextAt: now.Add(5 * time.Minute),
	}
	if err := db.PutReplyWatch(ctx, w); err != nil {
		t.Fatal(err)
	}

	got, ok, err := db.GetReplyWatch(ctx, 10, 30)
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Text != w.Text || got.Stage != 1 || got.AnsweredAt.Unix() != now.Unix() ||
		got.NextAt.Unix() != w.NextAt.Unix() || got.DeletedBy != "" {
		t.Fatalf("unexpected watch: %+v", got)
	}

	due, err := db.DueReplyWatches(ctx, now.Add(time.Minute), 10)
	if err != nil || len(due) != 0 {
		t.Fatalf("not due yet: len=%d err=%v", len(due), err)
	}
	due, err = db.DueReplyWatches(ctx, now.Add(5*time.Minute), 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("due: len=%d err=%v", len(due), err)
	}

	// Upsert перезаписывает состояние (повторный ответ тем же сообщением).
	w.Stage = 0
	w.NextAt = now.Add(time.Minute)
	w.DeletedBy = "bot:del"
	if err := db.PutReplyWatch(ctx, w); err != nil {
		t.Fatal(err)
	}
	got, _, _ = db.GetReplyWatch(ctx, 10, 30)
	if got.Stage != 0 || got.NextAt.Unix() != w.NextAt.Unix() || got.DeletedBy != "bot:del" {
		t.Fatalf("upsert did not reset: %+v", got)
	}
}

func TestReplyWatchTakeIsSingleWinner(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	now := time.Now()
	if err := db.PutReplyWatch(ctx, ReplyWatch{
		ChatID: 1, UserID: 2, MessageID: 3, AnsweredAt: now, NextAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	first, ok, err := db.TakeReplyWatch(ctx, 1, 3)
	if err != nil || !ok || first.MessageID != 3 {
		t.Fatalf("first take: ok=%v err=%v watch=%+v", ok, err, first)
	}
	if _, ok, err := db.TakeReplyWatch(ctx, 1, 3); err != nil || ok {
		t.Fatalf("second take must lose: ok=%v err=%v", ok, err)
	}
}

func TestReplyWatchAdvanceGuardAndText(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	now := time.Now().Truncate(time.Second)
	if err := db.PutReplyWatch(ctx, ReplyWatch{
		ChatID: 1, UserID: 2, MessageID: 3, Text: "старое",
		AnsweredAt: now, NextAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	ok, err := db.AdvanceReplyWatch(ctx, 1, 3, 1, 2, now.Add(time.Minute))
	if err != nil || ok {
		t.Fatalf("advance from wrong stage must be a no-op: ok=%v err=%v", ok, err)
	}
	ok, err = db.AdvanceReplyWatch(ctx, 1, 3, 0, 1, now.Add(time.Minute))
	if err != nil || !ok {
		t.Fatalf("advance: ok=%v err=%v", ok, err)
	}
	// Первая правка уведомляет сразу; снимок и счётчик — в одной транзакции.
	edit, found, err := db.ClaimReplyWatchEdit(ctx, 1, 3, 2, "новое", now,
		5*time.Minute, 3)
	if err != nil || !found {
		t.Fatalf("claim edit: found=%v err=%v", found, err)
	}
	if !edit.ShouldNotify || edit.OldText != "старое" || edit.Text != "новое" ||
		edit.EditReports != 1 {
		t.Fatalf("unexpected first claim: %+v", edit)
	}
	got, _, _ := db.GetReplyWatch(ctx, 1, 3)
	if got.Text != "новое" || got.Stage != 1 || got.EditReports != 1 ||
		got.EditNotifiedAt.IsZero() {
		t.Fatalf("unexpected after advance/claim: %+v", got)
	}

	// Та же версия текста — не правка для отчёта.
	edit, found, err = db.ClaimReplyWatchEdit(ctx, 1, 3, 2, "новое", now.Add(time.Minute),
		5*time.Minute, 3)
	if err != nil || !found || edit.ShouldNotify {
		t.Fatalf("same text must not notify: %+v found=%v err=%v", edit, found, err)
	}

	// Кулдаун: карточку не шлём, но снимок обновляем.
	edit, found, err = db.ClaimReplyWatchEdit(ctx, 1, 3, 2, "третье", now.Add(time.Minute),
		5*time.Minute, 3)
	if err != nil || !found || edit.ShouldNotify || edit.Text != "третье" ||
		edit.EditReports != 1 {
		t.Fatalf("cooldown claim: %+v found=%v err=%v", edit, found, err)
	}
	// Через 5 минут — вторая карточка.
	edit, found, err = db.ClaimReplyWatchEdit(ctx, 1, 3, 2, "четвёртое", now.Add(6*time.Minute),
		5*time.Minute, 3)
	if err != nil || !found || !edit.ShouldNotify || edit.EditReports != 2 {
		t.Fatalf("second claim after cooldown: %+v found=%v err=%v", edit, found, err)
	}
	// Третья — последняя.
	edit, found, err = db.ClaimReplyWatchEdit(ctx, 1, 3, 2, "пятое", now.Add(12*time.Minute),
		5*time.Minute, 3)
	if err != nil || !found || !edit.ShouldNotify || edit.EditReports != 3 {
		t.Fatalf("third claim: %+v found=%v err=%v", edit, found, err)
	}
	// Лимит исчерпан: снимок меняется, карточки больше нет.
	edit, found, err = db.ClaimReplyWatchEdit(ctx, 1, 3, 2, "шестое", now.Add(30*time.Minute),
		5*time.Minute, 3)
	if err != nil || !found || edit.ShouldNotify || edit.EditReports != 3 ||
		edit.Text != "шестое" {
		t.Fatalf("cap claim: %+v found=%v err=%v", edit, found, err)
	}

	// Чужая правка (не автор) не должна менять снимок/слать карточку.
	if _, found, err := db.ClaimReplyWatchEdit(ctx, 1, 3, 999, "чужое",
		now.Add(time.Hour), 5*time.Minute, 3); err != nil || found {
		t.Fatalf("foreign edit must be ignored: found=%v err=%v", found, err)
	}
	if _, found, err := db.ClaimReplyWatchEdit(ctx, 1, 999, 2, "нет строки",
		now, 5*time.Minute, 3); err != nil || found {
		t.Fatalf("missing watch must be a no-op: found=%v err=%v", found, err)
	}
}

func TestReplyWatchDeferAndPruneStale(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	now := time.Now().Truncate(time.Second)
	for _, w := range []ReplyWatch{
		{ChatID: 1, UserID: 2, MessageID: 10, Text: "старый",
			AnsweredAt: now.Add(-3 * time.Hour), NextAt: now},
		{ChatID: 1, UserID: 2, MessageID: 11, Text: "свежий",
			AnsweredAt: now, NextAt: now},
	} {
		if err := db.PutReplyWatch(ctx, w); err != nil {
			t.Fatal(err)
		}
	}
	next := now.Add(5 * time.Minute)
	if err := db.DeferReplyWatch(ctx, 1, 11, next); err != nil {
		t.Fatal(err)
	}
	got, _, _ := db.GetReplyWatch(ctx, 1, 11)
	if got.NextAt.Unix() != next.Unix() || got.Stage != 0 {
		t.Fatalf("defer changed wrong fields: %+v", got)
	}
	if n, err := db.DeleteStaleReplyWatches(ctx, now.Add(-2*time.Hour)); err != nil || n != 1 {
		t.Fatalf("prune stale: n=%d err=%v", n, err)
	}
	if _, ok, _ := db.GetReplyWatch(ctx, 1, 10); ok {
		t.Fatal("stale watch must be pruned")
	}
	if _, ok, _ := db.GetReplyWatch(ctx, 1, 11); !ok {
		t.Fatal("fresh watch must survive prune")
	}
}

func TestReplyWatchMarksAndDeletes(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	now := time.Now()
	for _, msg := range []int{10, 11} {
		if err := db.PutReplyWatch(ctx, ReplyWatch{
			ChatID: 1, UserID: 2, MessageID: msg, Text: "ответ",
			AnsweredAt: now, NextAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.PutReplyWatch(ctx, ReplyWatch{
		ChatID: 2, UserID: 2, MessageID: 12, Text: "ответ",
		AnsweredAt: now, NextAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	if err := db.MarkReplyWatchDeletedByMessage(ctx, 1, 10, "bot:del"); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkReplyWatchesDeletedByUser(ctx, 1, 2, "bot:spam"); err != nil {
		t.Fatal(err)
	}
	// Первая непустая метка побеждает: поздний revoke-бан не переписывает
	// причину уже помеченного /del сообщения.
	got, _, _ := db.GetReplyWatch(ctx, 1, 10)
	if got.DeletedBy != "bot:del" {
		t.Fatalf("first mark must win: %q", got.DeletedBy)
	}
	got, _, _ = db.GetReplyWatch(ctx, 1, 11)
	if got.DeletedBy != "bot:spam" {
		t.Fatalf("mark by user missed the watch: %q", got.DeletedBy)
	}
	got, _, _ = db.GetReplyWatch(ctx, 2, 12)
	if got.DeletedBy != "" {
		t.Fatalf("mark leaked into another chat: %q", got.DeletedBy)
	}

	if err := db.DeleteReplyWatchesChat(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := db.GetReplyWatch(ctx, 1, 10); ok {
		t.Fatal("chat watches must be deleted")
	}
	if _, ok, _ := db.GetReplyWatch(ctx, 2, 12); !ok {
		t.Fatal("other chat watch must survive")
	}
	if err := db.DeleteAllReplyWatches(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := db.GetReplyWatch(ctx, 2, 12); ok {
		t.Fatal("all watches must be deleted")
	}
}

func TestMigrateChatDropsReplyWatches(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	now := time.Now()
	if err := db.PutReplyWatch(ctx, ReplyWatch{
		ChatID: -100, UserID: 2, MessageID: 3, Text: "ответ",
		AnsweredAt: now, NextAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.MigrateChat(ctx, -100, -200); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := db.GetReplyWatch(ctx, -100, 3); ok {
		t.Fatal("old-chat watch must be dropped on migration")
	}
}
