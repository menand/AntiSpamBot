package bot

import (
	"bytes"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/mymmrac/telego"
)

// pollAddedJSON — реальная форма поломки из прода: message с poll_option_added,
// где poll_message — объект (интерфейс MaybeInaccessibleMessage, который
// telego v1.11.1 не умеет декодировать). TestPollServicePayloadFailsWithoutSanitizer
// держит фикстуру актуальной: если Telegram/telego поменяют формат и фикстура
// начнёт декодироваться сама по себе, тест упадёт и напомнит снять санитайзер.
const pollAddedJSON = `[{
	"update_id": 100,
	"message": {
		"message_id": 7,
		"date": 1724000000,
		"chat": {"id": -1001234567890, "type": "supergroup", "title": "Чат"},
		"from": {"id": 42, "is_bot": false, "first_name": "U"},
		"poll_option_added": {
			"poll_message": {
				"message_id": 6,
				"date": 1723999999,
				"chat": {"id": -1001234567890, "type": "supergroup"},
				"poll": {"id": "poll-1", "question": "q", "options": [{"text": "a", "voter_count": 1}]}
			},
			"option_persistent_id": "opt-1",
			"option_text": "new option"
		}
	}
}]`

const pollDeletedJSON = `[{
	"update_id": 101,
	"message": {
		"message_id": 8,
		"date": 1724000001,
		"chat": {"id": -1001234567890, "type": "supergroup", "title": "Чат"},
		"poll_option_deleted": {
			"poll_message": {"message_id": 6, "date": 1723999999, "chat": {"id": -1001234567890, "type": "supergroup"}},
			"option_persistent_id": "opt-2",
			"option_text": "old option"
		}
	}
}]`

// pollAddedInaccessibleJSON — poll_message с date=0: такой приходит, когда
// сообщение с опросом уже недоступно боту (InaccessibleMessage).
const pollAddedInaccessibleJSON = `[{
	"update_id": 102,
	"message": {
		"message_id": 9,
		"date": 1724000002,
		"chat": {"id": -1001234567890, "type": "supergroup", "title": "Чат"},
		"poll_option_added": {
			"poll_message": {"message_id": 6, "date": 0, "chat": {"id": -1001234567890, "type": "supergroup"}},
			"option_persistent_id": "opt-3",
			"option_text": "x"
		}
	}
}]`

// pollAddedNoPollMessageJSON — poll_message опциональный (в доках): без него
// сообщение декодируется и так, санитайзер не должен его ломать.
const pollAddedNoPollMessageJSON = `[{
	"update_id": 104,
	"message": {
		"message_id": 10,
		"date": 1724000003,
		"chat": {"id": -1001234567890, "type": "supergroup", "title": "Чат"},
		"poll_option_added": {"option_persistent_id": "opt-4", "option_text": "y"}
	}
}]`

// pollDeletedEditedJSON — те же сервисные поля, но в edited_message: рекурсивная
// вырезка должна работать на любой глубине/позиции.
const pollDeletedEditedJSON = `[{
	"update_id": 103,
	"edited_message": {
		"message_id": 8,
		"date": 1724000001,
		"chat": {"id": -1001234567890, "type": "supergroup", "title": "Чат"},
		"poll_option_deleted": {
			"poll_message": {"message_id": 6, "date": 1723999999, "chat": {"id": -1001234567890, "type": "supergroup"}},
			"option_persistent_id": "opt-2",
			"option_text": "old option"
		}
	}
}]`

// pinnedMessageJSON — другой интерфейсный MaybeInaccessibleMessage-поле
// (pinned_message): санитайзер не должен трогать то, что telego и так умеет.
const pinnedMessageJSON = `[{
	"update_id": 200,
	"message": {
		"message_id": 5,
		"date": 1724000000,
		"chat": {"id": -100, "type": "supergroup", "title": "t"},
		"pinned_message": {
			"message_id": 4,
			"date": 1723990000,
			"chat": {"id": -100, "type": "supergroup"},
			"text": "пин"
		}
	}
}]`

const plainMessageJSON = `[{
	"update_id": 300,
	"message": {
		"message_id": 8,
		"date": 1724000000,
		"chat": {"id": -100, "type": "supergroup", "title": "t"},
		"from": {"id": 1, "is_bot": false, "first_name": "A"},
		"text": "hello",
		"entities": [{"offset": 0, "length": 5, "type": "bold"}]
	}
}]`

