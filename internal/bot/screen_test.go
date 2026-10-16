package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"

	"github.com/menand/AntiSpamBot/internal/storage"
)

// privateMsg — синтетическое входящее ЛС-сообщение (команда или текст).
func privateMsg(userID int64, text string) telego.Message {
	return telego.Message{
		MessageID: 1,
		From:      &telego.User{ID: userID, FirstName: "Админ"},
		Chat:      telego.Chat{ID: userID, Type: "private"},
		Text:      text,
	}
}

// privateSentJSON — ответ fake-API на отправку ЛС-сообщения с заданным id.
func privateSentJSON(userID int64, msgID int) string {
	return fmt.Sprintf(`{"message_id":%d,"date":1700000000,"chat":{"id":%d,"type":"private"}}`, msgID, userID)
}

func TestStartReplacesOldMenu(t *testing.T) {
	b, _, fc := newFlowBot(t)
	uid := int64(777)
	fc.respSeq["sendMessage"] = []string{
		privateSentJSON(uid, 101),
		privateSentJSON(uid, 102),
	}

	if err := b.handlePrivateStart(nil, privateMsg(uid, "/start")); err != nil {
		t.Fatal(err)
	}
	if got := fc.callCount("deleteMessage"); got != 0 {
		t.Fatalf("deleteMessage calls = %d on first start, want 0", got)
	}
	if err := b.handlePrivateStart(nil, privateMsg(uid, "/start")); err != nil {
		t.Fatal(err)
	}
	if got := fc.callCount("deleteMessage"); got != 1 {
		t.Fatalf("deleteMessage calls = %d on second start, want 1", got)
	}
	if bodies := fc.callBodies("deleteMessage"); !strings.Contains(bodies[0], `"message_id":101`) {
		t.Fatalf("deleted %s, want previous menu 101", bodies[0])
	}
}

func TestPanelScreensReplaceEachOther(t *testing.T) {
	b, _, fc := newFlowBot(t)
	uid := int64(778)
	fc.respSeq["sendMessage"] = []string{
		privateSentJSON(uid, 201),
		privateSentJSON(uid, 202),
	}

	if err := b.handlePrivateStart(nil, privateMsg(uid, "/start")); err != nil {
		t.Fatal(err)
	}
	if err := b.handleHelpCommand(nil, privateMsg(uid, "/help")); err != nil {
		t.Fatal(err)
	}
	if got := fc.callCount("deleteMessage"); got != 1 {
		t.Fatalf("deleteMessage calls = %d, want 1 (menu replaced by help)", got)
	}
	if bodies := fc.callBodies("deleteMessage"); !strings.Contains(bodies[0], `"message_id":201`) {
		t.Fatalf("deleted %s, want previous panel 201", bodies[0])
	}
}

func TestReplaceScreenKeepsKindsSeparate(t *testing.T) {
	b, _, fc := newFlowBot(t)
	uid := int64(779)
	ctx := context.Background()

	b.replaceScreen(ctx, uid, screenPanel, 10)
	b.replaceScreen(ctx, uid, screenPrompt, 20)
	if got := fc.callCount("deleteMessage"); got != 0 {
		t.Fatalf("deleteMessage calls = %d, want 0 (kinds must not touch each other)", got)
	}
	b.replaceScreen(ctx, uid, screenPanel, 30)
	b.replaceScreen(ctx, uid, screenPrompt, 40)
	bodies := fc.callBodies("deleteMessage")
	if len(bodies) != 2 ||
		!strings.Contains(bodies[0], `"message_id":10`) ||
		!strings.Contains(bodies[1], `"message_id":20`) {
		t.Fatalf("deleted %v, want previous panel 10 then previous prompt 20", bodies)
	}
}

func TestFailedScreenSendKeepsPrevious(t *testing.T) {
	b, _, fc := newFlowBot(t)
	uid := int64(780)
	fc.respSeq["sendMessage"] = []string{
		privateSentJSON(uid, 301),
		privateSentJSON(uid, 302), // будет отброшен инъекцией ошибки
		privateSentJSON(uid, 303),
	}

	if err := b.handlePrivateStart(nil, privateMsg(uid, "/start")); err != nil {
		t.Fatal(err)
	}
	fc.err["sendMessage"] = &telegoapi.Error{ErrorCode: 400, Description: "injected"}
	if err := b.handlePrivateStart(nil, privateMsg(uid, "/start")); err != nil {
		t.Fatal(err)
	}
	if got := fc.callCount("deleteMessage"); got != 0 {
		t.Fatalf("failed send must not delete the previous screen (got %d deletes)", got)
	}
	delete(fc.err, "sendMessage")
	if err := b.handlePrivateStart(nil, privateMsg(uid, "/start")); err != nil {
		t.Fatal(err)
	}
	if bodies := fc.callBodies("deleteMessage"); len(bodies) != 1 || !strings.Contains(bodies[0], `"message_id":301`) {
		t.Fatalf("deleted %v, want the surviving previous menu 301", bodies)
	}
}

