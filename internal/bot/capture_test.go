package bot

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"

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
	for _, tc := range []struct {
		name             string
		push             int
		flushBeforeClose bool
	}{
		{"batch full flush", captureBatchMax, true},
		{"timer flush", 1, true},
		{"queue close flush", 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, db := newTestBotWithDB(t)
			b.cfg = &config.Config{MessageCaptureEnabled: true}
			b.startCaptureWriter()
			defer b.stopCaptureWriter()
			for i := 0; i < tc.push; i++ {
				b.captureCh <- storage.StoredMessage{
					ChatID: 1, UserID: 2, TGMsgID: int64(i), TS: time.Now(), Kind: "text", Text: "привет",
				}
			}
			if tc.flushBeforeClose {
				waitFor(t, func() bool {
					rows, err := db.QueryMessages(t.Context(), storage.MessageFilter{})
					return err == nil && len(rows) == tc.push
				})
			}
			b.stopCaptureWriter()
			got, err := db.QueryMessages(t.Context(), storage.MessageFilter{})
			if err != nil || len(got) != tc.push {
				t.Fatalf("rows = %d, want %d: %v", len(got), tc.push, err)
			}
		})
	}
}

func TestCaptureWriterDBErrorDropsBatchAndExits(t *testing.T) {
	b, db := newTestBotWithDB(t)
	b.cfg = &config.Config{MessageCaptureEnabled: true}
	b.startCaptureWriter()
	defer b.stopCaptureWriter()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	b.captureCh <- storage.StoredMessage{ChatID: 1, UserID: 2, TS: time.Now(), Kind: "text", Text: "a"}
	b.stopCaptureWriter()
	select {
	case <-b.captureDone:
	default:
		t.Fatal("writer did not finish after DB error")
	}
}

func TestCaptureShutdownDrainsLateHandler(t *testing.T) {
	b, _, _ := newFlowBot(t)
	path := filepath.Join(t.TempDir(), "capture.db")
	db, err := storage.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	b.db = db
	b.cfg.MessageCaptureEnabled = true
	b.startCaptureWriter()
	defer b.stopCaptureWriter()

	updates := make(chan telego.Update, 1)
	handler, err := th.NewBotHandler(b.api, updates)
	if err != nil {
		t.Fatal(err)
	}
	polling, stop := context.WithCancel(t.Context())
	defer stop()
	started := make(chan struct{})
	allowWrite := make(chan struct{})
	defer func() {
		select {
		case <-allowWrite:
		default:
			close(allowWrite)
		}
	}()
	message := telego.Message{Chat: telego.Chat{ID: 1}, From: &telego.User{ID: 2}, MessageID: 21, Date: time.Now().Unix(), Text: "late capture"}
	handler.HandleMessage(func(_ *th.Context, msg telego.Message) error {
		close(started)
		<-polling.Done()
		<-allowWrite
		b.captureMessage(&msg, "text")
		return nil
	})
	updates <- telego.Update{UpdateID: 1, Message: &message}
	handlerDone := make(chan error, 1)
	go func() { handlerDone <- handler.Start() }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("capture producer did not start")
	}
	shutdownDone := make(chan struct{})
	go func() {
		b.stopUpdateHandling(stop, handler)
		close(shutdownDone)
	}()
	<-polling.Done()
	select {
	case <-b.captureDone:
		t.Fatal("writer exited before the active producer")
	default:
	}
	close(allowWrite)
	select {
	case <-shutdownDone:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not drain the producer and writer")
	}
	if err := <-handlerDone; err != nil {
		t.Fatal(err)
	}

	// An accidental call after teardown must not send to a closed channel.
	b.captureMessage(&message, "text")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := storage.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	rows, err := reopened.QueryMessages(t.Context(), storage.MessageFilter{})
	if err != nil || len(rows) != 1 || rows[0].TGMsgID != 21 {
		t.Fatalf("late handler message was lost or duplicated: %v / %v", rows, err)
	}
}

func TestCaptureWriterExitsOnEarlyRunReturn(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		b, _, fc := newFlowBot(t)
		b.cfg.DBPath = filepath.Join(t.TempDir(), "run.db")
		b.cfg.MessageCaptureEnabled = enabled
		fc.resp["getUpdates"] = "[]"
		polling, stop := context.WithCancel(t.Context())
		updates, err := b.api.UpdatesViaLongPolling(polling, nil, telego.WithLongPollingUpdateInterval(100*time.Millisecond))
		if err != nil {
			stop()
			t.Fatal(err)
		}
		err = b.Run(t.Context())
		stop()
		for range updates {
		}
		if err == nil || !strings.Contains(err.Error(), "long polling:") || t.Context().Err() != nil {
			t.Fatalf("expected early polling error with a live parent: %v", err)
		}
		if !enabled {
			if b.captureDone != nil || b.captureCh != nil {
				t.Fatal("disabled capture started a writer")
			}
			continue
		}
		if b.captureCh != nil {
			t.Fatal("early exit left the capture queue open")
		}
		select {
		case <-b.captureDone:
		default:
			t.Fatal("early exit left the writer running")
		}
	}
}
