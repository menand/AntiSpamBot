package bot

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"

	"github.com/menand/AntiSpamBot/internal/storage"
)

const ownerDMOwnerID = 9001

func ownerDMReplyMsg(text string, replyToID int, quote *telego.TextQuote) telego.Message {
	m := telego.Message{
		MessageID: 900,
		Chat:      telego.Chat{ID: ownerDMOwnerID, Type: "private"},
		From:      &telego.User{ID: ownerDMOwnerID, FirstName: "Владелец"},
		Text:      text,
		Quote:     quote,
	}
	if replyToID != 0 {
		m.ReplyToMessage = &telego.Message{
			MessageID: replyToID,
			Chat:      telego.Chat{ID: ownerDMOwnerID, Type: "private"},
			From:      &telego.User{ID: testBotID, IsBot: true, FirstName: "Test"},
			Text:      "пересланное ботом сообщение",
		}
	}
	return m
}

// sendBodiesTo — тела sendMessage, ушедших в конкретный чат (в порядке
// отправки). Тела в fake-API разложены параллельно calls.
func sendBodiesTo(t *testing.T, fc *fakeCaller, chatID int64) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, raw := range fc.callBodies("sendMessage") {
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("unmarshal sendMessage body %q: %v", raw, err)
		}
		if id, ok := m["chat_id"].(float64); ok && int64(id) == chatID {
			out = append(out, m)
		}
	}
	return out
}

func replyParams(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	rp, ok := body["reply_parameters"].(map[string]any)
	if !ok {
		t.Fatalf("no reply_parameters in body %v", body)
	}
	return rp
}

func setupOwnerDMReply(t *testing.T) (*Bot, *storage.DB, *fakeCaller) {
	t.Helper()
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	b.cfg.OwnerIDs = map[int64]struct{}{ownerDMOwnerID: {}}
	return b, db, fc
}

// oneAttempt — одна попытка на шаг лестницы ретраев: иначе тест реально спит
// секунды на каждой инъекции ошибки.
func oneAttempt(t *testing.T) {
	t.Helper()
	old := tgBackoffs
	tgBackoffs = []time.Duration{0}
	t.Cleanup(func() { tgBackoffs = old })
}

func ownerNoticeText(t *testing.T, fc *fakeCaller) string {
	t.Helper()
	notices := sendBodiesTo(t, fc, ownerDMOwnerID)
	if len(notices) != 1 {
		t.Fatalf("want exactly one DM notice, got %v", notices)
	}
	text, _ := notices[0]["text"].(string)
	return text
}

// TestOwnerDMReplyPublishesWithQuote — владелец ответил реплаем на форвард,
// выделив часть текста: в исходный чат уходит текст от имени бота реплаем на
// исходное сообщение, в том же топике и с той же частичной цитатой.
func TestOwnerDMReplyPublishesWithQuote(t *testing.T) {
	b, db, fc := setupOwnerDMReply(t)
	ctx := context.Background()
	if err := db.PutOwnerDMLink(ctx, testBotID, ownerDMOwnerID, 900, testChatID, 77, 500, true, time.Now()); err != nil {
		t.Fatalf("put link: %v", err)
	}

	quote := &telego.TextQuote{Text: "фрагмент", Position: 5}
	if !b.handleOwnerDMReply(ctx, ownerDMReplyMsg("Ответ от бота", 900, quote)) {
		t.Fatalf("reply to linked message must be handled")
	}

	chatSends := sendBodiesTo(t, fc, testChatID)
	if len(chatSends) != 1 {
		t.Fatalf("sent %d messages to chat, want 1: %v", len(chatSends), chatSends)
	}
	body := chatSends[0]
	if got := body["text"]; got != "Ответ от бота" {
		t.Errorf("text = %v, want plain text", got)
	}
	if _, ok := body["parse_mode"]; ok {
		t.Errorf("parse_mode must be omitted (plain text)")
	}
	if got := body["message_thread_id"]; got != float64(77) {
		t.Errorf("message_thread_id = %v, want 77", got)
	}
	rp := replyParams(t, body)
	if got := rp["message_id"]; got != float64(500) {
		t.Errorf("reply message_id = %v, want 500", got)
	}
	if got := rp["quote"]; got != "фрагмент" {
		t.Errorf("quote = %v, want «фрагмент»", got)
	}
	if got := rp["quote_position"]; got != float64(5) {
		t.Errorf("quote_position = %v, want 5", got)
	}

	if notice := ownerNoticeText(t, fc); !strings.Contains(notice, "Опубликовано") {
		t.Fatalf("want success notice, got %q", notice)
	}
}