func TestLogsReplacesPreviousDocument(t *testing.T) {
	b, _, fc := newFlowBot(t)
	uid := int64(781)
	b.cfg.OwnerIDs = map[int64]struct{}{uid: {}}
	logPath := filepath.Join(t.TempDir(), "bot.log")
	if err := os.WriteFile(logPath, []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b.cfg.LogFile = logPath
	fc.respSeq["sendDocument"] = []string{
		privateSentJSON(uid, 401),
		privateSentJSON(uid, 402),
	}

	if err := b.handleLogsCommand(nil, privateMsg(uid, "/logs")); err != nil {
		t.Fatal(err)
	}
	if err := b.handleLogsCommand(nil, privateMsg(uid, "/logs")); err != nil {
		t.Fatal(err)
	}
	if got := fc.callCount("sendDocument"); got != 2 {
		t.Fatalf("sendDocument calls = %d, want 2", got)
	}
	if bodies := fc.callBodies("deleteMessage"); len(bodies) != 1 || !strings.Contains(bodies[0], `"message_id":401`) {
		t.Fatalf("deleted %v, want previous log document 401", bodies)
	}
}

func TestSendScreenReplacesPrevious(t *testing.T) {
	b, _, fc := newFlowBot(t)
	uid := int64(791)
	fc.respSeq["sendMessage"] = []string{
		privateSentJSON(uid, 601),
		privateSentJSON(uid, 602),
	}
	ctx := context.Background()

	if _, err := b.sendScreen(ctx, uid, screenPrompt, "пришли текст", true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := b.sendScreen(ctx, uid, screenPrompt, "пришли текст", true, nil); err != nil {
		t.Fatal(err)
	}
	if bodies := fc.callBodies("deleteMessage"); len(bodies) != 1 || !strings.Contains(bodies[0], `"message_id":601`) {
		t.Fatalf("deleted %v, want previous prompt 601", bodies)
	}
}

// menuAICheckQuery — нажатие «🔌 Проверка ИИ» из главного меню владельца.
func menuAICheckQuery(uid int64, msgID int) telego.CallbackQuery {
	return telego.CallbackQuery{
		ID:   "q1",
		From: telego.User{ID: uid, FirstName: "Владелец"},
		Data: "menu:aicheck",
		Message: &telego.Message{
			MessageID: msgID,
			Chat:      telego.Chat{ID: uid, Type: "private"},
		},
	}
}

// TestAICheckDoubleTapNotModified — двойной тап по «Проверка ИИ»: второй
// вызов ловит 400 «message is not modified» (плейсхолдер уже нарисован) и НЕ
// должен слать новое сообщение-плейсхолдер: проверка первого вызова уже в
// полёте, стопка не растёт.
func TestAICheckDoubleTapNotModified(t *testing.T) {
	b, _, fc := newFlowBot(t)
	uid := int64(782)
	b.cfg.OwnerIDs = map[int64]struct{}{uid: {}}
	fc.err["editMessageText"] = &telegoapi.Error{
		ErrorCode:   400,
		Description: "Bad Request: message is not modified: specified new message content and reply markup are exactly the same as a current content and reply markup of the message",
	}

	if err := b.handleMenuCallback(nil, menuAICheckQuery(uid, 900)); err != nil {
		t.Fatal(err)
	}
	// Плейсхолдер уже стоит у другого вызова — второй check не запускаем:
	// выждим и убедимся, что редактирований по-прежнему одно.
	time.Sleep(100 * time.Millisecond)
	if got := fc.callCount("sendMessage"); got != 0 {
		t.Fatalf("sendMessage calls = %d, want 0 (not-modified must not spawn a new placeholder)", got)
	}
	if got := fc.callCount("editMessageText"); got != 1 {
		t.Fatalf("editMessageText calls = %d, want 1 (no duplicate check)", got)
	}
}

// TestAICheckEditsPressedMessage — успешный путь: плейсхолдер и итог живут в
// НАЖАТОМ сообщении меню, новых сообщений нет.
func TestAICheckEditsPressedMessage(t *testing.T) {
	b, _, fc := newFlowBot(t)
	uid := int64(783)
	b.cfg.OwnerIDs = map[int64]struct{}{uid: {}}
	fc.resp["editMessageText"] = privateSentJSON(uid, 900)

	if err := b.handleMenuCallback(nil, menuAICheckQuery(uid, 900)); err != nil {
		t.Fatal(err)
	}
	// runAICheck — горутина (goSafe): ждём итоговое редактирование
	// (первое — плейсхолдер, второе — результат).
	deadline := time.Now().Add(3 * time.Second)
	for fc.callCount("editMessageText") < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("editMessageText calls = %d, want 2 (placeholder + result)", fc.callCount("editMessageText"))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := fc.callCount("sendMessage"); got != 0 {
		t.Fatalf("sendMessage calls = %d, want 0 (result must edit the pressed message)", got)
	}
	bodies := fc.callBodies("editMessageText")
	if !strings.Contains(bodies[0], "Назад") {
		t.Fatalf("placeholder edit body %s, want back keyboard", bodies[0])
	}
	if !strings.Contains(bodies[1], "Проверка ИИ-провайдеров") || !strings.Contains(bodies[1], "Назад") {
		t.Fatalf("second edit body %s, want AI check result with back keyboard", bodies[1])
	}
}

// TestAICheckEditFailureFallsBackToScreen — нажатое сообщение отредактировать
// нельзя (старше 48ч/удалено): фолбек-плейсхолдер уходит экраном panel (с
// «Назад» и под замену следующей панелью), а итог проверки редактирует именно
// свежий плейсхолдер.
func TestAICheckEditFailureFallsBackToScreen(t *testing.T) {
	b, _, fc := newFlowBot(t)
	uid := int64(788)
	b.cfg.OwnerIDs = map[int64]struct{}{uid: {}}
	// Падает только редактирование «старого» 900; свежий 901 редактируется.
	fc.errWhen = func(method string, data *telegoapi.RequestData) bool {
		if method != "editMessageText" {
			return false
		}
		var q struct {
			MessageID int `json:"message_id"`
		}
		return json.Unmarshal(data.BodyRaw, &q) == nil && q.MessageID == 900
	}
	fc.resp["editMessageText"] = privateSentJSON(uid, 901)
	fc.resp["sendMessage"] = privateSentJSON(uid, 901)

	if err := b.handleMenuCallback(nil, menuAICheckQuery(uid, 900)); err != nil {
		t.Fatal(err)
	}
	// Провал по 900, затем итог проверки по 901 — ждём второй edit.
	deadline := time.Now().Add(3 * time.Second)
	for fc.callCount("editMessageText") < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("editMessageText calls = %d, want 2 (failed 900 + result on 901)", fc.callCount("editMessageText"))
		}
		time.Sleep(10 * time.Millisecond)
	}
	sent := fc.callBodies("sendMessage")
	if len(sent) != 1 || !strings.Contains(sent[0], "Назад") {
		t.Fatalf("fallback placeholder = %v, want one panel screen with «Назад»", sent)
	}
	edits := fc.callBodies("editMessageText")
	if !strings.Contains(edits[0], `"message_id":900`) || !strings.Contains(edits[1], `"message_id":901`) {
		t.Fatalf("edits = %v, want failed 900 then result on 901", edits)
	}
}

