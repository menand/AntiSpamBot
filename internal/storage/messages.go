package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// StoredMessage — одна сохранённая запись сообщения для расширенного сбора
// статистики. Самодостаточна: цитата денормализована в reply_*, чтобы архив
// читался без joins (важно для ЛЛМ-разбора).
//
// Отдельно от users.MessageRecord, который описывает «тишину» для детектора
// возвращенцев и не имеет отношения к текстовому следу.
type StoredMessage struct {
	// ID — autoincrement-идентификатор строки (не Telegram-id). Нужен только
	// архиватору, чтобы удалять прочитанное по границе id <= maxID: строка,
	// вставленная между чтением дня и удалением, переживёт этот прогон.
	ID          int64
	ChatID      int64
	UserID      int64
	TGMsgID     int64 // id сообщения в Telegram (связь оригинал↔правка)
	TS          time.Time
	Kind        string // 'text' | 'caption' | 'media' | 'edit'
	Text        string // текст или подпись; "" = медиа без подписи
	HasMedia    bool   // было ли вложение (само вложение не храним, только факт)
	ReplyMsgID  *int64
	ReplyUID    *int64
	ReplyAuthor *string
	ReplyText   *string
}

// AppendMessages пачками пишет сообщения одной транзакцией. Пустой срез —
// no-op. Вызывается фоновой writer-горутиной, а не с горячего пути хендлера.
func (d *DB) AppendMessages(ctx context.Context, msgs []StoredMessage) error {
	if len(msgs) == 0 {
		return nil
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO messages (chat_id, user_id, tg_msg_id, ts, day, kind, text, has_media,
			reply_msg_id, reply_uid, reply_author, reply_text, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare append messages: %w", err)
	}
	defer stmt.Close() //nolint:errcheck
	now := time.Now().Unix()
	for _, m := range msgs {
		var text any
		if m.Text != "" {
			text = m.Text
		}
		if _, err := stmt.ExecContext(ctx,
			m.ChatID, m.UserID, m.TGMsgID, m.TS.Unix(), DayOf(m.TS), m.Kind,
			text, boolInt(m.HasMedia),
			m.ReplyMsgID, m.ReplyUID, m.ReplyAuthor, m.ReplyText, now,
		); err != nil {
			return fmt.Errorf("insert message: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit append messages: %w", err)
	}
	return nil
}

// MessageFilter — параметры выборки для msgtool (hot-окно в SQLite).
// Нулевые значения = «не фильтровать». Word ищется подстрокой в тексте/цитате.
type MessageFilter struct {
	ChatID int64
	UserID int64
	Word   string
	From   time.Time // ноль = без нижней границы
	Until  time.Time // ноль = без верхней
	Limit  int       // 0 = без лимита
}

// QueryMessages ищет «горячие» сообщения (последние дни, ещё не в архиве)
// по фильтру и возвращает их в хронологическом порядке. Используется msgtool
// для быстрых запросов по свежему окну; старое ищется стримингом по архивам.
func (d *DB) QueryMessages(ctx context.Context, f MessageFilter) ([]StoredMessage, error) {
	q := `SELECT id, chat_id, user_id, tg_msg_id, ts, kind, text, has_media,
			reply_msg_id, reply_uid, reply_author, reply_text
		FROM messages WHERE 1=1`
	args := make([]any, 0, 6)
	if f.ChatID != 0 {
		q += ` AND chat_id = ?`
		args = append(args, f.ChatID)
	}
	if f.UserID != 0 {
		q += ` AND user_id = ?`
		args = append(args, f.UserID)
	}
	if !f.From.IsZero() {
		// Фильтр по МСК-дню (storage.DayOf), а не по raw ts: границы должны
		// совпадать с архивной стороной (она режет по имени дня). Иначе
		// сообщения 00:00–03:00 МСК пограничного дня выпадали бы из hot, но
		// присутствовали бы в архиве того же дня.
		q += ` AND day >= ?`
		args = append(args, DayOf(f.From))
	}
	if !f.Until.IsZero() {
		q += ` AND day < ?`
		args = append(args, DayOf(f.Until))
	}
	if f.Word != "" {
		// instr() — литеральный регистрозависимый поиск подстроки, бинарно
		// совпадающий со strings.Contains на стороне архива. LIKE дал бы другой
		// результат через границу архива (% и _ — wildcard, ASCII —
		// регистронезависимы), и один -word вернул бы разные наборы.
		q += ` AND (instr(text, ?) > 0 OR instr(reply_text, ?) > 0)`
		args = append(args, f.Word, f.Word)
	}
	q += ` ORDER BY ts, id`
	if f.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, f.Limit)
	}
	rows, err := d.sql.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	defer rows.Close() //nolint:errcheck // cleanup, error intentionally ignored
	return scanStoredMessages(rows)
}