// TestOwnerDMReplyCardIgnoresQuote — ответ на карточку (линк не на форвард)
// уходит полным реплаем: цитата была бы фрагментом карточки, а не исходного
// сообщения. Владелец обязан узнать, что выделение не уехало.
func TestOwnerDMReplyCardIgnoresQuote(t *testing.T) {
	b, db, fc := setupOwnerDMReply(t)
	ctx := context.Background()
	if err := db.PutOwnerDMLink(ctx, testBotID, ownerDMOwnerID, 900, testChatID, 0, 500, false, time.Now()); err != nil {
		t.Fatalf("put link: %v", err)
	}
	quote := &telego.TextQuote{Text: "фрагмент карточки", Position: 2}
	if !b.handleOwnerDMReply(ctx, ownerDMReplyMsg("Ответ", 900, quote)) {
		t.Fatalf("reply to linked card must be handled")
	}
	chatSends := sendBodiesTo(t, fc, testChatID)
	if len(chatSends) != 1 {
		t.Fatalf("sent %d messages to chat, want 1", len(chatSends))
	}
	rp := replyParams(t, chatSends[0])
	if _, ok := rp["quote"]; ok {
		t.Fatalf("card link must not carry a partial quote: %v", rp)
	}
	if got := rp["message_id"]; got != float64(500) {
		t.Errorf("reply message_id = %v, want 500", got)
	}
	if notice := ownerNoticeText(t, fc); !strings.Contains(notice, "Цитату не перенёс") {
		t.Fatalf("card-quote drop must be named honestly, got %q", notice)
	}
}

// TestOwnerDMReplyTooLongQuote — цитата длиннее лимита Bot API отбрасывается
// целиком (обрезанная соврала бы о выделении), реплай остаётся, отбивка
// называет причину.
func TestOwnerDMReplyTooLongQuote(t *testing.T) {
	b, db, fc := setupOwnerDMReply(t)
	ctx := context.Background()
	if err := db.PutOwnerDMLink(ctx, testBotID, ownerDMOwnerID, 900, testChatID, 0, 500, true, time.Now()); err != nil {
		t.Fatalf("put link: %v", err)
	}
	long := strings.Repeat("я", ownerDMQuoteLimit+1)
	if !b.handleOwnerDMReply(ctx, ownerDMReplyMsg("Ответ", 900, &telego.TextQuote{Text: long})) {
		t.Fatalf("reply must be handled")
	}
	rp := replyParams(t, sendBodiesTo(t, fc, testChatID)[0])
	if _, ok := rp["quote"]; ok {
		t.Fatalf("oversized quote must be dropped")
	}
	if notice := ownerNoticeText(t, fc); !strings.Contains(notice, "1024") {
		t.Fatalf("oversized-quote notice must name the limit, got %q", notice)
	}
}