// stickerMsg — некомандное ЛС-сообщение с медиа вместо текста (стикер):
// попадает в пустую ветку флоу ввода текста (переспрос).
func stickerMsg(userID int64) telego.Message {
	return telego.Message{
		MessageID: 2,
		From:      &telego.User{ID: userID, FirstName: "Админ"},
		Chat:      telego.Chat{ID: userID, Type: "private"},
		Sticker:   &telego.Sticker{},
	}
}

// welcomeFlowBot — бот с обслуживаемым чатом и взведённым от владельца uid
// вводом текста приветствия: общая обвязка тестов переспроса.
func welcomeFlowBot(t *testing.T, uid int64) (*Bot, *storage.DB, *fakeCaller) {
	t.Helper()
	b, db, fc := newFlowBot(t)
	b.cfg.OwnerIDs = map[int64]struct{}{uid: {}}
	serviceableChat(t, b, db, testChatID)
	b.setGreetingInputPending(uid, testChatID, welcomeGreet)
	return b, db, fc
}

// TestWelcomeRePromptKeepsContext — переспрос во флоу ввода текста: это ПОЛНЫЙ
// промпт с вводной строкой (контекст {name}/«текущий текст»/лимит не теряется),
// и флоу остаётся взведённым.
func TestWelcomeRePromptKeepsContext(t *testing.T) {
	uid := int64(785)
	b, db, fc := welcomeFlowBot(t, uid)
	text := "Привет, {name}!"
	if err := db.SetGreetingText(context.Background(), testChatID, &text, nil); err != nil {
		t.Fatal(err)
	}

	if err := b.handlePrivateText(nil, stickerMsg(uid)); err != nil {
		t.Fatal(err)
	}
	bodies := fc.callBodies("sendMessage")
	if len(bodies) != 1 {
		t.Fatalf("sendMessage calls = %d, want 1", len(bodies))
	}
	for _, want := range []string{"Нужно именно текстовое сообщение", "✏️ Пришли мне текст приветствия", "Текущий текст", "Привет, {name}!"} {
		if !strings.Contains(bodies[0], want) {
			t.Fatalf("re-prompt body misses %q: %s", want, bodies[0])
		}
	}
	if _, _, ok, _ := b.takeGreetingInput(uid); !ok {
		t.Fatal("flow must stay armed after a delivered re-prompt")
	}
}

