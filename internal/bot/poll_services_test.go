package bot

import (
	"encoding/json"
	"testing"

	"github.com/mymmrac/telego"
)

// Фикстуры — реальная форма поломки из прода: сообщения с poll_option_added /
// poll_option_deleted, где poll_message — объект. telego v1.11.1 объявлял его
// как интерфейс MaybeInaccessibleMessage и не мог декодировать — один такой
// апдейт ронял весь getUpdates. С v1.11.2 у PollOptionAdded/PollOptionDeleted
// появился штатный UnmarshalJSON, и обходной санитайзер (updatesanitize.go)
// удалён. Эти тесты держат фикстуры актуальными и проверяют, что под нашим
// сбором (-tags stdjson) сервисные маркеры по-прежнему декодируются.
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
// сообщение должно декодироваться как раньше.
const pollAddedNoPollMessageJSON = `[{
	"update_id": 104,
	"message": {
		"message_id": 10,
		"date": 1724000003,
		"chat": {"id": -1001234567890, "type": "supergroup", "title": "Чат"},
		"poll_option_added": {"option_persistent_id": "opt-4", "option_text": "y"}
	}
}]`

// pollDeletedEditedJSON — те же сервисные поля в edited_message.
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

// pollPinnedMessageJSON — другое интерфейсное MaybeInaccessibleMessage-поле
// (pinned_message): должно продолжать декодироваться штатно.
const pollPinnedMessageJSON = `[{
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

func decodeUpdates(t *testing.T, raw string) []telego.Update {
	t.Helper()
	var ups []telego.Update
	if err := json.Unmarshal([]byte(raw), &ups); err != nil {
		t.Fatalf("декод батча обновлений: %v", err)
	}
	if len(ups) == 0 {
		t.Fatal("батч обновлений пуст")
	}
	return ups
}

// TestPollOptionAddedDecodes — getUpdates-батч с poll_option_added и полным
// poll_message обязан декодироваться целиком (маркер + вложенное сообщение).
func TestPollOptionAddedDecodes(t *testing.T) {
	ups := decodeUpdates(t, pollAddedJSON)
	m := ups[0].Message
	if m == nil {
		t.Fatal("message отсутствует")
	}
	if m.PollOptionAdded == nil {
		t.Fatal("маркер poll_option_added должен сохраниться")
	}
	p := m.PollOptionAdded
	// Конкретный тип + непустой MessageID: goccy-бэкенд (голый go test без
	// -tags stdjson) на старом telego клал бы в PollMessage typed-nil *Message,
	// который IsAccessible() == true без разыменования — только проверки на
	// доступность тест прошёл бы ложно.
	pm, ok := p.PollMessage.(*telego.Message)
	if !ok || pm.GetMessageID() != 6 {
		t.Fatalf("poll_message должен декодироваться в *telego.Message с message_id 6, got %+v", p.PollMessage)
	}
	if p.OptionText != "new option" {
		t.Fatalf("option_text = %q, want %q", p.OptionText, "new option")
	}
	if m.MessageID != 7 || m.Chat.ID != -1001234567890 || m.From == nil || m.From.ID != 42 {
		t.Fatalf("остальное сообщение должно быть нетронутым, got %+v", m)
	}
}

func TestPollOptionDeletedDecodes(t *testing.T) {
	ups := decodeUpdates(t, pollDeletedJSON)
	p := ups[0].Message.PollOptionDeleted
	if p == nil {
		t.Fatal("маркер poll_option_deleted должен сохраниться")
	}
	pm, ok := p.PollMessage.(*telego.Message)
	if !ok || pm.GetMessageID() != 6 {
		t.Fatalf("poll_message должен декодироваться в *telego.Message с message_id 6, got %+v", p.PollMessage)
	}
}

func TestPollOptionAddedInaccessibleDecodes(t *testing.T) {
	ups := decodeUpdates(t, pollAddedInaccessibleJSON)
	p := ups[0].Message.PollOptionAdded
	if p == nil {
		t.Fatal("маркер poll_option_added должен сохраниться")
	}
	pm, ok := p.PollMessage.(*telego.InaccessibleMessage)
	if !ok || pm.GetMessageID() != 6 {
		t.Fatalf("poll_message с date=0 должен декодироваться в *telego.InaccessibleMessage с message_id 6, got %+v", p.PollMessage)
	}
}

func TestPollOptionAddedWithoutPollMessage(t *testing.T) {
	ups := decodeUpdates(t, pollAddedNoPollMessageJSON)
	p := ups[0].Message.PollOptionAdded
	if p == nil || p.OptionText != "y" {
		t.Fatalf("poll_option_added без poll_message должен сохраниться целиком, got %+v", p)
	}
}

func TestPollOptionDeletedEditedDecodes(t *testing.T) {
	ups := decodeUpdates(t, pollDeletedEditedJSON)
	p := ups[0].EditedMessage
	if p == nil || p.PollOptionDeleted == nil {
		t.Fatalf("маркер poll_option_deleted в edited_message должен сохраниться, got %+v", p)
	}
	if pm, ok := p.PollOptionDeleted.PollMessage.(*telego.Message); !ok || pm.GetMessageID() != 6 {
		t.Fatalf("poll_message в edited_message должен декодироваться в *telego.Message с message_id 6, got %+v", p.PollOptionDeleted.PollMessage)
	}
}

func TestPinnedMessageStillDecodes(t *testing.T) {
	ups := decodeUpdates(t, pollPinnedMessageJSON)
	m := ups[0].Message
	if m == nil || m.PinnedMessage == nil || !m.PinnedMessage.IsAccessible() {
		t.Fatalf("pinned_message должен декодироваться, got %+v", m)
	}
	if m.PinnedMessage.GetMessageID() != 4 {
		t.Fatalf("pinned_message.message_id = %d, want 4", m.PinnedMessage.GetMessageID())
	}
}