// TestOwnerDMReplyQuoteLost — цитата не нашлась (оригинал отредактировали):
// уходит полный реплай, а отбивка честно говорит, что цитаты не будет.
func TestOwnerDMReplyQuoteLost(t *testing.T) {
	oneAttempt(t)
	b, db, fc := setupOwnerDMReply(t)
	ctx := context.Background()
	if err := db.PutOwnerDMLink(ctx, testBotID, ownerDMOwnerID, 900, testChatID, 0, 500, true, time.Now()); err != nil {
		t.Fatalf("put link: %v", err)
	}
	fc.errSeq["sendMessage"] = []*telegoapi.Error{
		{ErrorCode: 400, Description: "Bad Request: quote not found in the replied message"},
	}
	quote := &telego.TextQuote{Text: "фрагмент", Position: 1}
	if !b.handleOwnerDMReply(ctx, ownerDMReplyMsg("Ответ", 900, quote)) {
		t.Fatalf("reply must be handled")
	}
	chatSends := sendBodiesTo(t, fc, testChatID)
	if len(chatSends) != 2 {
		t.Fatalf("attempts = %d, want 2 (quote, full reply): %v", len(chatSends), chatSends)
	}
	if _, ok := replyParams(t, chatSends[1])["quote"]; ok {
		t.Fatalf("second attempt must be a plain reply: %v", chatSends[1])
	}
	if notice := ownerNoticeText(t, fc); !strings.Contains(notice, "без цитаты") {
		t.Fatalf("quote-lost notice must admit the loss, got %q", notice)
	}
}

// TestOwnerDMReplyDeletedOriginal — исходное сообщение удалено: реплай не
// проходит — публикуем с allow_sending_without_reply и честно говорим, что
// ушло без реплая.
func TestOwnerDMReplyDeletedOriginal(t *testing.T) {
	oneAttempt(t)
	b, db, fc := setupOwnerDMReply(t)
	ctx := context.Background()
	if err := db.PutOwnerDMLink(ctx, testBotID, ownerDMOwnerID, 900, testChatID, 0, 500, true, time.Now()); err != nil {
		t.Fatalf("put link: %v", err)
	}
	fc.errSeq["sendMessage"] = []*telegoapi.Error{
		{ErrorCode: 400, Description: "Bad Request: message to be replied not found"},
	}
	if !b.handleOwnerDMReply(ctx, ownerDMReplyMsg("Ответ", 900, nil)) {
		t.Fatalf("reply must be handled")
	}
	chatSends := sendBodiesTo(t, fc, testChatID)
	if len(chatSends) != 2 {
		t.Fatalf("attempts = %d, want 2 (reply, bare): %v", len(chatSends), chatSends)
	}
	last := chatSends[len(chatSends)-1]
	rp := replyParams(t, last)
	if got := rp["allow_sending_without_reply"]; got != true {
		t.Fatalf("bare fallback must allow a missing reply target: %v", last)
	}
	if notice := ownerNoticeText(t, fc); !strings.Contains(notice, "исходное сообщение удалено") {
		t.Fatalf("want deletion notice, got %q", notice)
	}
}

// TestOwnerDMReplySendFailure — отправка невозможна (нет прав): фолбэки не
// гоняют заведомо неверные параметры, отбивка называет реальную причину.
func TestOwnerDMReplySendFailure(t *testing.T) {
	oneAttempt(t)
	b, db, fc := setupOwnerDMReply(t)
	ctx := context.Background()
	if err := db.PutOwnerDMLink(ctx, testBotID, ownerDMOwnerID, 900, testChatID, 0, 500, true, time.Now()); err != nil {
		t.Fatalf("put link: %v", err)
	}
	fc.errSeq["sendMessage"] = []*telegoapi.Error{
		{ErrorCode: 400, Description: "Bad Request: not enough rights to send text messages to the chat"},
	}
	if !b.handleOwnerDMReply(ctx, ownerDMReplyMsg("Ответ", 900, nil)) {
		t.Fatalf("failed publish must still consume the update")
	}
	if n := len(sendBodiesTo(t, fc, testChatID)); n != 1 {
		t.Fatalf("publish attempts = %d, want 1 (permanent error, no fallbacks)", n)
	}
	if notice := ownerNoticeText(t, fc); !strings.Contains(notice, "нет прав") {
		t.Fatalf("failure notice must name the real reason, got %q", notice)
	}
}

