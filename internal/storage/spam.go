package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// SpamVote — активное голосование «спам/не спам» под подозрительным сообщением.
type SpamVote struct {
	ChatID      int64
	BotMsgID    int
	TargetMsgID int
	AuthorID    int64
	Prob        int
	CreatedAt   time.Time
}

func (d *DB) PutSpamVote(ctx context.Context, v SpamVote) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO spam_votes (chat_id, bot_msg_id, target_msg_id, author_id, prob, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(chat_id, bot_msg_id) DO NOTHING
	`, v.ChatID, v.BotMsgID, v.TargetMsgID, v.AuthorID, v.Prob, v.CreatedAt.Unix())
	if err != nil {
		return fmt.Errorf("put spam vote: %w", err)
	}
	return nil
}

func (d *DB) GetSpamVote(ctx context.Context, chatID int64, botMsgID int) (SpamVote, bool, error) {
	var v SpamVote
	var at int64
	err := d.sql.QueryRowContext(ctx, `
		SELECT chat_id, bot_msg_id, target_msg_id, author_id, prob, created_at
		FROM spam_votes WHERE chat_id = ? AND bot_msg_id = ?
	`, chatID, botMsgID).Scan(&v.ChatID, &v.BotMsgID, &v.TargetMsgID, &v.AuthorID, &v.Prob, &at)
	if err == sql.ErrNoRows {
		return v, false, nil
	}
	if err != nil {
		return v, false, fmt.Errorf("get spam vote: %w", err)
	}
	v.CreatedAt = time.Unix(at, 0)
	return v, true, nil
}

// TakeSpamVote атомарно забирает голосование: первый успешный вызов получает
// true и право исполнить вердикт, все последующие — false (гонки коллбэков и
// двойные клики отсекаются здесь). Бюллетени чистятся заодно.
func (d *DB) TakeSpamVote(ctx context.Context, chatID int64, botMsgID int) (bool, error) {
	res, err := d.sql.ExecContext(ctx,
		`DELETE FROM spam_votes WHERE chat_id = ? AND bot_msg_id = ?`, chatID, botMsgID)
	if err != nil {
		return false, fmt.Errorf("take spam vote: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("take spam vote rows: %w", err)
	}
	if _, err := d.sql.ExecContext(ctx,
		`DELETE FROM spam_ballots WHERE chat_id = ? AND bot_msg_id = ?`, chatID, botMsgID); err != nil {
		return false, fmt.Errorf("clear spam ballots: %w", err)
	}
	return n > 0, nil
}

// HasPendingVoteForAuthor: не вешаем вторую плашку на того же автора — при
// вердикте «спам» revoke_messages снесёт все его сообщения разом.
func (d *DB) HasPendingVoteForAuthor(ctx context.Context, chatID, authorID int64) (bool, error) {
	var one int
	err := d.sql.QueryRowContext(ctx,
		`SELECT 1 FROM spam_votes WHERE chat_id = ? AND author_id = ? LIMIT 1`,
		chatID, authorID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("pending vote for author: %w", err)
	}
	return true, nil
}

// UpsertBallot записывает голос; повторное нажатие того же юзера меняет голос.
func (d *DB) UpsertBallot(ctx context.Context, chatID int64, botMsgID int, voterID int64, isSpam bool) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO spam_ballots (chat_id, bot_msg_id, voter_id, is_spam)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(chat_id, bot_msg_id, voter_id) DO UPDATE SET is_spam = excluded.is_spam
	`, chatID, botMsgID, voterID, boolToInt(isSpam))
	if err != nil {
		return fmt.Errorf("upsert ballot: %w", err)
	}
	return nil
}

func (d *DB) CountBallots(ctx context.Context, chatID int64, botMsgID int) (yes, no int, err error) {
	err = d.sql.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(is_spam), 0), COALESCE(SUM(1 - is_spam), 0)
		FROM spam_ballots WHERE chat_id = ? AND bot_msg_id = ?
	`, chatID, botMsgID).Scan(&yes, &no)
	if err != nil {
		return 0, 0, fmt.Errorf("count ballots: %w", err)
	}
	return yes, no, nil
}

// ExpiredSpamVotes — голосования старше olderThan (для часового свипера:
// после 48 ч Telegram не даст удалить плашку, тянуть нельзя).
func (d *DB) ExpiredSpamVotes(ctx context.Context, olderThan time.Time) ([]SpamVote, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT chat_id, bot_msg_id, target_msg_id, author_id, prob, created_at
		FROM spam_votes WHERE created_at < ?
	`, olderThan.Unix())
	if err != nil {
		return nil, fmt.Errorf("expired spam votes: %w", err)
	}
	defer rows.Close()
	var out []SpamVote
	for rows.Next() {
		var v SpamVote
		var at int64
		if err := rows.Scan(&v.ChatID, &v.BotMsgID, &v.TargetMsgID, &v.AuthorID, &v.Prob, &at); err != nil {
			return nil, fmt.Errorf("scan expired vote: %w", err)
		}
		v.CreatedAt = time.Unix(at, 0)
		out = append(out, v)
	}
	return out, rows.Err()
}

// UserMessageTotal — сколько всего сообщений юзер написал в чате (для белого
// списка антиспама). Считается по дневным агрегатам, копится исторически.
func (d *DB) UserMessageTotal(ctx context.Context, chatID, userID int64) (int, error) {
	var n int
	err := d.sql.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(count), 0) FROM user_message_counts
		WHERE chat_id = ? AND user_id = ?
	`, chatID, userID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("user message total: %w", err)
	}
	return n, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// PutGreeting запоминает message_id приветствия для (chat, user), чтобы при
// спам-бане снести и его. Повторный вход перезаписывает.
func (d *DB) PutGreeting(ctx context.Context, chatID, userID int64, messageID int, at time.Time) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO greetings (chat_id, user_id, message_id, sent_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(chat_id, user_id) DO UPDATE SET
			message_id = excluded.message_id, sent_at = excluded.sent_at
	`, chatID, userID, messageID, at.Unix())
	if err != nil {
		return fmt.Errorf("put greeting: %w", err)
	}
	return nil
}

// TakeGreetingMsg возвращает и удаляет запомненное приветствие юзера.
func (d *DB) TakeGreetingMsg(ctx context.Context, chatID, userID int64) (int, bool, error) {
	var msgID int
	err := d.sql.QueryRowContext(ctx,
		`SELECT message_id FROM greetings WHERE chat_id = ? AND user_id = ?`,
		chatID, userID).Scan(&msgID)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("take greeting: %w", err)
	}
	if _, err := d.sql.ExecContext(ctx,
		`DELETE FROM greetings WHERE chat_id = ? AND user_id = ?`, chatID, userID); err != nil {
		return 0, false, fmt.Errorf("delete greeting row: %w", err)
	}
	return msgID, true, nil
}

// PruneGreetings выкидывает записи старше olderThan — сообщения старше 48 ч
// Telegram боту удалять не даёт, хранить их id незачем.
func (d *DB) PruneGreetings(ctx context.Context, olderThan time.Time) error {
	_, err := d.sql.ExecContext(ctx,
		`DELETE FROM greetings WHERE sent_at < ?`, olderThan.Unix())
	if err != nil {
		return fmt.Errorf("prune greetings: %w", err)
	}
	return nil
}
