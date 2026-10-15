package storage

import (
	"context"
	"testing"
	"time"
)

func msg(chat, user, tgMsg int64, ts time.Time, kind, text string) StoredMessage {
	return StoredMessage{
		ChatID:  chat,
		UserID:  user,
		TGMsgID: tgMsg,
		TS:      ts,
		Kind:    kind,
		Text:    text,
	}
}

func TestAppendAndQueryRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	base := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	replyAuthor := "Alina"
	replyText := "Ребёнку два месяца"
	replyUID := int64(42)
	replyMsgID := int64(7)

	m1 := msg(-1001, 10, 100, base, "text", "Как твои руки?")
	m1.ReplyMsgID = &replyMsgID
	m1.ReplyUID = &replyUID
	m1.ReplyAuthor = &replyAuthor
	m1.ReplyText = &replyText
	m2 := msg(-1001, 11, 101, base.Add(time.Minute), "caption", "")
	m2.HasMedia = true

	if err := db.AppendMessages(ctx, []StoredMessage{m1, m2}); err != nil {
		t.Fatalf("append: %v", err)
	}

	got, err := db.QueryMessages(ctx, MessageFilter{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}

	a := got[0]
	if a.ChatID != -1001 || a.UserID != 10 || a.TGMsgID != 100 || a.Kind != "text" || a.Text != "Как твои руки?" {
		t.Fatalf("unexpected first row: %+v", a)
	}
	if a.ReplyUID == nil || *a.ReplyUID != replyUID {
		t.Fatalf("reply_uid mismatch: %+v", a.ReplyUID)
	}
	if a.ReplyAuthor == nil || *a.ReplyAuthor != replyAuthor {
		t.Fatalf("reply_author mismatch: %+v", a.ReplyAuthor)
	}
	if a.ReplyText == nil || *a.ReplyText != replyText {
		t.Fatalf("reply_text mismatch: %+v", a.ReplyText)
	}

	b := got[1]
	if !b.HasMedia || b.Kind != "caption" || b.Text != "" {
		t.Fatalf("unexpected second row: %+v", b)
	}
	if b.ReplyMsgID != nil || b.ReplyUID != nil {
		t.Fatalf("expected nil reply fields: %+v", b)
	}
}