// TestOwnerDMReplyGates — чужая личка, неразмеченное сообщение, пустой текст
// и не-владелец обрабатываются обычным путём: бот не публикует ничего.
func TestOwnerDMReplyGates(t *testing.T) {
	b, db, fc := setupOwnerDMReply(t)
	ctx := context.Background()
	if err := db.PutOwnerDMLink(ctx, testBotID, ownerDMOwnerID, 900, testChatID, 0, 500, true, time.Now()); err != nil {
		t.Fatalf("put link: %v", err)
	}

	// Ответ на сообщение, которое бот не связывал и которое писал не он.
	foreign := ownerDMReplyMsg("Ответ", 1234, nil)
	foreign.ReplyToMessage.From = &telego.User{ID: testUserID, FirstName: "Гость"}
	if b.handleOwnerDMReply(ctx, foreign) {
		t.Fatalf("unmapped non-bot reply must not be handled")
	}
	// Стикер/медиа без текста и подписи на связанное сообщение.
	if !b.handleOwnerDMReply(ctx, ownerDMReplyMsg("", 900, nil)) {
		t.Fatalf("empty reply to a linked message must be handled with a notice")
	}
	if len(sendBodiesTo(t, fc, testChatID)) != 0 {
		t.Fatalf("nothing must be published")
	}
	if notice := ownerNoticeText(t, fc); !strings.Contains(notice, "только текст") {
		t.Fatalf("empty-reply notice must explain the rule, got %q", notice)
	}

	// Не владелец, даже с валидным линком.
	b.cfg.OwnerIDs = map[int64]struct{}{}
	if b.handleOwnerDMReply(ctx, ownerDMReplyMsg("Ответ", 900, nil)) {
		t.Fatalf("non-owner reply must not be handled")
	}
	if len(sendBodiesTo(t, fc, testChatID)) != 0 {
		t.Fatalf("non-owner must not publish anything")
	}
}

// TestOwnerDMReplyStaleLink — реплай на собственное сообщение бота, но связки
// нет (старше 30 дней / потерялась): молчать нельзя, иначе владелец уверен,
// что ответил в чат.
func TestOwnerDMReplyStaleLink(t *testing.T) {
	b, _, fc := setupOwnerDMReply(t)
	if !b.handleOwnerDMReply(context.Background(), ownerDMReplyMsg("Ответ", 1234, nil)) {
		t.Fatalf("reply to bot message without a link must be consumed")
	}
	if len(sendBodiesTo(t, fc, testChatID)) != 0 {
		t.Fatalf("nothing must be published")
	}
	if notice := ownerNoticeText(t, fc); !strings.Contains(notice, "устарела") {
		t.Fatalf("stale-link notice must explain the miss, got %q", notice)
	}
}

// TestOwnerDMReplyCaption — подпись медиа тоже текст владельца: публикуется
// она, сам файл остаётся в ЛС.
func TestOwnerDMReplyCaption(t *testing.T) {
	b, db, fc := setupOwnerDMReply(t)
	ctx := context.Background()
	if err := db.PutOwnerDMLink(ctx, testBotID, ownerDMOwnerID, 900, testChatID, 0, 500, true, time.Now()); err != nil {
		t.Fatalf("put link: %v", err)
	}
	m := ownerDMReplyMsg("", 900, nil)
	m.Caption = "Подпись вместо текста"
	if !b.handleOwnerDMReply(ctx, m) {
		t.Fatalf("caption reply must be handled")
	}
	chatSends := sendBodiesTo(t, fc, testChatID)
	if len(chatSends) != 1 || chatSends[0]["text"] != "Подпись вместо текста" {
		t.Fatalf("caption must be published, got %v", chatSends)
	}
}

// TestOwnerDMReplySlashText — незарегистрированная «команда» (опечатка,
// случайный /cancel) в чат от имени бота не уходит.
func TestOwnerDMReplySlashText(t *testing.T) {
	b, db, fc := setupOwnerDMReply(t)
	ctx := context.Background()
	if err := db.PutOwnerDMLink(ctx, testBotID, ownerDMOwnerID, 900, testChatID, 0, 500, true, time.Now()); err != nil {
		t.Fatalf("put link: %v", err)
	}
	if !b.handleOwnerDMReply(ctx, ownerDMReplyMsg("/note что-то", 900, nil)) {
		t.Fatalf("slash text must be consumed with a notice")
	}
	if len(sendBodiesTo(t, fc, testChatID)) != 0 {
		t.Fatalf("command-like text must not be published")
	}
	if notice := ownerNoticeText(t, fc); !strings.Contains(notice, "похоже на команду") {
		t.Fatalf("slash-text notice must explain the refusal, got %q", notice)
	}
}

