package bot

import (
	"bytes"
	"encoding/json"

	"github.com/mymmrac/telego"
)

// Telegram шлёт служебные сообщения poll_option_added / poll_option_deleted,
// когда в опросе добавляют или удаляют вариант. telego v1.11.1 объявляет их
// поле poll_message как интерфейс MaybeInaccessibleMessage, в который
// JSON-объект не раскладывается ни stdlib-, ни grbit-бэкендом (у Message и
// CallbackQuery для этого есть кастомный UnmarshalJSON, у этих типов — нет;
// фикса в апстриме нет даже в main). Один такой апдейт роняет весь getUpdates:
// батч не декодируется → offset не продвигается → Telegram вечно отдаёт тот же
// апдейт → бот не обрабатывает ничего (цикл ошибок каждые 8с, рестарт не
// помогает — апдейт остаётся в очереди).
//
// Санитайзер вырезает из JSON поле poll_message внутри этих двух сервисных
// объектов до декодера: само поле боту не нужно, а маркер poll_option_added /
// poll_option_deleted оставляем — по нему handleGroupMessage отсекает
// сервис-сообщение от статистики и спам-чека. Ставится через
// telego.SetJSONUnmarshal — глобально на весь telego, поэтому через init():
// установка случается до любых декодов и race-free (в отличие от вызова из
// New(), который гонялся бы с параллельными тестами). Функция чистая, без
// состояния — переустановка безвредна.
func init() {
	telego.SetJSONUnmarshal(sanitizeAndUnmarshal)
}

// pollServiceKeys — ключи сервис-сообщений опросов, из которых вырезаем
// poll_message. Пакетная переменная, чтобы не аллоцировать слайс на каждый map
// в рекурсии.
var pollServiceKeys = []string{"poll_option_added", "poll_option_deleted"}

func sanitizeAndUnmarshal(data []byte, v any) error {
	// telego зовёт внутренний json.Unmarshal из performRequest как
	// json.Unmarshal(result, &vs[i]) — таргет приходит как *interface{}, а не
	// *[]telego.Update, поэтому диспетчеризация по типу не сработала бы. Ловим
	// по содержимому: ключи poll_option_added / poll_option_deleted в JSON
	// присутствуют литерально всегда, когда есть такие сервис-сообщения, и
	// нигде больше не встречаются как ключи (текст юзера — строка-значение,
	// вырезка до неё не дотягивается, а зря прошедший через неё батч безвреден:
	// round-trip семантически идемпотентен).
	//
	// Побочный эффект: в сборках без -tags stdjson (голый `go run`/`go test`)
	// подмена делает декод ВСЕХ API-ответов stdlib'ом, а не grbit — из-за
	// быстрого пути ниже. Функционально безопасно (stdlib — референсный
	// декодер), разница dev/prod сводится к одному marshal.
	if !bytes.Contains(data, []byte(`"poll_option_added"`)) &&
		!bytes.Contains(data, []byte(`"poll_option_deleted"`)) {
		// Fast-path: обычные ответы API декодируются как раньше.
		return json.Unmarshal(data, v)
	}
	clean, err := stripPollServiceMessages(data)
	if err != nil {
		return err
	}
	return json.Unmarshal(clean, v)
}

func stripPollServiceMessages(data []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber() // 64-битные ID (чаты, юзеры, апдейты) должны пройти round-trip без потери точности
	var root any
	if err := dec.Decode(&root); err != nil {
		return nil, err
	}
	root = stripPollMessage(root)
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(root); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func stripPollMessage(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for _, key := range pollServiceKeys {
			if inner, ok := t[key].(map[string]any); ok {
				delete(inner, "poll_message")
			}
		}
		for k, child := range t {
			t[k] = stripPollMessage(child)
		}
		return t
	case []any:
		for i, child := range t {
			t[i] = stripPollMessage(child)
		}
		return t
	default:
		return v
	}
}
