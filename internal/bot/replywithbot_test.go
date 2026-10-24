package bot

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"
)

const (
	replyWithBotOwnerID = 9
	replyWithBotAdminID = 10
	replyWithBotGuestID = 11
)

func replyWithBotTargetMsg() *telego.Message {
	return &telego.Message{
		MessageID: 500,
		Chat:      telego.Chat{ID: testChatID, Type: "supergroup"},
		From:      &telego.User{ID: testUserID, FirstName: "Гость"},
		Text:      "Привет",
	}
}

func replyWithBotMessage(text string, target *telego.Message) telego.Message {
	return telego.Message{
		MessageID:      501,
		Chat:           telego.Chat{ID: testChatID, Type: "supergroup"},
		From:           &telego.User{ID: replyWithBotOwnerID, FirstName: "Владелец"},
		Text:           text,
		ReplyToMessage: target,
	}
}

func jsonBody(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("unmarshal request body %q: %v", body, err)
	}
	return m
}

func botAdminJSON() string {
	return `{"status":"administrator","user":{"id":42,"is_bot":true,"first_name":"Test"},"can_restrict_members":true}`
}

// TestReplyWithBotOwnerSendsPlainReply — владелец отвечает на сообщение юзера:
// текст уходит от имени бота реплаем на цель, без parse_mode, а сообщение с
// командой удаляется. Никаких событий статистики команда не пишет.
func TestReplyWithBotOwnerSendsPlainReply(t *testing.T) {
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	b.cfg.OwnerIDs = map[int64]struct{}{replyWithBotOwnerID: {}}

	msg := replyWithBotMessage("/replywithbot Привет от бота", replyWithBotTargetMsg())
	if err := b.handleReplyWithBotCommand(nil, msg); err != nil {
		t.Fatal(err)
	}

	sends := fc.callBodies("sendMessage")
	if len(sends) != 1 {
		t.Fatalf("sendMessage calls = %d, want 1: %v", len(sends), sends)
	}
	body := jsonBody(t, sends[0])
	if got := body["text"]; got != "Привет от бота" {
		t.Fatalf("text = %v, want %q", got, "Привет от бота")
	}
	if _, ok := body["parse_mode"]; ok {
		t.Fatalf("plain text must not carry parse_mode: %v", body)
	}
	if _, ok := body["ephemeral_message_parameters"]; ok {
		t.Fatalf("bot reply must be public even in ephemeral mode: %v", body)
	}
	rp, ok := body["reply_parameters"].(map[string]any)
	if !ok {
		t.Fatalf("reply_parameters missing: %v", body)
	}
	if got := rp["message_id"]; got != float64(500) {
		t.Fatalf("reply message_id = %v, want 500", got)
	}
	if got := rp["allow_sending_without_reply"]; got != true {
		t.Fatalf("allow_sending_without_reply = %v, want true", got)
	}

	if fc.callCount("deleteMessage") != 1 {
		t.Fatalf("deleteMessage calls = %d, want 1: %v",
			fc.callCount("deleteMessage"), fc.callList())
	}
	del := jsonBody(t, fc.callBodies("deleteMessage")[0])
	if got := del["message_id"]; got != float64(501) {
		t.Fatalf("deleted message_id = %v, want 501 (the command)", got)
	}

	for kind, n := range statsKinds(t, db, testChatID, testUserID) {
		if n != 0 {
			t.Fatalf("event %s = %d, want 0 (replywithbot must not touch stats)", kind, n)
		}
	}
}

// TestReplyWithBotNonOwnerAdminRefused — админ чата проходит modPrologue, но
// команда только для владельца: вежливый отказ, команда удалена, мьюта нет.
func TestReplyWithBotNonOwnerAdminRefused(t *testing.T) {
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	b.cfg.OwnerIDs = map[int64]struct{}{replyWithBotOwnerID: {}}
	fc.respSeq["getChatMember"] = []string{
		botAdminJSON(),
		`{"status":"administrator","user":{"id":10,"is_bot":false,"first_name":"Админ"}}`,
	}

	msg := replyWithBotMessage("/replywithbot Привет от бота", replyWithBotTargetMsg())
	msg.From = &telego.User{ID: replyWithBotAdminID, FirstName: "Админ"}
	if err := b.handleReplyWithBotCommand(nil, msg); err != nil {
		t.Fatal(err)
	}

	sends := fc.callBodies("sendMessage")
	if len(sends) != 1 {
		t.Fatalf("sendMessage calls = %d, want 1 refusal: %v", len(sends), sends)
	}
	if !strings.Contains(sends[0], "только для владельца") {
		t.Fatalf("want owner-only refusal, got: %s", sends[0])
	}
	if strings.Contains(sends[0], "Привет от бота") {
		t.Fatalf("non-owner must not publish the text: %s", sends[0])
	}
	if fc.callCount("restrictChatMember") != 0 {
		t.Fatalf("admin refusal must not mute: %v", fc.callList())
	}
	if fc.callCount("deleteMessage") != 1 {
		t.Fatalf("command must be deleted, calls: %v", fc.callList())
	}
}

