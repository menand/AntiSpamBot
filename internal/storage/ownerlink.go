package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// OwnerDMLink — связка «служебное сообщение бота в ЛС владельца» →
// «сообщение в исходном чате» (таблица owner_dm_links). По ней ответ владельца
// реплаем в личке превращается в публикацию от имени бота в чате.
type OwnerDMLink struct {
	ChatID      int64
	ThreadID    int
	SourceMsgID int
	// IsForward — линк на сам форвард (а не на соседнюю карточку): только на
	// форварде частичная цитата владельца относится к исходному сообщению.
	IsForward bool
}

// PutOwnerDMLink запоминает связку. Нулевые id — не ошибка, а «связывать
// нечего» (форвард не удался): строка просто не пишется, фича деградирует до
// «ответ на карточку работает, на форвард — нет». Best effort по смыслу:
// вызывающие только логируют ошибку, доставка уведомления важнее линка.
//
// bot_id обязателен: нумерация сообщений в ЛС своя у КАЖДОГО бота, а БД
// переживает смену BOT_TOKEN. Без привязки к боту карточка старого бота с тем
// же dm_message_id столкнулась бы с новой и увела ответ в чужой чат.
func (d *DB) PutOwnerDMLink(ctx context.Context, botID, ownerID int64, dmMsgID int, chatID int64,
	threadID, sourceMsgID int, isForward bool, at time.Time) error {
	if botID == 0 || ownerID == 0 || dmMsgID <= 0 || chatID == 0 || sourceMsgID <= 0 {
		return nil
	}
	forward := 0
	if isForward {
		forward = 1
	}
	if _, err := d.sql.ExecContext(ctx, `
		INSERT OR REPLACE INTO owner_dm_links
			(bot_id, owner_id, dm_message_id, chat_id, thread_id, source_msg_id, is_forward, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, botID, ownerID, dmMsgID, chatID, threadID, sourceMsgID, forward, at.Unix()); err != nil {
		return fmt.Errorf("put owner dm link: %w", err)
	}
	return nil
}

// OwnerDMLink ищет связку по тройке (бот, владелец, id сообщения в ЛС). false —
// строки нет (сообщение не наше, линк протух/вычищен, или это сообщение
// другого бота в той же БД): вызывающий уходит в обычную обработку личных
// сообщений.
func (d *DB) OwnerDMLink(ctx context.Context, botID, ownerID int64, dmMsgID int) (OwnerDMLink, bool, error) {
	var (
		link    OwnerDMLink
		forward int
	)
	err := d.sql.QueryRowContext(ctx, `
		SELECT chat_id, thread_id, source_msg_id, is_forward
		FROM owner_dm_links
		WHERE bot_id = ? AND owner_id = ? AND dm_message_id = ?
	`, botID, ownerID, dmMsgID).Scan(&link.ChatID, &link.ThreadID, &link.SourceMsgID, &forward)
	if errors.Is(err, sql.ErrNoRows) {
		return OwnerDMLink{}, false, nil
	}
	if err != nil {
		return OwnerDMLink{}, false, fmt.Errorf("owner dm link: %w", err)
	}
	link.IsForward = forward != 0
	return link, true, nil
}

// PruneOwnerDMLinks удаляет связки старше before: карточка месячной давности
// — уже не повод публиковать что-то «от имени бота» задним числом.
func (d *DB) PruneOwnerDMLinks(ctx context.Context, before time.Time) (int64, error) {
	res, err := d.sql.ExecContext(ctx,
		`DELETE FROM owner_dm_links WHERE at < ?`, before.Unix())
	if err != nil {
		return 0, fmt.Errorf("prune owner dm links: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune owner dm links: rows affected: %w", err)
	}
	return n, nil
}