// TestOwnerDMReplyUnserviceableChat — чат перестал обслуживаться: публикации
// нет, владелец получает честную отбивку.
func TestOwnerDMReplyUnserviceableChat(t *testing.T) {
	b, db, fc := setupOwnerDMReply(t)
	ctx := context.Background()
	if err := db.PutOwnerDMLink(ctx, testBotID, ownerDMOwnerID, 900, testChatID, 0, 500, true, time.Now()); err != nil {
		t.Fatalf("put link: %v", err)
	}
	b.approvalCache[testChatID] = false

	if !b.handleOwnerDMReply(ctx, ownerDMReplyMsg("Ответ", 900, nil)) {
		t.Fatalf("linked reply must be handled")
	}
	if len(sendBodiesTo(t, fc, testChatID)) != 0 {
		t.Fatalf("nothing must be published into an unserviceable chat")
	}
	if notice := ownerNoticeText(t, fc); !strings.Contains(notice, "больше не обслуживается") {
		t.Fatalf("want serviceability notice, got %q", notice)
	}
}

// TestOwnerDMReplySenderGates — бот-отправитель, отсутствующий From,
// не-приватный чат и business-личка (chat.id != from.id) ничего не публикуют
// даже при валидном линке: линк адресуется парой (owner, dm_message_id), и
// чужое пространство id не должно в него попадать.
func TestOwnerDMReplySenderGates(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(m *telego.Message)
	}{
		{name: "отправитель — бот", mutate: func(m *telego.Message) { m.From.IsBot = true }},
		{name: "нет From", mutate: func(m *telego.Message) { m.From = nil }},
		{name: "не приватный чат", mutate: func(m *telego.Message) { m.Chat.Type = telego.ChatTypeSupergroup }},
		{name: "business-личка", mutate: func(m *telego.Message) { m.Chat.ID = 4242 }},
		{name: "нет reply_to", mutate: func(m *telego.Message) { m.ReplyToMessage = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, db, fc := setupOwnerDMReply(t)
			ctx := context.Background()
			if err := db.PutOwnerDMLink(ctx, testBotID, ownerDMOwnerID, 900, testChatID, 0, 500, true, time.Now()); err != nil {
				t.Fatalf("put link: %v", err)
			}
			m := ownerDMReplyMsg("Ответ", 900, nil)
			tc.mutate(&m)
			if b.handleOwnerDMReply(ctx, m) {
				t.Fatalf("update must fall through to ordinary DM handling")
			}
			if n := fc.callCount("sendMessage"); n != 0 {
				t.Fatalf("nothing may be published, sends = %d", n)
			}
		})
	}
}

// TestOwnerDMReplyKeepsArmedWelcomeInput — ответ реплаем по линку публикуется,
// но взведённый ввод текста приветствия остаётся взведённым, а текст не
// становится шаблоном чата.
func TestOwnerDMReplyKeepsArmedWelcomeInput(t *testing.T) {
	b, db, fc := setupOwnerDMReply(t)
	ctx := context.Background()
	if err := db.PutOwnerDMLink(ctx, testBotID, ownerDMOwnerID, 900, testChatID, 0, 500, true, time.Now()); err != nil {
		t.Fatalf("put link: %v", err)
	}
	b.setGreetingInputPending(ownerDMOwnerID, testChatID, welcomeGreet, 501)

	if err := b.handlePrivateText(nil, ownerDMReplyMsg("Ответ", 900, nil)); err != nil {
		t.Fatal(err)
	}
	if len(sendBodiesTo(t, fc, testChatID)) != 1 {
		t.Fatalf("bridged reply must be published")
	}
	if len(b.greetInput) != 1 {
		t.Fatalf("bridged reply must not burn the armed welcome input, greetInput = %v", b.greetInput)
	}
	s, err := db.GetChatSettings(ctx, testChatID)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if s.GreetingText.String != "" {
		t.Fatalf("bridged reply must not become the greeting template: %q", s.GreetingText.String)
	}
}