// TestReplyWithBotNonAdminPunished — обычный участник, как и на прочих
// модкомандах, получает минутный мьют и «не балуйся»; текст не публикуется.
func TestReplyWithBotNonAdminPunished(t *testing.T) {
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	b.cfg.OwnerIDs = map[int64]struct{}{replyWithBotOwnerID: {}}
	fc.respSeq["getChatMember"] = []string{
		botAdminJSON(),
		`{"status":"member","user":{"id":11,"is_bot":false,"first_name":"Гость"}}`,
	}

	msg := replyWithBotMessage("/replywithbot Привет от бота", replyWithBotTargetMsg())
	msg.From = &telego.User{ID: replyWithBotGuestID, FirstName: "Гость"}
	if err := b.handleReplyWithBotCommand(nil, msg); err != nil {
		t.Fatal(err)
	}

	if fc.callCount("restrictChatMember") == 0 {
		t.Fatalf("non-admin must be muted: %v", fc.callList())
	}
	sends := fc.callBodies("sendMessage")
	if len(sends) != 1 || !strings.Contains(sends[0], "не балуйся") {
		t.Fatalf("want punish notice, got: %v", sends)
	}
	if strings.Contains(sends[0], "Привет от бота") {
		t.Fatalf("non-admin must not publish the text: %s", sends[0])
	}
	if fc.callCount("deleteMessage") != 1 {
		t.Fatalf("command must be deleted, calls: %v", fc.callList())
	}
}

// TestReplyWithBotRefusals — без реплая, с пустым текстом и от анонимного
// админа команда не публикует ничего: только подсказка/отказ, а сама команда
// удаляется.
func TestReplyWithBotRefusals(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		target  *telego.Message
		from    *telego.User
		sender  *telego.Chat
		wantSub string
	}{
		{"нет реплая", "/replywithbot Привет", nil,
			&telego.User{ID: replyWithBotOwnerID, FirstName: "Владелец"}, nil,
			"Ответь командой"},
		{"пустой текст", "/replywithbot", replyWithBotTargetMsg(),
			&telego.User{ID: replyWithBotOwnerID, FirstName: "Владелец"}, nil,
			"Напиши текст"},
		{"только пробелы", "/replywithbot   ", replyWithBotTargetMsg(),
			&telego.User{ID: replyWithBotOwnerID, FirstName: "Владелец"}, nil,
			"Напиши текст"},
		{"анонимный админ", "/replywithbot Привет", replyWithBotTargetMsg(),
			&telego.User{ID: groupAnonymousBotID, IsBot: true, FirstName: "GroupAnonymousBot"},
			&telego.Chat{ID: testChatID, Type: "supergroup"},
			"только для владельца"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, db, fc := newFlowBot(t)
			serviceableChat(t, b, db, testChatID)
			b.cfg.OwnerIDs = map[int64]struct{}{replyWithBotOwnerID: {}}

			msg := replyWithBotMessage(tc.text, tc.target)
			msg.From = tc.from
			msg.SenderChat = tc.sender
			if err := b.handleReplyWithBotCommand(nil, msg); err != nil {
				t.Fatal(err)
			}

			sends := fc.callBodies("sendMessage")
			if len(sends) != 1 {
				t.Fatalf("sendMessage calls = %d, want 1 refusal: %v", len(sends), sends)
			}
			if !strings.Contains(sends[0], tc.wantSub) {
				t.Fatalf("want %q in refusal, got: %s", tc.wantSub, sends[0])
			}
			if strings.Contains(sends[0], "Привет") {
				t.Fatalf("refusal must not publish the text: %s", sends[0])
			}
			if fc.callCount("deleteMessage") != 1 {
				t.Fatalf("command must be deleted, calls: %v", fc.callList())
			}
		})
	}
}

// TestReplyWithBotPlainTextAndThread — HTML остаётся текстом (никакого
// parse_mode), переносы строк сохраняются, в форуме ответ уходит в тот же
// топик.
func TestReplyWithBotPlainTextAndThread(t *testing.T) {
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	b.cfg.OwnerIDs = map[int64]struct{}{replyWithBotOwnerID: {}}

	msg := replyWithBotMessage("/replywithbot <b>жирный</b> & <script>\nвторая строка", replyWithBotTargetMsg())
	msg.IsTopicMessage = true
	msg.MessageThreadID = 77
	if err := b.handleReplyWithBotCommand(nil, msg); err != nil {
		t.Fatal(err)
	}

	sends := fc.callBodies("sendMessage")
	if len(sends) != 1 {
		t.Fatalf("sendMessage calls = %d, want 1: %v", len(sends), sends)
	}
	body := jsonBody(t, sends[0])
	if got := body["text"]; got != "<b>жирный</b> & <script>\nвторая строка" {
		t.Fatalf("text = %q", got)
	}
	if _, ok := body["parse_mode"]; ok {
		t.Fatalf("HTML must stay plain text: %v", body)
	}
	if got := body["message_thread_id"]; got != float64(77) {
		t.Fatalf("message_thread_id = %v, want 77", got)
	}
}