// TestPollServiceFixtureHasPollMessage — фикстура обязана содержать то самое
// проблемное поле poll_message, ради которого санитайзер существует. «Канарейка»
// в форме «фикстура не декодируется без санитайзера» нежизнеспособна: init()
// ставит санитайзер глобально, и даже прямой stdlib-декод идёт через
// Message.UnmarshalJSON, который зовёт уже-подменённый внутренний json. Поэтому
// проверяем по сырым байтам: поле есть и вырезается.
func TestPollServiceFixtureHasPollMessage(t *testing.T) {
	if !bytes.Contains([]byte(pollAddedJSON), []byte(`"poll_message"`)) {
		t.Fatal("фикстура должна содержать poll_message — иначе санитайзеру нечего вырезать, проверь актуальность фикстуры")
	}
	clean, err := stripPollServiceMessages([]byte(pollAddedJSON))
	if err != nil {
		t.Fatalf("stripPollServiceMessages: %v", err)
	}
	if bytes.Contains(clean, []byte(`"poll_message"`)) {
		t.Fatal("stripPollServiceMessages не вырезал poll_message")
	}
}

// TestPollMessageInterfaceStillBroken — сильная канарейка: пока телеговский
// poll_message объявлен как непустой интерфейс MaybeInaccessibleMessage,
// stdlib-декод объекта в него обязан падать. Тест не зависит от глобальной
// подмены json (анонимная структура, не telego.Message — кастомный
// UnmarshalJSON не вызывается). Если апстрим починит тип/декол, тест упадёт и
// напомнит снять санитайзер.
func TestPollMessageInterfaceStillBroken(t *testing.T) {
	var v struct {
		PollMessage telego.MaybeInaccessibleMessage `json:"poll_message"`
	}
	if err := json.Unmarshal([]byte(`{"poll_message":{"message_id":6,"date":0,"chat":{"id":-1,"type":"supergroup"}}}`), &v); err == nil {
		t.Fatal("объект в poll_message обязан не декодироваться в MaybeInaccessibleMessage (иначе санитайзер больше не нужен)")
	}
}

func sanitizeUpdates(t *testing.T, raw string) []telego.Update {
	t.Helper()
	var ups []telego.Update
	if err := sanitizeAndUnmarshal([]byte(raw), &ups); err != nil {
		t.Fatalf("sanitizeAndUnmarshal: %v", err)
	}
	return ups
}

// TestSanitizeRealTelegoPath — имитация реального пути telego: performRequest
// кладёт &updates в ...any и зовёт json.Unmarshal(result, &vs[i]), т.е. таргет
// приходит как *interface{}, а не *[]telego.Update. Диспетчеризация по типу
// этот случай упускала — см. updatesanitize.go.
func TestSanitizeRealTelegoPath(t *testing.T) {
	var ups []telego.Update
	var v any = &ups
	if err := sanitizeAndUnmarshal([]byte(pollAddedJSON), &v); err != nil {
		t.Fatalf("реальный путь (таргет *interface{}): %v", err)
	}
	m := ups[0].Message
	if m == nil || m.PollOptionAdded == nil {
		t.Fatalf("маркер poll_option_added должен сохраниться, got %+v", m)
	}
	if m.PollOptionAdded.PollMessage != nil {
		t.Fatal("poll_message должен быть вырезан")
	}
}

func TestSanitizePollOptionAdded(t *testing.T) {
	ups := sanitizeUpdates(t, pollAddedJSON)
	m := ups[0].Message
	if m == nil {
		t.Fatal("message отсутствует")
	}
	if m.PollOptionAdded == nil {
		t.Fatal("маркер poll_option_added должен сохраниться")
	}
	if m.PollOptionAdded.PollMessage != nil {
		t.Fatal("poll_message должен быть вырезан")
	}
	if m.MessageID != 7 {
		t.Fatalf("message_id = %d, want 7 (остальное сообщение должно быть нетронутым)", m.MessageID)
	}
	if m.Chat.ID != -1001234567890 {
		t.Fatalf("chat.id = %d, want -1001234567890", m.Chat.ID)
	}
	if m.From == nil || m.From.ID != 42 {
		t.Fatalf("from должен сохраниться, got %+v", m.From)
	}
}

func TestSanitizePollOptionDeleted(t *testing.T) {
	ups := sanitizeUpdates(t, pollDeletedJSON)
	p := ups[0].Message.PollOptionDeleted
	if p == nil {
		t.Fatal("маркер poll_option_deleted должен сохраниться")
	}
	if p.PollMessage != nil {
		t.Fatal("poll_message должен быть вырезан")
	}
}

func TestSanitizeInaccessiblePollMessage(t *testing.T) {
	ups := sanitizeUpdates(t, pollAddedInaccessibleJSON)
	p := ups[0].Message.PollOptionAdded
	if p == nil {
		t.Fatal("маркер poll_option_added должен сохраниться")
	}
	if p.PollMessage != nil {
		t.Fatal("poll_message (даже недоступный) должен быть вырезан")
	}
}

