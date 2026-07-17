package storage

import (
	"context"
	"fmt"
	"time"
)

type PendingRow struct {
	ChatID      int64
	UserID      int64
	MessageID   int
	CorrectIdx  int
	ExpiresAt   time.Time
	ThreadID    int // топик форума, в котором вошёл юзер; 0 = без топика
	EphemeralID int // ≠0: капча эфемерная, удалять по этому id
}

func (d *DB) PutPending(ctx context.Context, p PendingRow) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO pending_captchas (chat_id, user_id, message_id, correct_idx, expires_at, thread_id, ephemeral_msg_id)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(chat_id, user_id) DO UPDATE SET
			message_id = excluded.message_id,
			correct_idx = excluded.correct_idx,
			expires_at = excluded.expires_at,
			thread_id = excluded.thread_id,
			ephemeral_msg_id = excluded.ephemeral_msg_id
	`, p.ChatID, p.UserID, p.MessageID, p.CorrectIdx, p.ExpiresAt.Unix(), p.ThreadID, p.EphemeralID)
	if err != nil {
		return fmt.Errorf("put pending: %w", err)
	}
	return nil
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

// PendingReply — ожидание «ответь на приветствие» (режим reply_check).
type PendingReply struct {
	ChatID    int64
	UserID    int64
	ExpiresAt time.Time
}

// PutPendingReply взводит (или перевзводит) ожидание ответа.
func (d *DB) PutPendingReply(ctx context.Context, r PendingReply) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO pending_replies (chat_id, user_id, expires_at)
		VALUES (?, ?, ?)
		ON CONFLICT(chat_id, user_id) DO UPDATE SET expires_at = excluded.expires_at
	`, r.ChatID, r.UserID, r.ExpiresAt.Unix())
	if err != nil {
		return fmt.Errorf("put pending reply: %w", err)
	}
	return nil
}

func (d *DB) DeletePendingReply(ctx context.Context, chatID, userID int64) error {
	_, err := d.sql.ExecContext(ctx,
		`DELETE FROM pending_replies WHERE chat_id = ? AND user_id = ?`,
		chatID, userID)
	if err != nil {
		return fmt.Errorf("delete pending reply: %w", err)
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
		`SELECT chat_id, user_id, expires_at FROM pending_replies`)
	if err != nil {
		return nil, fmt.Errorf("load pending replies: %w", err)
	}
	defer rows.Close()
	var out []PendingReply
	for rows.Next() {
		var r PendingReply
		var exp int64
		if err := rows.Scan(&r.ChatID, &r.UserID, &exp); err != nil {
			return nil, fmt.Errorf("scan pending reply: %w", err)
		}
		r.ExpiresAt = time.Unix(exp, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) LoadAllPending(ctx context.Context) ([]PendingRow, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT chat_id, user_id, message_id, correct_idx, expires_at, thread_id, ephemeral_msg_id FROM pending_captchas`)
	if err != nil {
		return nil, fmt.Errorf("load pending: %w", err)
	}
	defer rows.Close()

	var out []PendingRow
	for rows.Next() {
		var p PendingRow
		var expiresUnix int64
		if err := rows.Scan(&p.ChatID, &p.UserID, &p.MessageID, &p.CorrectIdx, &expiresUnix, &p.ThreadID, &p.EphemeralID); err != nil {
			return nil, fmt.Errorf("scan pending: %w", err)
		}
		p.ExpiresAt = time.Unix(expiresUnix, 0)
		out = append(out, p)
	}
	return out, rows.Err()
}