func TestQueryMessagesFilters(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	day1 := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	if err := db.AppendMessages(ctx, []StoredMessage{
		msg(-1001, 10, 1, day1, "text", "про ананас и салат"),
		msg(-1001, 11, 2, day1, "text", "обычный текст"),
		msg(-1002, 10, 3, day2, "text", "ананас в другом чате"),
		msg(-1001, 10, 4, day2, "text", "второй ананас юзера 10"),
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	// По слову.
	got, err := db.QueryMessages(ctx, MessageFilter{Word: "ананас"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("word filter: got %d, want 3", len(got))
	}

	// По чату.
	got, _ = db.QueryMessages(ctx, MessageFilter{ChatID: -1002})
	if len(got) != 1 || got[0].TGMsgID != 3 {
		t.Fatalf("chat filter: %+v", got)
	}

	// По юзеру.
	got, _ = db.QueryMessages(ctx, MessageFilter{UserID: 10})
	if len(got) != 3 {
		t.Fatalf("user filter: got %d, want 3", len(got))
	}

	// По времени: [day2, ∞).
	got, _ = db.QueryMessages(ctx, MessageFilter{From: day2})
	if len(got) != 2 {
		t.Fatalf("from filter: got %d, want 2", len(got))
	}

	// Until — верхняя граница по МСК-дню, исключительно: day < DayOf(until).
	// until = полдень day2 → DayOf = «2026-10-14» → берём только day1 (2 шт.).
	got, _ = db.QueryMessages(ctx, MessageFilter{Until: day2})
	if len(got) != 2 {
		t.Fatalf("until filter: got %d, want 2 (day1 only)", len(got))
	}

	// Лимит.
	got, _ = db.QueryMessages(ctx, MessageFilter{Limit: 2})
	if len(got) != 2 {
		t.Fatalf("limit filter: got %d, want 2", len(got))
	}
}

func TestAppendMessagesEmptyBatchNoop(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	// Пустая пачка — не ошибка и не вставка: снимок до/after совпадает.
	if err := db.AppendMessages(ctx, nil); err != nil {
		t.Fatalf("empty append must be noop, got %v", err)
	}
	if err := db.AppendMessages(ctx, []StoredMessage{}); err != nil {
		t.Fatalf("zero-length append must be noop, got %v", err)
	}
	if got, _ := db.QueryMessages(ctx, MessageFilter{}); len(got) != 0 {
		t.Fatalf("rows after empty append = %d, want 0", len(got))
	}
}

// Все API сообщений обязаны возвращать ошибку на закрытой БД, а не молча
// succeed — иначе архиватор/msgtool получили бы ложный «всё хорошо». Один
// тест закрывает error-пути Append/Query/ForDay/Unarchived/DeleteDayThrough.
func TestMessageAPIsErrorOnClosedDB(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		call func() error
	}{
		{"AppendMessages", func() error {
			return db.AppendMessages(ctx, []StoredMessage{msg(-1001, 1, 1, base, "text", "x")})
		}},
		{"QueryMessages", func() error { _, err := db.QueryMessages(ctx, MessageFilter{}); return err }},
		{"MessagesForDay", func() error { _, err := db.MessagesForDay(ctx, DayOf(base)); return err }},
		{"UnarchivedDaysBefore", func() error { _, err := db.UnarchivedDaysBefore(ctx, base); return err }},
		{"DeleteDayThrough", func() error { return db.DeleteDayThrough(ctx, DayOf(base), 100) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.call(); err == nil {
				t.Fatal("want error on closed db")
			}
		})
	}
}

// Граница DeleteDayThrough: строка дня, вставленная ПОСЛЕ чтения (поздняя
// правка/реплей), имеет бо́льший id и обязана пережить удаление прочитанного —
// иначе попала бы под вычистку, не попав в архив (молчаливая потеря).
func TestDeleteDayThroughSparesRowsAfterRead(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	day := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

	if err := db.AppendMessages(ctx, []StoredMessage{
		msg(-1001, 10, 1, day, "text", "прочитано 1"),
		msg(-1001, 10, 2, day, "text", "прочитано 2"),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// «Чтение дня» архиватором: фиксируем границу maxID.
	read, err := db.MessagesForDay(ctx, DayOf(day))
	if err != nil {
		t.Fatal(err)
	}
	var maxID int64
	for _, m := range read {
		if m.ID > maxID {
			maxID = m.ID
		}
	}

	// Поздняя строка приходит МЕЖДУ чтением и удалением (та же секунда/день).
	if err := db.AppendMessages(ctx, []StoredMessage{
		msg(-1001, 11, 3, day.Add(time.Hour), "edit", "поздняя правка"),
	}); err != nil {
		t.Fatalf("late append: %v", err)
	}

	if err := db.DeleteDayThrough(ctx, DayOf(day), maxID); err != nil {
		t.Fatal(err)
	}

	got, err := db.MessagesForDay(ctx, DayOf(day))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Text != "поздняя правка" || got[0].ID <= maxID {
		t.Fatalf("late row must survive: %+v (maxID=%d)", got, maxID)
	}
}

func TestUnarchivedDaysAndDeleteDay(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	old1 := time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC)
	old2 := time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)
	fresh := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	if err := db.AppendMessages(ctx, []StoredMessage{
		msg(-1001, 10, 1, old1, "text", "старое"),
		msg(-1001, 10, 2, old2, "text", "старое 2"),
		msg(-1001, 10, 3, fresh, "text", "свежее"),
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	// Отсечка между old2 и fresh: возвращаются два старых дня (МСК-день).
	cutoff := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	days, err := db.UnarchivedDaysBefore(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 2 {
		t.Fatalf("days: got %v, want 2 старых дня", days)
	}

	// Удаляем один день — он пропадает из списка, свежий не появляется.
	// DeleteDayThrough принимает границу id: берём максимум прочитанного.
	deleted, err := db.MessagesForDay(ctx, days[0])
	if err != nil {
		t.Fatal(err)
	}
	var maxID int64
	for _, m := range deleted {
		if m.ID > maxID {
			maxID = m.ID
		}
	}
	if err := db.DeleteDayThrough(ctx, days[0], maxID); err != nil {
		t.Fatal(err)
	}
	days, _ = db.UnarchivedDaysBefore(ctx, cutoff)
	if len(days) != 1 {
		t.Fatalf("after delete: got %v, want 1", days)
	}
	remaining, _ := db.QueryMessages(ctx, MessageFilter{})
	if len(remaining) != 2 {
		t.Fatalf("remaining rows: got %d, want 2 (deleted day + fresh)", len(remaining))
	}
}

func TestMessagesForDayOrdering(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	day := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	if err := db.AppendMessages(ctx, []StoredMessage{
		msg(-1001, 10, 3, day.Add(2*time.Hour), "text", "третий"),
		msg(-1001, 10, 1, day.Add(time.Hour), "text", "первый"),
		msg(-1001, 10, 2, day.Add(30*time.Minute), "text", "нулевой"),
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	got, err := db.MessagesForDay(ctx, DayOf(day))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d, want 3", len(got))
	}
	want := []string{"нулевой", "первый", "третий"}
	for i, w := range want {
		if got[i].Text != w {
			t.Fatalf("position %d: got %q, want %q", i, got[i].Text, w)
		}
	}
}