// UnarchivedDaysBefore возвращает отсортированные дни ('YYYY-MM-DD'), в которых
// есть сообщения старше before и ещё не вынесенные в архив. Именно по этим дням
// прогоняется архиватор; каждая единица — день, чтобы результат был идемпотентен.
func (d *DB) UnarchivedDaysBefore(ctx context.Context, before time.Time) ([]string, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT DISTINCT day FROM messages
		WHERE day < ?
		ORDER BY day
	`, DayOf(before))
	if err != nil {
		return nil, fmt.Errorf("query unarchived days: %w", err)
	}
	defer rows.Close() //nolint:errcheck // cleanup, error intentionally ignored
	var out []string
	for rows.Next() {
		var day string
		if err := rows.Scan(&day); err != nil {
			return nil, fmt.Errorf("scan unarchived day: %w", err)
		}
		out = append(out, day)
	}
	return out, rows.Err()
}

// MessagesForDay грузит все сообщения дня, в порядке (ts, id) — стабильном
// воспроизведении хронологии для архива.
func (d *DB) MessagesForDay(ctx context.Context, day string) ([]StoredMessage, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT id, chat_id, user_id, tg_msg_id, ts, kind, text, has_media,
			reply_msg_id, reply_uid, reply_author, reply_text
		FROM messages
		WHERE day = ?
		ORDER BY ts, id
	`, day)
	if err != nil {
		return nil, fmt.Errorf("query messages for day: %w", err)
	}
	defer rows.Close() //nolint:errcheck // cleanup, error intentionally ignored
	return scanStoredMessages(rows)
}

// scanStoredMessages сводит строки выборки сообщений в []StoredMessage.
// Общий хелпер для QueryMessages и MessagesForDay (одинаковый набор колонок).
func scanStoredMessages(rows *sql.Rows) ([]StoredMessage, error) {
	var out []StoredMessage
	for rows.Next() {
		var m StoredMessage
		var ts int64
		var hasMedia int
		var text, replyAuthor, replyText sql.NullString
		var replyMsgID, replyUID sql.NullInt64
		if err := rows.Scan(&m.ID, &m.ChatID, &m.UserID, &m.TGMsgID, &ts, &m.Kind, &text,
			&hasMedia, &replyMsgID, &replyUID, &replyAuthor, &replyText); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		m.TS = time.Unix(ts, 0)
		m.Text = text.String
		m.HasMedia = hasMedia != 0
		if replyMsgID.Valid {
			v := replyMsgID.Int64
			m.ReplyMsgID = &v
		}
		if replyUID.Valid {
			v := replyUID.Int64
			m.ReplyUID = &v
		}
		if replyAuthor.Valid && replyAuthor.String != "" {
			v := replyAuthor.String
			m.ReplyAuthor = &v
		}
		if replyText.Valid && replyText.String != "" {
			v := replyText.String
			m.ReplyText = &v
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteDayThrough удаляет hot-строки дня с id <= throughID. Вызывается только
// ПОСЛЕ успешной записи дня в архив. Граница по id критична: строка, вставленная
// архиватором ПОСЛЕ того, как MessagesForDay прочитал день (поздняя правка /
// реплей), имеет id > throughID и переживает этот прогон — её допишет следующий
// тик. Без границы такую строку удалили бы, не попав в файл (потеря данных).
// Идемпотентен (повторный вызов для уже удалённого дня — no-op).
func (d *DB) DeleteDayThrough(ctx context.Context, day string, throughID int64) error {
	if _, err := d.sql.ExecContext(ctx,
		`DELETE FROM messages WHERE day = ? AND id <= ?`, day, throughID); err != nil {
		return fmt.Errorf("delete day %s through id %d: %w", day, throughID, err)
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
