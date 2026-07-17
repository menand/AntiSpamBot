package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SpamVote — активное голосование «спам/не спам» под подозрительным
// сообщением ИЛИ «забанить по профилю?» для подозрительного профиля новичка.
type SpamVote struct {
	ChatID   int64
	BotMsgID int
	// TargetMsgID — подозрительное сообщение; 0 = ПРОФИЛЬНАЯ плашка
	// (голосование о бане по профилю, целевого сообщения не существует).
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
	if errors.Is(err, sql.ErrNoRows) {
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
// двойные клики отсекаются здесь). Бюллетени чистятся в той же транзакции —
// крэш между DELETE'ами не оставляет бюллетеней-сирот.
func (d *DB) TakeSpamVote(ctx context.Context, chatID int64, botMsgID int) (bool, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("take spam vote: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	res, err := tx.ExecContext(ctx,
		`DELETE FROM spam_votes WHERE chat_id = ? AND bot_msg_id = ?`, chatID, botMsgID)
	if err != nil {
		return false, fmt.Errorf("take spam vote: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("take spam vote rows: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM spam_ballots WHERE chat_id = ? AND bot_msg_id = ?`, chatID, botMsgID); err != nil {
		return false, fmt.Errorf("clear spam ballots: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("take spam vote: commit: %w", err)
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
	if errors.Is(err, sql.ErrNoRows) {
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

// Ballot — один голос в голосовании «спам/не спам».
type Ballot struct {
	VoterID int64
	IsSpam  bool
}

// ListBallots — все голоса активного голосования. Вызывать ДО TakeSpamVote:
// он удаляет бюллетени в своей транзакции.
func (d *DB) ListBallots(ctx context.Context, chatID int64, botMsgID int) ([]Ballot, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT voter_id, is_spam FROM spam_ballots WHERE chat_id = ? AND bot_msg_id = ?`,
		chatID, botMsgID)
	if err != nil {
		return nil, fmt.Errorf("list ballots: %w", err)
	}
	defer rows.Close()
	var out []Ballot
	for rows.Next() {
		var bl Ballot
		var isSpam int
		if err := rows.Scan(&bl.VoterID, &isSpam); err != nil {
			return nil, fmt.Errorf("scan ballot: %w", err)
		}
		bl.IsSpam = isSpam != 0
		out = append(out, bl)
	}
	return out, rows.Err()
}

// AddSpamBanned заносит юзера в общую базу спамеров. Повторный вердикт в
// другом чате не перезаписывает первоисточник.
func (d *DB) AddSpamBanned(ctx context.Context, userID, chatID int64, at time.Time) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO spam_banned (user_id, chat_id, at) VALUES (?, ?, ?)
		ON CONFLICT(user_id) DO NOTHING
	`, userID, chatID, at.Unix())
	if err != nil {
		return fmt.Errorf("add spam banned: %w", err)
	}
	return nil
}

func (d *DB) IsSpamBanned(ctx context.Context, userID int64) (bool, error) {
	var one int
	err := d.sql.QueryRowContext(ctx,
		`SELECT 1 FROM spam_banned WHERE user_id = ?`, userID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("is spam banned: %w", err)
	}
	return true, nil
}

// DeleteSpamBanned — прощение: ручной разбан админом в любом чате снимает
// глобальный флаг, иначе ошибочный вердикт был бы неисправим (join-хук банил
// бы заново при каждом входе). Возвращает, была ли запись.
func (d *DB) DeleteSpamBanned(ctx context.Context, userID int64) (bool, error) {
	res, err := d.sql.ExecContext(ctx,
		`DELETE FROM spam_banned WHERE user_id = ?`, userID)
	if err != nil {
		return false, fmt.Errorf("delete spam banned: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("delete spam banned rows: %w", err)
	}
	return n > 0, nil
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

// UserMessageTotalsByChat — сколько сообщений юзер написал в каждом чате
// бота. Кросс-чатовое доверие: наговорил на белый список в одном чате —
// доверяем и в остальных (максимум по чатам, не сумма: 3+3 — ещё не доверие);
// фильтр по ALLOWED_CHATS применяет вызывающий — это конфиг, не БД.
func (d *DB) UserMessageTotalsByChat(ctx context.Context, userID int64) (map[int64]int, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT chat_id, SUM(count) FROM user_message_counts
		WHERE user_id = ? GROUP BY chat_id
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("user message totals by chat: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]int)
	for rows.Next() {
		var chatID int64
		var n int
		if err := rows.Scan(&chatID, &n); err != nil {
			return nil, fmt.Errorf("scan user message totals: %w", err)
		}
		out[chatID] = n
	}
	return out, rows.Err()
}

// SpamNotifyEnabled — включены ли у владельца ЛС-уведомления о спаме.
func (d *DB) SpamNotifyEnabled(ctx context.Context, ownerID int64) (bool, error) {
	var on int
	err := d.sql.QueryRowContext(ctx,
		`SELECT spam_notify FROM owner_settings WHERE owner_id = ?`, ownerID).Scan(&on)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("spam notify enabled: %w", err)
	}
	return on != 0, nil
}

// SpamNotifyOwners — все владельцы с включёнными уведомлениями одним
// запросом (вместо точечного SELECT на каждого при каждом событии).
func (d *DB) SpamNotifyOwners(ctx context.Context) ([]int64, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT owner_id FROM owner_settings WHERE spam_notify != 0`)
	if err != nil {
		return nil, fmt.Errorf("spam notify owners: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan spam notify owner: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (d *DB) SetSpamNotify(ctx context.Context, ownerID int64, on bool) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO owner_settings (owner_id, spam_notify) VALUES (?, ?)
		ON CONFLICT(owner_id) DO UPDATE SET spam_notify = excluded.spam_notify
	`, ownerID, boolToInt(on))
	if err != nil {
		return fmt.Errorf("set spam notify: %w", err)
	}
	return nil
}

// ModNotifyEnabled — включены ли у владельца ЛС-уведомления о киках/банах
// и проходах капчи.
func (d *DB) ModNotifyEnabled(ctx context.Context, ownerID int64) (bool, error) {
	var on int
	err := d.sql.QueryRowContext(ctx,
		`SELECT mod_notify FROM owner_settings WHERE owner_id = ?`, ownerID).Scan(&on)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("mod notify enabled: %w", err)
	}
	return on != 0, nil
}

// ModNotifyOwners — все владельцы с включёнными уведомлениями о модерации.
func (d *DB) ModNotifyOwners(ctx context.Context) ([]int64, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT owner_id FROM owner_settings WHERE mod_notify != 0`)
	if err != nil {
		return nil, fmt.Errorf("mod notify owners: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan mod notify owner: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (d *DB) SetModNotify(ctx context.Context, ownerID int64, on bool) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO owner_settings (owner_id, mod_notify) VALUES (?, ?)
		ON CONFLICT(owner_id) DO UPDATE SET mod_notify = excluded.mod_notify
	`, ownerID, boolToInt(on))
	if err != nil {
		return fmt.Errorf("set mod notify: %w", err)
	}
	return nil
}

// CaptchaNotifyEnabled — включены ли у владельца ЛС-уведомления о КАЖДОМ
// провале капчи (mod_notify шлёт провалы только со второй попытки).
func (d *DB) CaptchaNotifyEnabled(ctx context.Context, ownerID int64) (bool, error) {
	var on int
	err := d.sql.QueryRowContext(ctx,
		`SELECT captcha_notify FROM owner_settings WHERE owner_id = ?`, ownerID).Scan(&on)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("captcha notify enabled: %w", err)
	}
	return on != 0, nil
}

// CaptchaNotifyOwners — все владельцы с подпиской на все провалы капчи.
func (d *DB) CaptchaNotifyOwners(ctx context.Context) ([]int64, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT owner_id FROM owner_settings WHERE captcha_notify != 0`)
	if err != nil {
		return nil, fmt.Errorf("captcha notify owners: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan captcha notify owner: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (d *DB) SetCaptchaNotify(ctx context.Context, ownerID int64, on bool) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO owner_settings (owner_id, captcha_notify) VALUES (?, ?)
		ON CONFLICT(owner_id) DO UPDATE SET captcha_notify = excluded.captcha_notify
	`, ownerID, boolToInt(on))
	if err != nil {
		return fmt.Errorf("set captcha notify: %w", err)
	}
	return nil
}

// DailyReportEnabled — включена ли у юзера утренняя ЛС-сводка по его чатам.
// В отличие от spam/mod_notify доступна не только владельцам, но и админам.
func (d *DB) DailyReportEnabled(ctx context.Context, userID int64) (bool, error) {
	var on int
	err := d.sql.QueryRowContext(ctx,
		`SELECT daily_report FROM owner_settings WHERE owner_id = ?`, userID).Scan(&on)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("daily report enabled: %w", err)
	}
	return on != 0, nil
}

func (d *DB) SetDailyReport(ctx context.Context, userID int64, on bool) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO owner_settings (owner_id, daily_report) VALUES (?, ?)
		ON CONFLICT(owner_id) DO UPDATE SET daily_report = excluded.daily_report
	`, userID, boolToInt(on))
	if err != nil {
		return fmt.Errorf("set daily report: %w", err)
	}
	return nil
}

// ReportSub — подписчик ЛС-сводки и день последней отправки ("" = ещё не слали).
type ReportSub struct {
	UserID  int64
	LastDay string
}

// DailyReportSubscribers — все подписчики сводки одним запросом.
func (d *DB) DailyReportSubscribers(ctx context.Context) ([]ReportSub, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT owner_id, COALESCE(last_report_day, '') FROM owner_settings WHERE daily_report != 0`)
	if err != nil {
		return nil, fmt.Errorf("daily report subscribers: %w", err)
	}
	defer rows.Close()
	var out []ReportSub
	for rows.Next() {
		var s ReportSub
		if err := rows.Scan(&s.UserID, &s.LastDay); err != nil {
			return nil, fmt.Errorf("scan daily report subscriber: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// MarkDailyReportSent помечает день отправленным (аналог MarkDailyStatsSent,
// но per-user): гейт в maybeSendDMReports не пошлёт сводку дважды.
func (d *DB) MarkDailyReportSent(ctx context.Context, userID int64, day string) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO owner_settings (owner_id, last_report_day) VALUES (?, ?)
		ON CONFLICT(owner_id) DO UPDATE SET last_report_day = excluded.last_report_day
	`, userID, day)
	if err != nil {
		return fmt.Errorf("mark daily report sent: %w", err)
	}
	return nil
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
	if errors.Is(err, sql.ErrNoRows) {
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

// GreetingUserByMsg находит юзера по message_id его приветствия — резолв
// цели для /kick|/ban реплаем на «Добро пожаловать». (0, false, nil) — нет.
func (d *DB) GreetingUserByMsg(ctx context.Context, chatID int64, messageID int) (int64, bool, error) {
	var userID int64
	err := d.sql.QueryRowContext(ctx,
		`SELECT user_id FROM greetings WHERE chat_id = ? AND message_id = ?`,
		chatID, messageID).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("greeting user by msg: %w", err)
	}
	return userID, true, nil
}

// TakeSpamVoteByAuthor удаляет активную плашку голосования по автору и
// возвращает её bot_msg_id — чтобы снести само сообщение при /kick|/ban.
// Бюллетени чистятся в той же транзакции. (0, false, nil) — плашки нет.
func (d *DB) TakeSpamVoteByAuthor(ctx context.Context, chatID, authorID int64) (int, bool, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("take vote by author: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var botMsgID int
	err = tx.QueryRowContext(ctx,
		`SELECT bot_msg_id FROM spam_votes WHERE chat_id = ? AND author_id = ? LIMIT 1`,
		chatID, authorID).Scan(&botMsgID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("take vote by author: select: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM spam_votes WHERE chat_id = ? AND bot_msg_id = ?`, chatID, botMsgID); err != nil {
		return 0, false, fmt.Errorf("take vote by author: delete vote: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM spam_ballots WHERE chat_id = ? AND bot_msg_id = ?`, chatID, botMsgID); err != nil {
		return 0, false, fmt.Errorf("take vote by author: delete ballots: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("take vote by author: commit: %w", err)
	}
	return botMsgID, true, nil
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