// TestReplyWithBotStaysPublicInEphemeralMode — эфемерный режим чата не делает
// ответ бота приватным: текст обязан увидеть весь чат.
func TestReplyWithBotStaysPublicInEphemeralMode(t *testing.T) {
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	b.cfg.OwnerIDs = map[int64]struct{}{replyWithBotOwnerID: {}}
	if err := db.SetEphemeralEnabled(context.Background(), testChatID, true); err != nil {
		t.Fatal(err)
	}

	msg := replyWithBotMessage("/replywithbot Привет всем", replyWithBotTargetMsg())
	if err := b.handleReplyWithBotCommand(nil, msg); err != nil {
		t.Fatal(err)
	}

	sends := fc.callBodies("sendMessage")
	if len(sends) != 1 {
		t.Fatalf("sendMessage calls = %d, want 1: %v", len(sends), sends)
	}
	body := jsonBody(t, sends[0])
	if _, ok := body["ephemeral_message_parameters"]; ok {
		t.Fatalf("ephemeral mode must not hide the bot's public reply: %v", body)
	}
	if got := body["text"]; got != "Привет всем" {
		t.Fatalf("text = %v, want %q", got, "Привет всем")
	}
}

// TestReplyWithBotSendFailureKeepsCommand — отправка не удалась после ретраев:
// владелец получает ошибку, а команда остаётся в чате (текст не потерян).
func TestReplyWithBotSendFailureKeepsCommand(t *testing.T) {
	old := tgBackoffs
	tgBackoffs = []time.Duration{0, time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { tgBackoffs = old })

	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	b.cfg.OwnerIDs = map[int64]struct{}{replyWithBotOwnerID: {}}
	fc.errWhen = func(method string, data *telegoapi.RequestData) bool {
		return method == "sendMessage" && data != nil && data.BodyRaw != nil &&
			strings.Contains(string(data.BodyRaw), "Привет от бота")
	}

	msg := replyWithBotMessage("/replywithbot Привет от бота", replyWithBotTargetMsg())
	if err := b.handleReplyWithBotCommand(nil, msg); err != nil {
		t.Fatal(err)
	}

	if got := fc.callCount("sendMessage"); got != len(tgBackoffs)+1 {
		t.Fatalf("sendMessage calls = %d, want %d (retries + failure notice): %v",
			got, len(tgBackoffs)+1, fc.callList())
	}
	sends := fc.callBodies("sendMessage")
	if !strings.Contains(sends[len(sends)-1], "Не получилось отправить") {
		t.Fatalf("want failure notice, got: %s", sends[len(sends)-1])
	}
	if fc.callCount("deleteMessage") != 0 {
		t.Fatalf("command must survive a failed send, calls: %v", fc.callList())
	}
}

// TestReplyWithBotText — парсер аргумента: команда с @username, переводы
// строк, пустой остаток.
func TestReplyWithBotText(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"/replywithbot привет", "привет"},
		{"/replywithbot@antispam_bot привет", "привет"},
		{"/replywithbot\nпривет\nмир", "привет\nмир"},
		{"/replywithbot\tпривет", "привет"},
		{"/replywithbot  привет  ", "привет"},
		{"/replywithbot", ""},
		{"/replywithbot   ", ""},
		{"привет", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := replyWithBotText(tt.in); got != tt.want {
			t.Errorf("replyWithBotText(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestReplyWithBotTarget — целью может быть только содержательное сообщение
// живого пользователя.
func TestReplyWithBotTarget(t *testing.T) {
	base := func() *telego.Message { return replyWithBotTargetMsg() }
	tests := []struct {
		name string
		mod  func(*telego.Message)
		want bool
	}{
		{"обычный юзер", func(*telego.Message) {}, true},
		{"юзер с подписью к медиа", func(m *telego.Message) {
			m.Text = ""
			m.Caption = "подпись"
		}, true},
		{"бот", func(m *telego.Message) { m.From = &telego.User{ID: 5, IsBot: true} }, false},
		{"сервисный Telegram", func(m *telego.Message) { m.From = &telego.User{ID: telegramServiceUserID} }, false},
		{"канал", func(m *telego.Message) {
			m.SenderChat = &telego.Chat{ID: -100777, Type: "channel"}
		}, false},
		{"автофорвард", func(m *telego.Message) { m.IsAutomaticForward = true }, false},
		{"корень топика", func(m *telego.Message) { m.ForumTopicCreated = &telego.ForumTopicCreated{} }, false},
		{"без From", func(m *telego.Message) { m.From = nil }, false},
		{"без контента", func(m *telego.Message) { m.Text = "" }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := base()
			tt.mod(m)
			if _, ok := replyWithBotTarget(m); ok != tt.want {
				t.Fatalf("replyWithBotTarget() = %v, want %v", ok, tt.want)
			}
		})
	}
	if _, ok := replyWithBotTarget(nil); ok {
		t.Fatal("nil target must be rejected")
	}
}