// TestOwnerDMReplyPromptReplySavesTemplate — ответ реплаем ровно на промпт
// «пришли текст приветствия» — штатный ответ флоу, а не «устаревшая связка»:
// текст обязан сохраниться шаблоном, а не потеряться с чужой отбивкой.
func TestOwnerDMReplyPromptReplySavesTemplate(t *testing.T) {
	b, db, fc := setupOwnerDMReply(t)
	ctx := context.Background()
	b.setGreetingInputPending(ownerDMOwnerID, testChatID, welcomeGreet, 777)

	if err := b.handlePrivateText(nil, ownerDMReplyMsg("Новый текст", 777, nil)); err != nil {
		t.Fatal(err)
	}
	if n := len(sendBodiesTo(t, fc, testChatID)); n != 0 {
		t.Fatalf("prompt reply must not be published to the chat, sends = %d", n)
	}
	s, err := db.GetChatSettings(ctx, testChatID)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if s.GreetingText.String != "Новый текст" {
		t.Fatalf("prompt reply must become the greeting template, got %q", s.GreetingText.String)
	}
	if len(b.greetInput) != 0 {
		t.Fatalf("flow must consume the armed input, greetInput = %v", b.greetInput)
	}
}

// TestOwnerDMReplyExpiredPromptFallsThroughToFlow — ответ на протухший промпт
// флоу — это не «устаревшая связка»: мост пропускает апдейт, флоу честно
// говорит «запрос устарел, текст не сохранён» и снимает арм.
func TestOwnerDMReplyExpiredPromptFallsThroughToFlow(t *testing.T) {
	b, db, fc := setupOwnerDMReply(t)
	ctx := context.Background()
	b.greetInput[ownerDMOwnerID] = greetInputState{
		chatID:      testChatID,
		kind:        welcomeGreet,
		promptMsgID: 777,
		armedAt:     time.Now().Add(-greetInputTTL - time.Minute),
	}

	if err := b.handlePrivateText(nil, ownerDMReplyMsg("Поздний текст", 777, nil)); err != nil {
		t.Fatal(err)
	}
	if n := len(sendBodiesTo(t, fc, testChatID)); n != 0 {
		t.Fatalf("expired prompt reply must not be published, sends = %d", n)
	}
	notice := ownerNoticeText(t, fc)
	if !strings.Contains(notice, "устарел") {
		t.Fatalf("flow must say the request expired, got %q", notice)
	}
	if strings.Contains(notice, "связк") {
		t.Fatalf("expired prompt must not be diagnosed as a stale link, got %q", notice)
	}
	if _, ok := b.greetInput[ownerDMOwnerID]; ok {
		t.Fatalf("expired flow must clear the armed input")
	}
	s, err := db.GetChatSettings(ctx, testChatID)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if s.GreetingText.String != "" {
		t.Fatalf("expired text must not be saved as the template: %q", s.GreetingText.String)
	}
}