func TestSanitizeNoPollMessageField(t *testing.T) {
	ups := sanitizeUpdates(t, pollAddedNoPollMessageJSON)
	p := ups[0].Message.PollOptionAdded
	if p == nil || p.OptionText != "y" {
		t.Fatalf("poll_option_added без poll_message должен сохраниться целиком, got %+v", p)
	}
}

func TestSanitizeEditedMessage(t *testing.T) {
	ups := sanitizeUpdates(t, pollDeletedEditedJSON)
	p := ups[0].EditedMessage
	if p == nil || p.PollOptionDeleted == nil {
		t.Fatalf("маркер poll_option_deleted в edited_message должен сохраниться, got %+v", p)
	}
	if p.PollOptionDeleted.PollMessage != nil {
		t.Fatal("poll_message в edited_message должен быть вырезан")
	}
}

func TestSanitizeKeepsPinnedMessage(t *testing.T) {
	ups := sanitizeUpdates(t, pinnedMessageJSON)
	m := ups[0].Message
	if m == nil || m.PinnedMessage == nil {
		t.Fatalf("pinned_message должен сохраниться, got %+v", m)
	}
	if !m.PinnedMessage.IsAccessible() {
		t.Fatal("pinned_message должен быть доступным (date != 0)")
	}
	if m.PinnedMessage.GetMessageID() != 4 {
		t.Fatalf("pinned_message.message_id = %d, want 4", m.PinnedMessage.GetMessageID())
	}
}

func TestSanitizePlainMessage(t *testing.T) {
	ups := sanitizeUpdates(t, plainMessageJSON)
	m := ups[0].Message
	if m == nil || m.Text != "hello" {
		t.Fatalf("обычное сообщение должно декодироваться как раньше, got %+v", m)
	}
	if len(m.Entities) != 1 || m.Entities[0].Type != "bold" {
		t.Fatalf("entities должны сохраниться, got %+v", m.Entities)
	}
}

// TestSanitizeFastPathViaInterface — обычный батч (без poll-ключей) через
// реальную форму таргета performRequest (*interface{}, держащий *[]Update)
// должен уходить в fast-path и декодироваться без перекодирования.
func TestSanitizeFastPathViaInterface(t *testing.T) {
	var ups []telego.Update
	var v any = &ups
	if err := sanitizeAndUnmarshal([]byte(plainMessageJSON), &v); err != nil {
		t.Fatalf("fast-path через *interface{}: %v", err)
	}
	if ups[0].Message == nil || ups[0].Message.Text != "hello" {
		t.Fatalf("обычный батч должен декодироваться, got %+v", ups[0].Message)
	}
}

// TestSanitizePreservesIntPrecision — round-trip через UseNumber не должен
// портить 64-битные ID (обычный map-round-trip через float64 теряет точность
// за 2^53).
func TestSanitizePreservesIntPrecision(t *testing.T) {
	const bigID int64 = 9007199254740993 // 2^53 + 1
	raw := `[{
		"update_id": 999999999999999999,
		"message": {
			"message_id": 1,
			"date": 1724000000,
			"chat": {"id": ` + strconv.FormatInt(bigID, 10) + `, "type": "supergroup", "title": "t"},
			"poll_option_added": {
				"poll_message": {"message_id": 6, "date": 1723999999, "chat": {"id": ` + strconv.FormatInt(bigID, 10) + `, "type": "supergroup"}},
				"option_persistent_id": "opt-1",
				"option_text": "x"
			}
		}
	}]`
	ups := sanitizeUpdates(t, raw)
	if ups[0].UpdateID != 999999999999999999 {
		t.Fatalf("update_id = %d, want 999999999999999999", ups[0].UpdateID)
	}
	if m := ups[0].Message; m == nil || m.Chat.ID != bigID {
		t.Fatalf("chat.id = %v, want %d", m.Chat.ID, bigID)
	}
}

// TestSanitizeNonUpdatePassthrough — не-Updates-таргеты уходят в stdlib без
// изменений (санитайзер не должен трогать остальные API-декоды).
func TestSanitizeNonUpdatePassthrough(t *testing.T) {
	var u telego.User
	if err := sanitizeAndUnmarshal([]byte(`{"id":1,"is_bot":true,"first_name":"B","username":"x"}`), &u); err != nil {
		t.Fatalf("passthrough: %v", err)
	}
	if u.ID != 1 || u.Username != "x" {
		t.Fatalf("user = %+v, want id=1 username=x", u)
	}
}
