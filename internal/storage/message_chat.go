package storage

import (
	"context"
	"database/sql"
	"fmt"
)

const messageChatHistorySQL = `
	WITH RECURSIVE history(id) AS (
		VALUES (?)
		UNION
		SELECT m.old_id FROM message_chat_moves m JOIN history h ON m.new_id = h.id
	)
`

func recordMessageChatMove(ctx context.Context, tx *sql.Tx, oldID, newID int64) error {
	var cycle bool
	if err := tx.QueryRowContext(ctx, messageChatHistorySQL+`SELECT EXISTS (SELECT 1 FROM history WHERE id = ?)`,
		oldID, newID).Scan(&cycle); err != nil {
		return fmt.Errorf("check message chat move: %w", err)
	}
	if cycle {
		return fmt.Errorf("message chat move %d to %d would create a cycle", oldID, newID)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO message_chat_moves (old_id, new_id) VALUES (?, ?)
		ON CONFLICT(old_id) DO NOTHING`, oldID, newID); err != nil {
		return fmt.Errorf("record message chat move: %w", err)
	}
	var target int64
	if err := tx.QueryRowContext(ctx, `SELECT new_id FROM message_chat_moves WHERE old_id = ?`, oldID).Scan(&target); err != nil {
		return fmt.Errorf("check recorded message chat move: %w", err)
	}
	if target != newID {
		return fmt.Errorf("message chat %d was already moved to %d", oldID, target)
	}
	return nil
}

func messageChatHistory(ctx context.Context, tx *sql.Tx, chatID int64) ([]int64, error) {
	var migratedSchema bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = 'message_chat_moves'
	)`).Scan(&migratedSchema); err != nil {
		return nil, fmt.Errorf("read chat history schema: %w", err)
	}
	if !migratedSchema {
		return []int64{chatID}, nil
	}
	rows, err := tx.QueryContext(ctx, messageChatHistorySQL+`SELECT id FROM history`, chatID)
	if err != nil {
		return nil, fmt.Errorf("query message chat history: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan message chat history: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