// TestOwnerDMReplyNoticeReplacesPrevious — отбивки живут одним экраном
// notice: вторая гасит первую, а не копится стопкой.
func TestOwnerDMReplyNoticeReplacesPrevious(t *testing.T) {
	b, db, fc := setupOwnerDMReply(t)
	ctx := context.Background()
	if err := db.PutOwnerDMLink(ctx, testBotID, ownerDMOwnerID, 900, testChatID, 0, 500, true, time.Now()); err != nil {
		t.Fatalf("put link: %v", err)
	}
	// У каждого сообщения свой id: иначе swap экрана честно решит, что это
	// то же самое сообщение, и ничего не удалит.
	fc.respSeq["sendMessage"] = []string{
		privateSentJSON(ownerDMOwnerID, 101), // публикация #1
		privateSentJSON(ownerDMOwnerID, 102), // отбивка #1
		privateSentJSON(ownerDMOwnerID, 103), // публикация #2
		privateSentJSON(ownerDMOwnerID, 104), // отбивка #2
	}
	for i := 0; i < 2; i++ {
		if !b.handleOwnerDMReply(ctx, ownerDMReplyMsg("Ответ", 900, nil)) {
			t.Fatalf("reply #%d must be handled", i+1)
		}
	}
	if n := fc.callCount("deleteMessage"); n != 1 {
		t.Fatalf("second notice must replace the first (deleteMessage calls = %d)", n)
	}
	if bodies := fc.callBodies("deleteMessage"); !strings.Contains(bodies[0], `"message_id":102`) {
		t.Fatalf("deleted %s, want previous notice 102", bodies[0])
	}
}

// TestOwnerDMLinkTTLPinned — 30 дней из CLAUDE.md: связка живёт ровно месяц.
func TestOwnerDMLinkTTLPinned(t *testing.T) {
	if ownerDMLinkTTL != 30*24*time.Hour {
		t.Fatalf("ownerDMLinkTTL = %v, want 30 дней (CLAUDE.md §Owner reply from DM)", ownerDMLinkTTL)
	}
}

// TestOwnerDMQuote — лимит Bot API считается в UTF-16 единицах: «🙂» = 2.
func TestOwnerDMQuote(t *testing.T) {
	emoji := strings.Repeat("🙂", ownerDMQuoteLimit/2)
	fwd := storage.OwnerDMLink{IsForward: true}
	cases := []struct {
		name string
		link storage.OwnerDMLink
		m    telego.Message
		want ownerQuoteDrop
	}{
		{name: "форвард + цитата", link: fwd, m: telego.Message{Quote: &telego.TextQuote{Text: "фрагмент"}}, want: ownerQuoteKept},
		{name: "карточка — цитата не наша", link: storage.OwnerDMLink{}, m: telego.Message{Quote: &telego.TextQuote{Text: "фрагмент"}}, want: ownerQuoteFromCard},
		{name: "цитаты нет", link: fwd, want: ownerQuoteAbsent},
		{name: "ровно лимит в UTF-16", link: fwd, m: telego.Message{Quote: &telego.TextQuote{Text: emoji}}, want: ownerQuoteKept},
		{name: "эмодзи: рун меньше, единиц больше", link: fwd, m: telego.Message{Quote: &telego.TextQuote{Text: emoji + "🙂"}}, want: ownerQuoteTooLong},
		{name: "пробельная цитата", link: fwd, m: telego.Message{Quote: &telego.TextQuote{Text: "  \n  "}}, want: ownerQuoteEmpty},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			quote, drop := ownerDMQuote(tc.m, tc.link)
			if drop != tc.want {
				t.Fatalf("drop = %v, want %v", drop, tc.want)
			}
			if (quote != nil) != (tc.want == ownerQuoteKept) {
				t.Fatalf("quote = %v, want kept=%v", quote, tc.want == ownerQuoteKept)
			}
		})
	}
}