// TestWelcomeRePromptFailedSendDoesNotRearm — упавшая отправка переспроса НЕ
// перевзводит ввод: иначе следующее обычное ЛС админа молча стало бы шаблоном.
func TestWelcomeRePromptFailedSendDoesNotRearm(t *testing.T) {
	uid := int64(786)
	b, _, fc := welcomeFlowBot(t, uid)
	fc.err["sendMessage"] = &telegoapi.Error{ErrorCode: 403, Description: "Forbidden: bot was blocked by the user"}

	if err := b.handlePrivateText(nil, stickerMsg(uid)); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := b.takeGreetingInput(uid); ok {
		t.Fatal("failed re-prompt must not leave the input armed")
	}
}

// TestWelcomeTooLongRePrompt — слишком длинный текст не сохраняется, приходит
// полный переспрос, флоу остаётся взведённым.
func TestWelcomeTooLongRePrompt(t *testing.T) {
	uid := int64(787)
	b, db, fc := welcomeFlowBot(t, uid)

	if err := b.handlePrivateText(nil, privateMsg(uid, strings.Repeat("я", maxGreetingRunes+1))); err != nil {
		t.Fatal(err)
	}
	bodies := fc.callBodies("sendMessage")
	if len(bodies) != 1 ||
		!strings.Contains(bodies[0], "Слишком длинно") ||
		!strings.Contains(bodies[0], "Пришли мне текст приветствия") {
		t.Fatalf("bodies = %v, want a too-long re-prompt with the full prompt", bodies)
	}
	s, err := db.GetChatSettings(context.Background(), testChatID)
	if err != nil {
		t.Fatal(err)
	}
	if s.GreetingText.Valid {
		t.Fatal("too-long text must not be saved")
	}
	if _, _, ok, _ := b.takeGreetingInput(uid); !ok {
		t.Fatal("flow must stay armed after a delivered re-prompt")
	}
}

// TestAccessRefusalsReplaceThemselves — отбивки-отказы (/info не-владельцу)
// живут экраном screenNotice: повторный отказ заменяет прошлый, а не копится.
func TestAccessRefusalsReplaceThemselves(t *testing.T) {
	b, _, fc := newFlowBot(t)
	uid := int64(784) // не владелец
	fc.respSeq["sendMessage"] = []string{
		privateSentJSON(uid, 501),
		privateSentJSON(uid, 502),
	}

	if err := b.handlePrivateInfoCommand(nil, privateMsg(uid, "/info")); err != nil {
		t.Fatal(err)
	}
	if err := b.handlePrivateInfoCommand(nil, privateMsg(uid, "/info")); err != nil {
		t.Fatal(err)
	}
	if bodies := fc.callBodies("deleteMessage"); len(bodies) != 1 || !strings.Contains(bodies[0], `"message_id":501`) {
		t.Fatalf("deleted %v, want previous refusal 501", bodies)
	}
}
