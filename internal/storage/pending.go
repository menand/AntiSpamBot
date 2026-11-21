package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrAdmissionPending = errors.New("admission already pending")

type PendingRow struct {
	Generation    string
	RestrictUntil int64 // абсолютный until_date; 0 у прежних бессрочных ограничений
	Suspended     bool
	ChatID        int64
	UserID        int64
	MessageID     int
	CorrectIdx    int
	ExpiresAt     time.Time
	ThreadID      int // топик форума, в котором вошёл юзер; 0 = без топика
	EphemeralID   int // ≠0: капча эфемерная, удалять по этому id
	Stage         int // стадия серии капчи (1..3); рестарт продолжает с неё
}

func (p PendingRow) Preparing() bool {
	return p.Generation != "" && p.MessageID == 0 && p.EphemeralID == 0
}

// BeginCaptchaPreparation сохраняет намерение и точный until_date до Telegram.
func (d *DB) BeginCaptchaPreparation(ctx context.Context, p PendingRow, joinedAt time.Time) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	res, err := tx.ExecContext(ctx, `
		INSERT INTO pending_captchas (chat_id, user_id, message_id, correct_idx, expires_at, thread_id, generation, restrict_until)
		SELECT ?, ?, 0, 0, ?, ?, ?, ? WHERE NOT EXISTS (
			SELECT 1 FROM pending_admissions WHERE chat_id = ? AND user_id = ?)
		ON CONFLICT(chat_id, user_id) DO UPDATE SET message_id = 0, ephemeral_msg_id = 0,
			correct_idx = 0, stage = 1, suspended = 0, expires_at = excluded.expires_at,
			thread_id = excluded.thread_id, generation = excluded.generation, restrict_until = excluded.restrict_until
	`, p.ChatID, p.UserID, p.ExpiresAt.Unix(), p.ThreadID, p.Generation, p.RestrictUntil, p.ChatID, p.UserID)
	if err := pendingCaptchaWritten(res, err); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events (chat_id, user_id, kind, at) VALUES (?, ?, ?, ?)`,
		p.ChatID, p.UserID, EventJoin, joinedAt.Unix()); err != nil {
		return fmt.Errorf("insert join for captcha preparation: %w", err)
	}
	return tx.Commit()
}

// ActivatePreparedCaptcha подтверждает отправку только для того же намерения.
func (d *DB) ActivatePreparedCaptcha(ctx context.Context, p PendingRow) (bool, error) {
	result, err := d.sql.ExecContext(ctx, `
		UPDATE pending_captchas SET message_id = ?, ephemeral_msg_id = ?, correct_idx = ?, expires_at = ?
		WHERE chat_id = ? AND user_id = ? AND generation = ? AND message_id = 0 AND ephemeral_msg_id = 0 AND suspended = 0
	`, p.MessageID, p.EphemeralID, p.CorrectIdx, p.ExpiresAt.Unix(), p.ChatID, p.UserID, p.Generation)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (d *DB) CancelCaptchaGeneration(ctx context.Context, p PendingRow) error {
	_, err := d.sql.ExecContext(ctx, `DELETE FROM pending_captchas WHERE chat_id = ? AND user_id = ? AND generation = ?`,
		p.ChatID, p.UserID, p.Generation)
	return err
}

func (d *DB) FinishCaptchaPreparation(ctx context.Context, p PendingRow, kind EventKind) error {
	return d.finishPendingCheck(ctx, p.ChatID, p.UserID, kind,
		`DELETE FROM pending_captchas WHERE chat_id = ? AND user_id = ? AND generation = ? AND message_id = 0 AND ephemeral_msg_id = 0`,
		p.Generation)
}

func (d *DB) PutPending(ctx context.Context, p PendingRow) error {
	if p.Stage < 1 {
		p.Stage = 1
	}
	res, err := d.sql.ExecContext(ctx, `
		INSERT INTO pending_captchas (chat_id, user_id, message_id, correct_idx, expires_at, thread_id, ephemeral_msg_id, stage)
		SELECT ?, ?, ?, ?, ?, ?, ?, ? WHERE NOT EXISTS (
			SELECT 1 FROM pending_admissions WHERE chat_id = ? AND user_id = ?)
		ON CONFLICT(chat_id, user_id) DO UPDATE SET
			message_id = excluded.message_id,
			correct_idx = excluded.correct_idx,
			expires_at = excluded.expires_at,
			thread_id = excluded.thread_id,
			ephemeral_msg_id = excluded.ephemeral_msg_id,
			stage = excluded.stage,
			suspended = 0
	`, p.ChatID, p.UserID, p.MessageID, p.CorrectIdx, p.ExpiresAt.Unix(), p.ThreadID, p.EphemeralID, p.Stage, p.ChatID, p.UserID)
	if err := pendingCaptchaWritten(res, err); err != nil {
		return fmt.Errorf("put pending: %w", err)
	}
	return nil
}

func pendingCaptchaWritten(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return ErrAdmissionPending
	}
	return err
}

func (d *DB) DeletePending(ctx context.Context, chatID, userID int64) error {
	_, err := d.sql.ExecContext(ctx,
		`DELETE FROM pending_captchas WHERE chat_id = ? AND user_id = ?`,
		chatID, userID)
	if err != nil {
		return fmt.Errorf("delete pending: %w", err)
	}
	return nil
}

// DeletePendingIfMsg удаляет капчу, только если она всё ещё та же самая
// (совпадают message_id и ephemeral_msg_id). Отложенная очистка из
// завершившегося флоу не должна стереть строку НОВОЙ капчи того же юзера,
// успевшей взвестись, пока старый флоу доезжал (например, ретраи release
// под 429 растянулись на секунды).
func (d *DB) DeletePendingIfMsg(ctx context.Context, chatID, userID int64, msgID, ephID int) error {
	_, err := d.sql.ExecContext(ctx,
		`DELETE FROM pending_captchas WHERE chat_id = ? AND user_id = ?
		 AND message_id = ? AND ephemeral_msg_id = ?`,
		chatID, userID, msgID, ephID)
	if err != nil {
		return fmt.Errorf("delete pending (guarded): %w", err)
	}
	return nil
}

// DeletePendingChat удаляет все активные капчи чата. Вызывается, когда бот
// покидает чат (или его выгоняют) — сообщения капч там уже недоступны, и
// таймауты порождали бы только падающие вызовы kick/ban.
func (d *DB) DeletePendingChat(ctx context.Context, chatID int64) error {
	_, err := d.sql.ExecContext(ctx,
		`DELETE FROM pending_captchas WHERE chat_id = ?`, chatID)
	if err != nil {
		return fmt.Errorf("delete pending by chat: %w", err)
	}
	return nil
}

func (d *DB) HasPendingChatRestrictions(ctx context.Context, chatID int64) (bool, error) {
	var exists bool
	// Допуск и капча сменяют друг друга одной транзакцией.
	err := d.sql.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM pending_captchas WHERE chat_id = ?
		UNION ALL
		SELECT 1 FROM pending_admissions WHERE chat_id = ? AND release_needed = 1
	)`, chatID, chatID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check pending chat restrictions: %w", err)
	}
	return exists, nil
}

