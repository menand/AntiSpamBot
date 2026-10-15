package bot

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mymmrac/telego"

	"github.com/menand/AntiSpamBot/internal/config"
	"github.com/menand/AntiSpamBot/internal/storage"
)

func TestTruncateRunes(t *testing.T) {
	// Кириллица — многобайтовая: обрезка должна идти по рунам, не по байтам.
	s := strings.Repeat("я", 50)
	got := truncateRunes(s, 10)
	if got != strings.Repeat("я", 10)+"…" {
		t.Fatalf("got %q", got)
	}
	// Проверяем, что многобайтовый хвост не порван: строка — валидный UTF-8 и
	// действительно обрывается на многоточии.
	if !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
		t.Fatalf("truncation broke UTF-8: %q", got)
	}
	// Короткая строка не трогается.
	if truncateRunes("аб", 10) != "аб" {
		t.Fatal("short string should be unchanged")
	}
}

func TestCaptureKindFor(t *testing.T) {
	cases := []struct {
		name string
		m    telego.Message
		want string
	}{
		{"text", telego.Message{Text: "привет"}, "text"},
		{"caption", telego.Message{Caption: "подпись"}, "caption"},
		{"media no text", telego.Message{Photo: []telego.PhotoSize{{FileID: "x"}}}, "media"},
		{"text beats caption", telego.Message{Text: "т", Caption: "п"}, "text"},
		{"empty is media", telego.Message{}, "media"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := captureKindFor(&c.m); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestCaptureFromMessageQuote(t *testing.T) {
	replyFrom := &telego.User{ID: 42, FirstName: "Alina", LastName: "Petrova"}
	m := &telego.Message{
		Chat:      telego.Chat{ID: -1001},
		From:      &telego.User{ID: 10, FirstName: "Oleg"},
		MessageID: 100,
		Date:      1760000000,
		Text:      "Как твои руки?",
		ReplyToMessage: &telego.Message{
			MessageID: 7,
			From:      replyFrom,
			Text:      "Ребёнку два месяца",
		},
	}
	rec := captureFromMessage(m)
	if rec.ChatID != -1001 || rec.UserID != 10 || rec.TGMsgID != 100 {
		t.Fatalf("identity: %+v", rec)
	}
	if rec.Text != "Как твои руки?" {
		t.Fatalf("text: %q", rec.Text)
	}
	if rec.ReplyMsgID == nil || *rec.ReplyMsgID != 7 {
		t.Fatalf("reply msg id: %+v", rec.ReplyMsgID)
	}
	if rec.ReplyUID == nil || *rec.ReplyUID != 42 {
		t.Fatalf("reply uid: %+v", rec.ReplyUID)
	}
	if rec.ReplyAuthor == nil || *rec.ReplyAuthor != "Alina Petrova" {
		t.Fatalf("reply author: %+v", rec.ReplyAuthor)
	}
	if rec.ReplyText == nil || *rec.ReplyText != "Ребёнку два месяца" {
		t.Fatalf("reply text: %+v", rec.ReplyText)
	}
}

func TestCaptureFromMessageNoReply(t *testing.T) {
	m := &telego.Message{
		Chat:      telego.Chat{ID: -1001},
		From:      &telego.User{ID: 10},
		MessageID: 5,
		Date:      1760000000,
		Text:      "просто текст",
	}
	rec := captureFromMessage(m)
	if rec.ReplyMsgID != nil || rec.ReplyUID != nil || rec.ReplyAuthor != nil || rec.ReplyText != nil {
		t.Fatalf("expected nil reply fields: %+v", rec)
	}
}

// TS правки обязан браться из EditDate: с оригинальным Date поздняя правка
// попала бы в уже упакованный день и урезала его архив (потеря данных).
func TestCaptureFromMessageUsesEditDateForEdits(t *testing.T) {
	cases := []struct {
		name     string
		date     int64
		editDate int64
		want     int64
	}{
		{"правка — берём EditDate", 1760000000, 1760864000, 1760864000},
		{"без правки — берём Date", 1760000000, 0, 1760000000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := &telego.Message{
				Chat:      telego.Chat{ID: -1001},
				From:      &telego.User{ID: 10},
				MessageID: 5,
				Date:      c.date,
				EditDate:  c.editDate,
				Text:      "текст",
			}
			if got := captureFromMessage(m).TS.Unix(); got != c.want {
				t.Fatalf("TS = %d, want %d", got, c.want)
			}
		})
	}
}

func TestCaptureFromMessageMediaHasNoTextButFlagged(t *testing.T) {
	m := &telego.Message{
		Chat:      telego.Chat{ID: -1001},
		From:      &telego.User{ID: 10},
		MessageID: 6,
		Date:      1760000000,
		Photo:     []telego.PhotoSize{{FileID: "abc"}},
	}
	rec := captureFromMessage(m)
	if rec.Text != "" {
		t.Fatalf("photo should have empty text, got %q", rec.Text)
	}
	if !rec.HasMedia {
		t.Fatal("photo should set HasMedia")
	}
}

func TestDisplayName(t *testing.T) {
	cases := []struct {
		u    *telego.User
		want string
	}{
		{&telego.User{FirstName: "Alina", LastName: "Petrova"}, "Alina Petrova"},
		{&telego.User{FirstName: "Oleg"}, "Oleg"},
		{&telego.User{Username: "oleg"}, "@oleg"},
		{&telego.User{}, ""},
	}
	for _, c := range cases {
		if got := displayName(c.u); got != c.want {
			t.Fatalf("displayName(%+v) = %q, want %q", c.u, got, c.want)
		}
	}
}

func TestCaptureFromMessageReplyCaptionFallback(t *testing.T) {
	// Цитата — медиа без текста, но с подписью: берём подпись.
	m := &telego.Message{
		Chat: telego.Chat{ID: -1001}, From: &telego.User{ID: 10}, MessageID: 5, Date: 1,
		ReplyToMessage: &telego.Message{MessageID: 7, From: &telego.User{ID: 42}, Caption: "подпись медиа"},
	}
	rec := captureFromMessage(m)
	if rec.ReplyText == nil || *rec.ReplyText != "подпись медиа" {
		t.Fatalf("reply caption fallback: %+v", rec.ReplyText)
	}
}

func TestCaptureMessageDropsWhenChannelFull(t *testing.T) {
	b, _ := newTestBotWithDB(t)
	b.cfg = &config.Config{MessageCaptureEnabled: true}
	b.captureCh = make(chan storage.StoredMessage, 1)
	b.captureMessage(&telego.Message{Chat: telego.Chat{ID: 1}, From: &telego.User{ID: 2}, MessageID: 1, Date: 1, Text: "a"}, "text")
	// Вторая — не должна блокировать (channel full → drop).
	b.captureMessage(&telego.Message{Chat: telego.Chat{ID: 1}, From: &telego.User{ID: 2}, MessageID: 2, Date: 1, Text: "b"}, "text")
	if len(b.captureCh) != 1 {
		t.Fatalf("len(ch) = %d, want 1 (second dropped)", len(b.captureCh))
	}
}

func TestCaptureMessageDisabledIsNoop(t *testing.T) {
	b, _ := newTestBotWithDB(t)
	b.cfg = &config.Config{MessageCaptureEnabled: false}
	b.captureCh = make(chan storage.StoredMessage, 1)
	b.captureMessage(&telego.Message{Chat: telego.Chat{ID: 1}, From: &telego.User{ID: 2}, MessageID: 1, Date: 1, Text: "a"}, "text")
	if len(b.captureCh) != 0 {
		t.Fatal("disabled capture must not enqueue")
	}
}

func TestCaptureWriterFlushPaths(t *testing.T) {
	tests := []struct {
		name      string
		push      int
		cancelNow bool // отменить ctx сразу (проверка финального флаша)
	}{
		{"batch full flush", captureBatchMax, false},
		{"ctx done flush", 3, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, db := newTestBotWithDB(t)
			b.cfg = &config.Config{MessageCaptureEnabled: true}
			b.captureCh = make(chan storage.StoredMessage, captureBufSize)
			b.captureDone = make(chan struct{})
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { b.captureWriter(ctx); close(done) }()
			for i := 0; i < tc.push; i++ {
				b.captureCh <- storage.StoredMessage{
					ChatID: 1, UserID: 2, TGMsgID: int64(i), TS: time.Now(), Kind: "text", Text: "привет",
				}
			}
			if tc.cancelNow {
				cancel()
			} else {
				// Ждём таймер-флаш (captureFlushInt), затем останавливаем.
				time.Sleep(captureFlushInt + 300*time.Millisecond)
				cancel()
			}
			<-done
			got, err := db.QueryMessages(context.Background(), storage.MessageFilter{})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tc.push {
				t.Fatalf("rows = %d, want %d", len(got), tc.push)
			}
		})
	}
}

// Настоящая ошибка БД (не отмена контекста): пачка логируется и отбрасывается,
// writer НЕ зависает — горутина доезжает до ctx.Done и закрывает captureDone.
// Проверяет ветку «wctx жив → warn + drop» в flush.
func TestCaptureWriterDBErrorDropsBatchAndExits(t *testing.T) {
	b, db := newTestBotWithDB(t)
	b.cfg = &config.Config{MessageCaptureEnabled: true}
	b.captureCh = make(chan storage.StoredMessage, captureBufSize)
	b.captureDone = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	go b.captureWriter(ctx)

	// БД мертва → AppendMessages упадёт настоящей ошибкой (не ctx-отменой).
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	b.captureCh <- storage.StoredMessage{ChatID: 1, UserID: 2, TS: time.Now(), Kind: "text", Text: "a"}
	// Ждём таймер-флаш с ошибкой, потом останавливаем: финальный флаш тоже
	// упадёт, но не зависнет и закроет captureDone.
	time.Sleep(captureFlushInt + 300*time.Millisecond)
	cancel()
	select {
	case <-b.captureDone:
	case <-time.After(captureShutdownTimeout + time.Second):
		t.Fatal("writer must exit on ctx.Done even after DB errors")
	}
}

func TestWaitCaptureDone(t *testing.T) {
	b, _ := newTestBotWithDB(t)
	b.captureDone = make(chan struct{})

	// Выключен: мгновенный no-op (captureDone никогда не закроется).
	b.cfg = &config.Config{MessageCaptureEnabled: false}
	done := make(chan struct{})
	go func() { b.waitCaptureDone(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("disabled capture must not wait")
	}

	// Включён, канал уже закрыт: мгновенный возврат.
	b.cfg.MessageCaptureEnabled = true
	close(b.captureDone)
	start := time.Now()
	b.waitCaptureDone()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("closed captureDone must return promptly, took %v", elapsed)
	}
}