// TestNotifyReplyAnsweredLinksMessages — форвард и карточка «прошёл 2-й
// уровень» связываются с исходным сообщением: по этим линкам работает ответ
// реплаем в ЛС. Подсказка «ответь на это сообщение» едет в карточке — без неё
// владелец не узнает о фиче.
func TestNotifyReplyAnsweredLinksMessages(t *testing.T) {
	b, db, fc := setupOwnerDMReply(t)
	ctx := context.Background()
	if err := db.SetModNotify(ctx, ownerDMOwnerID, true); err != nil {
		t.Fatalf("enable mod notify: %v", err)
	}
	fc.resp["forwardMessage"] = `{"message_id":777,"date":1700000000,
		"chat":{"id":9001,"type":"private"}}`

	msg := &telego.Message{
		MessageID: 501,
		Chat:      telego.Chat{ID: testChatID, Type: "supergroup", Title: "Тест"},
		From:      &telego.User{ID: testUserID, FirstName: "Гость"},
		Text:      "привет, хочу вступить",
	}
	b.notifyReplyAnswered(msg, &replyPending{ChatID: testChatID, UserID: testUserID, Stage: 1})

	forwardLink, ok, err := db.OwnerDMLink(ctx, testBotID, ownerDMOwnerID, 777)
	if err != nil || !ok {
		t.Fatalf("forward link: ok=%v err=%v", ok, err)
	}
	if !forwardLink.IsForward || forwardLink.ChatID != testChatID || forwardLink.SourceMsgID != 501 {
		t.Fatalf("forward link = %+v", forwardLink)
	}

	// Карточке fake-API выдал message_id 555 (см. newFlowBot).
	cardLink, ok, err := db.OwnerDMLink(ctx, testBotID, ownerDMOwnerID, 555)
	if err != nil || !ok {
		t.Fatalf("card link: ok=%v err=%v", ok, err)
	}
	if cardLink.IsForward || cardLink.SourceMsgID != 501 {
		t.Fatalf("card link = %+v", cardLink)
	}

	cards := sendBodiesTo(t, fc, ownerDMOwnerID)
	if len(cards) != 1 || !strings.Contains(cards[0]["text"].(string), "Ответь на это сообщение") {
		t.Fatalf("card must carry the reply hint, got %v", cards)
	}
}

// TestNotifySpamSuspicionLinksMessages — форвард подозрения и карточка
// связываются с сообщением нарушителя (топик сохраняется), а при запрещённом
// форварде фича деградирует до ответа на карточку, а не пропадает.
func TestNotifySpamSuspicionLinksMessages(t *testing.T) {
	cases := []struct {
		name            string
		forwardFails    bool
		wantForwardLink bool
	}{
		{name: "форвард прошёл", wantForwardLink: true},
		{name: "форвард запрещён", forwardFails: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, db, fc := setupOwnerDMReply(t)
			ctx := context.Background()
			if err := db.SetSpamNotify(ctx, ownerDMOwnerID, true); err != nil {
				t.Fatalf("enable spam notify: %v", err)
			}
			fc.resp["forwardMessage"] = `{"message_id":777,"date":1700000000,
				"chat":{"id":9001,"type":"private"}}`
			if tc.forwardFails {
				fc.err["forwardMessage"] = &telegoapi.Error{
					ErrorCode: 400, Description: "Bad Request: message can't be forwarded"}
			}
			msg := telego.Message{
				MessageID:       700,
				MessageThreadID: 3,
				IsTopicMessage:  true,
				Chat:            telego.Chat{ID: testChatID, Type: "supergroup", Title: "Тест"},
				From:            &telego.User{ID: testUserID, FirstName: "Гость"},
				Text:            "купите крипту",
			}
			b.notifySpamSuspicion(msg, "🤖 ИИ-антиспам")

			if tc.wantForwardLink {
				link, ok, err := db.OwnerDMLink(ctx, testBotID, ownerDMOwnerID, 777)
				if err != nil || !ok || !link.IsForward || link.SourceMsgID != 700 || link.ThreadID != 3 {
					t.Fatalf("forward link = %+v ok=%v err=%v", link, ok, err)
				}
			} else if _, ok, _ := db.OwnerDMLink(ctx, testBotID, ownerDMOwnerID, 777); ok {
				t.Fatalf("failed forward must not be linked")
			}
			cardLink, ok, err := db.OwnerDMLink(ctx, testBotID, ownerDMOwnerID, 555)
			if err != nil || !ok || cardLink.IsForward || cardLink.SourceMsgID != 700 {
				t.Fatalf("card link = %+v ok=%v err=%v", cardLink, ok, err)
			}
			cards := sendBodiesTo(t, fc, ownerDMOwnerID)
			if len(cards) != 1 || !strings.Contains(cards[0]["text"].(string), "Ответь на это сообщение") {
				t.Fatalf("card must carry the reply hint, got %v", cards)
			}
		})
	}
}