func (d *DB) HasPendingCaptchaRestriction(ctx context.Context, chatID, userID, until int64) (bool, error) {
	var exists bool
	err := d.sql.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pending_captchas
		WHERE chat_id = ? AND user_id = ? AND restrict_until = ?)`, chatID, userID, until).Scan(&exists)
	return exists, err
}

// PendingReply — ожидание «ответь на приветствие» (режим reply_check).
type PendingReply struct {
	WelcomeClaimed bool
	GreetingMsgID  int
	Suspended      bool
	ChatID         int64
	UserID         int64
	ExpiresAt      time.Time
	Stage          int // стадия серии напоминаний (1..3)
	ThreadID       int // топик форума для повторных отправок якоря; 0 = без топика
}

// PutPendingReply взводит (или перевзводит) ожидание ответа.
func (d *DB) PutPendingReply(ctx context.Context, r PendingReply) error {
	if r.Stage < 1 {
		r.Stage = 1
	}
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO pending_replies (chat_id, user_id, expires_at, stage, thread_id, greeting_msg_id)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(chat_id, user_id) DO UPDATE SET
			expires_at = excluded.expires_at,
			stage = excluded.stage,
			thread_id = excluded.thread_id,
			greeting_msg_id = excluded.greeting_msg_id,
			welcome_claimed = 0,
			suspended = 0
	`, r.ChatID, r.UserID, r.ExpiresAt.Unix(), r.Stage, r.ThreadID, r.GreetingMsgID)
	if err != nil {
		return fmt.Errorf("put pending reply: %w", err)
	}
	return nil
}

