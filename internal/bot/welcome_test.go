package bot

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/menand/AntiSpamBot/internal/storage"
)

// nsNS — валидный sql.NullString для табличных случаев.
func nsNS(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }

func TestEffectiveWelcomeMode(t *testing.T) {
	tests := []struct {
		name string
		s    storage.ChatSettings
		want welcomeMode
	}{
		// Странный чат без обоих легаси-флагов (руками правили БД) — молчим.
		{"нет флагов", storage.ChatSettings{}, welcomeNone},
		// Дефолт defaultChatSettings: greeting_enabled=1 → greet.
		{"дефолт новой строки", storage.ChatSettings{GreetingEnabled: true}, welcomeGreet},
		{"легаси reply-check", storage.ChatSettings{GreetingEnabled: true, ReplyCheckEnabled: true}, welcomeReply},
		{"reply-check важнее приветствия", storage.ChatSettings{ReplyCheckEnabled: true}, welcomeReply},
		// Явный выбор режима бьёт легаси-флаги в любую сторону.
		{"явный none бьёт легаси reply", storage.ChatSettings{GreetingEnabled: true, ReplyCheckEnabled: true, WelcomeMode: nsNS("none")}, welcomeNone},
		{"явный greet бьёт легаси reply", storage.ChatSettings{ReplyCheckEnabled: true, WelcomeMode: nsNS("greet")}, welcomeGreet},
		{"явный reply при выключенных легаси", storage.ChatSettings{WelcomeMode: nsNS("reply")}, welcomeReply},
		// Битое/будущее значение — откат к деривации, не паника и не пустота.
		{"битое значение", storage.ChatSettings{GreetingEnabled: true, WelcomeMode: nsNS("bogus")}, welcomeGreet},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveWelcomeMode(tc.s); got != tc.want {
				t.Errorf("effectiveWelcomeMode = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDefaultNewChatModeIsGreet — дефолт НОВЫХ чатов: строки chat_settings нет,
// GetChatSettings отдаёт defaultChatSettings с GreetingEnabled=true, деривация
// даёт «приветствие». Пин на случай, если дефолты когда-нибудь поедут.
func TestDefaultNewChatModeIsGreet(t *testing.T) {
	ctx := context.Background()
	b, db, _ := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)

	s, err := db.GetChatSettings(ctx, testChatID)
	if err != nil {
		t.Fatal(err)
	}
	if !s.GreetingEnabled {
		t.Fatal("defaultChatSettings должны нести GreetingEnabled=true — иначе дефолт «после капчи» поедет")
	}
	if got := effectiveWelcomeMode(s); got != welcomeGreet {
		t.Fatalf("новый чат: режим = %q, want greet", got)
	}
}

// TestSendGreetingAnchorModes — тело якоря по режимам: none молчит, greet
// шлёт текст приветствия без требования и кнопок, reply — текст проверки со
// строкой-требованием и админ-кнопками; карантинная сноска добавляется в оба
// говорящих режима.
func TestSendGreetingAnchorModes(t *testing.T) {
	ctx := context.Background()

	t.Run("none не шлёт ничего", func(t *testing.T) {
		b, _, fc := newFlowBot(t)
		s := storage.ChatSettings{GreetingEnabled: true, GreetingText: nsNS("не должно уйти")}
		msgID, ok := b.sendGreetingAnchor(ctx, s, welcomeNone, testChatID, testUserID, 0, 1)
		if !ok || msgID != 0 {
			t.Fatalf("none: got (%d, %v), want (0, true)", msgID, ok)
		}
		if n := fc.callCount("sendMessage"); n != 0 {
			t.Fatalf("none must stay silent, sends=%d", n)
		}
	})

	t.Run("greet: текст приветствия, без требования и кнопок", func(t *testing.T) {
		b, _, fc := newFlowBot(t)
		s := storage.ChatSettings{GreetingEnabled: true, GreetingText: nsNS("Ласково, {name}!")}
		msgID, ok := b.sendGreetingAnchor(ctx, s, welcomeGreet, testChatID, testUserID, 0, 1)
		if !ok || msgID == 0 {
			t.Fatalf("greet send failed: (%d, %v)", msgID, ok)
		}
		body := fc.callBodies("sendMessage")[0]
		if !strings.Contains(body, "Ласково") {
			t.Errorf("greeting body expected, got: %s", body)
		}
		if strings.Contains(body, "Напиши что-нибудь") {
			t.Errorf("greet must not carry the requirement line: %s", body)
		}
		if strings.Contains(body, "rpok:") || strings.Contains(body, "reply_markup") {
			t.Errorf("greet must not carry admin buttons: %s", body)
		}
	})

	t.Run("reply: текст проверки с требованием и кнопками", func(t *testing.T) {
		b, _, fc := newFlowBot(t)
		s := storage.ChatSettings{
			GreetingEnabled: true, ReplyCheckEnabled: true,
			ReplyText: nsNS("Проверочка, {name}?"),
		}
		msgID, ok := b.sendGreetingAnchor(ctx, s, welcomeReply, testChatID, testUserID, 0, 1)
		if !ok || msgID == 0 {
			t.Fatalf("reply send failed: (%d, %v)", msgID, ok)
		}
		body := fc.callBodies("sendMessage")[0]
		if !strings.Contains(body, "Проверочка") {
			t.Errorf("reply body expected, got: %s", body)
		}
		if !strings.Contains(body, "Напиши что-нибудь") {
			t.Errorf("requirement line expected: %s", body)
		}
		if !strings.Contains(body, "rpok:") {
			t.Errorf("admin buttons expected: %s", body)
		}
	})

	t.Run("reply без кастомного текста — встроенный дефолт", func(t *testing.T) {
		b, _, fc := newFlowBot(t)
		s := storage.ChatSettings{ReplyCheckEnabled: true}
		b.sendGreetingAnchor(ctx, s, welcomeReply, testChatID, testUserID, 0, 1)
		body := fc.callBodies("sendMessage")[0]
		if !strings.Contains(body, "а ты точно не спамер") {
			t.Errorf("built-in reply text expected, got: %s", body)
		}
	})

	t.Run("карантин добавляет сноску", func(t *testing.T) {
		b, _, fc := newFlowBot(t)
		s := storage.ChatSettings{GreetingEnabled: true, QuarantineEnabled: true}
		b.sendGreetingAnchor(ctx, s, welcomeGreet, testChatID, testUserID, 0, 1)
		body := fc.callBodies("sendMessage")[0]
		if !strings.Contains(body, "Карантин новичка") {
			t.Errorf("quarantine notice expected, got: %s", body)
		}
	})
}

// TestOnSuccessWelcomeModes — режимы на НАСТОЯЩЕМ onSuccess. Отдельный пин на
// регрессию v1.13–v1.19: в однофакторном режиме приветствие не уходило вовсе
// (ветка `else if s.ReplyCheckEnabled` теряла greet-путь).
func TestOnSuccessWelcomeModes(t *testing.T) {
	ctx := context.Background()

	t.Run("greet: пасс сразу, приветствие уходит", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		pend := putCaptcha(b, db, testChatID, testUserID, 77)
		if err := b.onSuccess(ctx, pend, ""); err != nil {
			t.Fatal(err)
		}
		if kinds := statsKinds(t, db, testChatID, testUserID); kinds[storage.EventPass] != 1 {
			t.Fatalf("pass = %d, want 1", kinds[storage.EventPass])
		}
		if n := fc.callCount("sendMessage"); n != 1 {
			t.Fatalf("приветствие должно уходить ровно одним сообщением, sends=%d", n)
		}
	})

	t.Run("none: пасс сразу, сообщений нет", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		if err := db.SetWelcomeMode(ctx, testChatID, string(welcomeNone)); err != nil {
			t.Fatal(err)
		}
		pend := putCaptcha(b, db, testChatID, testUserID, 77)
		if err := b.onSuccess(ctx, pend, ""); err != nil {
			t.Fatal(err)
		}
		if kinds := statsKinds(t, db, testChatID, testUserID); kinds[storage.EventPass] != 1 {
			t.Fatalf("pass = %d, want 1", kinds[storage.EventPass])
		}
		if n := fc.callCount("sendMessage"); n != 0 {
			t.Fatalf("none must stay silent, sends=%d", n)
		}
	})

	t.Run("reply: пасс ждёт ответа, якорь с требованием", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		if err := db.SetReplyCheckEnabled(ctx, testChatID, true); err != nil {
			t.Fatal(err)
		}
		pend := putCaptcha(b, db, testChatID, testUserID, 77)
		if err := b.onSuccess(ctx, pend, ""); err != nil {
			t.Fatal(err)
		}
		// Пасс отложен до ответа (при записи ответа его поставит
		// replyWaitSatisfied) — иначе молчание закрылось бы киком поверх пасса.
		if kinds := statsKinds(t, db, testChatID, testUserID); kinds[storage.EventPass] != 0 {
			t.Fatalf("pass = %d, want 0 (ждём ответа)", kinds[storage.EventPass])
		}
		if _, ok := b.replies.Get(testChatID, testUserID); !ok {
			t.Fatal("reply wait must be armed")
		}
		bodies := fc.callBodies("sendMessage")
		if len(bodies) != 1 || !strings.Contains(bodies[0], "Напиши что-нибудь") {
			t.Fatalf("anchor with the requirement expected, bodies = %v", bodies)
		}
		// Гасим серию, чтобы её таймер не пострелял после закрытия БД.
		if p, ok := b.replies.Take(testChatID, testUserID); ok {
			p.Cancel()
		}
	})
}