// BindPendingReplyGreeting привязывает якорь к стадии ожидания и
// одновременно обновляет ссылку для будущей очистки приветствия.
func (d *DB) BindPendingReplyGreeting(ctx context.Context, chatID, userID int64, stage, msgID int) (bool, error) {
	if msgID <= 0 {
		return false, nil
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck
	res, err := tx.ExecContext(ctx, `
		UPDATE pending_replies SET greeting_msg_id = ?, welcome_claimed = 0
		WHERE chat_id = ? AND user_id = ? AND stage = ? AND suspended = 0 AND greeting_msg_id = 0
	`, msgID, chatID, userID, stage)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, putGreetingSQL, chatID, userID, msgID, time.Now().Unix()); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// DeletePendingReplyIf — guard-удаление по СТАДИИ: очистка хвоста завершившейся
// стадии не должна стереть строку следующей (переход персистится ДО отправки
// напоминания, так что во время него в БД уже лежит stage+1). Guard по
// expires_at здесь не годится: цикл серии переписывает дедлайн в середине
// окна, и ответивший в это окно юзер стирал бы чужую строку или оставлял
// призрак, кикающий позже.
func (d *DB) DeletePendingReplyIf(ctx context.Context, chatID, userID int64, stage int) error {
	_, err := d.sql.ExecContext(ctx,
		`DELETE FROM pending_replies WHERE chat_id = ? AND user_id = ? AND stage = ?`,
		chatID, userID, stage)
	if err != nil {
		return fmt.Errorf("delete pending reply (guarded): %w", err)
	}
	return nil
}

// FinishReplyAnswer also covers the next stage while its reminder is being sent.
func (d *DB) FinishReplyAnswer(ctx context.Context, chatID, userID int64, stage, greetingID int) error {
	return d.finishPendingCheck(ctx, chatID, userID, EventPass,
		`DELETE FROM pending_replies WHERE chat_id = ? AND user_id = ? AND suspended = 0
		AND ((stage = ? AND (greeting_msg_id = ? OR greeting_msg_id = 0))
			OR (stage = ? AND greeting_msg_id = 0 AND ? = 0))`,
		stage, greetingID, stage+1, greetingID)
}

// ExtendPendingReplyIf — guard-продление ожидания ответа по СТАДИИ (прецедент
// DeletePendingReplyIf): мусорный ответ отодвигает дедлайн текущей стадии, но
// не должен переписать строку следующей (переход персистится ДО отправки
// напоминания) или воскресить удалённую (ответ/выход/кара уже разобрались).
// Продление «вхолостую» (0 строк) — норма: кто-то другой успел первым.
func (d *DB) ExtendPendingReplyIf(ctx context.Context, chatID, userID int64, stage int, expiresAt time.Time) error {
	_, err := d.sql.ExecContext(ctx,
		`UPDATE pending_replies SET expires_at = ? WHERE chat_id = ? AND user_id = ? AND stage = ?`,
		expiresAt.Unix(), chatID, userID, stage)
	if err != nil {
		return fmt.Errorf("extend pending reply (guarded): %w", err)
	}
	return nil
}

// DeletePendingRepliesChat сносит все ожидания чата (бот покинул чат).
func (d *DB) DeletePendingRepliesChat(ctx context.Context, chatID int64) error {
	_, err := d.sql.ExecContext(ctx,
		`DELETE FROM pending_replies WHERE chat_id = ?`, chatID)
	if err != nil {
		return fmt.Errorf("delete pending replies by chat: %w", err)
	}
	return nil
}

func (d *DB) LoadAllPendingReplies(ctx context.Context) ([]PendingReply, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT chat_id, user_id, expires_at, stage, thread_id, suspended, greeting_msg_id, welcome_claimed FROM pending_replies`)
	if err != nil {
		return nil, fmt.Errorf("load pending replies: %w", err)
	}
	defer rows.Close() //nolint:errcheck // cleanup, error intentionally ignored
	var out []PendingReply
	for rows.Next() {
		var r PendingReply
		var exp int64
		if err := rows.Scan(&r.ChatID, &r.UserID, &exp, &r.Stage, &r.ThreadID, &r.Suspended, &r.GreetingMsgID, &r.WelcomeClaimed); err != nil {
			return nil, fmt.Errorf("scan pending reply: %w", err)
		}
		r.ExpiresAt = time.Unix(exp, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

const pendingCaptchaColumns = `chat_id, user_id, message_id, correct_idx, expires_at, thread_id,
	ephemeral_msg_id, stage, suspended, generation, restrict_until`

func scanPendingCaptcha(row interface{ Scan(...any) error }) (PendingRow, error) {
	var p PendingRow
	var expiresUnix int64
	err := row.Scan(&p.ChatID, &p.UserID, &p.MessageID, &p.CorrectIdx, &expiresUnix, &p.ThreadID,
		&p.EphemeralID, &p.Stage, &p.Suspended, &p.Generation, &p.RestrictUntil)
	p.ExpiresAt = time.Unix(expiresUnix, 0)
	return p, err
}

func (d *DB) GetPendingCaptcha(ctx context.Context, chatID, userID int64) (PendingRow, bool, error) {
	p, err := scanPendingCaptcha(d.sql.QueryRowContext(ctx, `SELECT `+pendingCaptchaColumns+`
		FROM pending_captchas WHERE chat_id = ? AND user_id = ?`, chatID, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return p, false, nil
	}
	return p, err == nil, err
}

func (d *DB) LoadAllPending(ctx context.Context) ([]PendingRow, error) {
	return d.loadPendingCaptchas(ctx, `SELECT `+pendingCaptchaColumns+` FROM pending_captchas`)
}

func (d *DB) LoadCaptchaRestorations(ctx context.Context) ([]PendingRow, error) {
	return d.loadPendingCaptchas(ctx, `SELECT `+pendingCaptchaColumns+` FROM pending_captchas
		WHERE suspended = 1 OR (generation <> '' AND message_id = 0 AND ephemeral_msg_id = 0)`)
}

func (d *DB) loadPendingCaptchas(ctx context.Context, query string) ([]PendingRow, error) {
	rows, err := d.sql.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("load pending: %w", err)
	}
	defer rows.Close() //nolint:errcheck // cleanup, error intentionally ignored

	var out []PendingRow
	for rows.Next() {
		p, err := scanPendingCaptcha(rows)
		if err != nil {
			return nil, fmt.Errorf("scan pending: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SuspendPendingChecks сохраняет данные проверок чата, пока он недоступен.
func (d *DB) SuspendPendingChecks(ctx context.Context, chatID int64) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, `UPDATE pending_captchas SET suspended = 1 WHERE chat_id = ? AND suspended = 0`, chatID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE pending_replies SET suspended = 1 WHERE chat_id = ? AND suspended = 0`, chatID); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) FinishSuspendedCaptcha(ctx context.Context, row PendingRow, kind EventKind) error {
	return d.finishPendingCheck(ctx, row.ChatID, row.UserID, kind,
		`DELETE FROM pending_captchas WHERE chat_id = ? AND user_id = ? AND suspended = 1
		AND generation = ? AND message_id = ? AND ephemeral_msg_id = ? AND expires_at = ? AND restrict_until = ?
		AND correct_idx = ? AND thread_id = ? AND stage = ?`,
		row.Generation, row.MessageID, row.EphemeralID, row.ExpiresAt.Unix(), row.RestrictUntil, row.CorrectIdx, row.ThreadID, row.Stage)
}

// FinishUnconfirmedReplyWelcome закрывает допуск, если результат отправки
// первого приветствия после снятия прав неизвестен.
func (d *DB) FinishUnconfirmedReplyWelcome(ctx context.Context, row PendingReply) error {
	return d.finishPendingCheck(ctx, row.ChatID, row.UserID, EventPass,
		`DELETE FROM pending_replies WHERE chat_id = ? AND user_id = ? AND welcome_claimed = 1 AND greeting_msg_id = 0`)
}

func (d *DB) FinishSuspendedReply(ctx context.Context, row PendingReply) error {
	return d.finishPendingCheck(ctx, row.ChatID, row.UserID, EventAbort,
		`DELETE FROM pending_replies WHERE chat_id = ? AND user_id = ? AND suspended = 1`)
}

// Закрытие воронки и удаление строки фиксируются вместе.
func (d *DB) finishPendingCheck(ctx context.Context, chatID, userID int64, kind EventKind, deleteSQL string, extraArgs ...any) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	res, err := tx.ExecContext(ctx, deleteSQL, append([]any{chatID, userID}, extraArgs...)...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO events (chat_id, user_id, kind, at) VALUES (?, ?, ?, ?)
	`, chatID, userID, kind, time.Now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}
