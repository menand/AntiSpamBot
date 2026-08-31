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
	// InitiatorID — кто запустил голосование командой /spam; 0 = плашка ИИ.
	// Инициатор (как и автор) не голосует в своём репорте.
	InitiatorID int64
	Prob        int
	CreatedAt   time.Time
}

const spamVoteColumns = `chat_id, bot_msg_id, target_msg_id, author_id, initiator_id, prob, created_at`

// PutSpamVoteOnce создаёт голосование ТОЛЬКО если у автора ещё нет живой
// плашки — атомарно, одним INSERT…WHERE NOT EXISTS (check-then-act гонка
// двух создателей исключена: проигравший получает ok=false и сносит свою
// только что отправленную плашку). Инвариант «одна активная плашка на
// автора» общий для всех трёх путей создания: ИИ-спам-чек, профиль-чек и
// ручной репорт /spam.
func (d *DB) PutSpamVoteOnce(ctx context.Context, v SpamVote) (bool, error) {
	res, err := d.sql.ExecContext(ctx, `
		INSERT INTO spam_votes (chat_id, bot_msg_id, target_msg_id, author_id, initiator_id, prob, created_at)
		SELECT ?, ?, ?, ?, ?, ?, ?
		WHERE NOT EXISTS (
			SELECT 1 FROM spam_votes WHERE chat_id = ? AND author_id = ?
		)
	`, v.ChatID, v.BotMsgID, v.TargetMsgID, v.AuthorID, v.InitiatorID,
		v.Prob, v.CreatedAt.Unix(), v.ChatID, v.AuthorID)
	if err != nil {
		return false, fmt.Errorf("put spam vote once: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("put spam vote once rows: %w", err)
	}
	return n == 1, nil
}

func (d *DB) PutSpamVote(ctx context.Context, v SpamVote) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO spam_votes (chat_id, bot_msg_id, target_msg_id, author_id, initiator_id, prob, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(chat_id, bot_msg_id) DO NOTHING
	`, v.ChatID, v.BotMsgID, v.TargetMsgID, v.AuthorID, v.InitiatorID, v.Prob, v.CreatedAt.Unix())
	if err != nil {
		return fmt.Errorf("put spam vote: %w", err)
	}
	return nil
}

func scanSpamVote(scanner interface{ Scan(dest ...any) error }) (SpamVote, error) {
	var v SpamVote
	var at int64
	if err := scanner.Scan(&v.ChatID, &v.BotMsgID, &v.TargetMsgID, &v.AuthorID,
		&v.InitiatorID, &v.Prob, &at); err != nil {
		return v, err
	}
	v.CreatedAt = time.Unix(at, 0)
	return v, nil
}

func (d *DB) GetSpamVote(ctx context.Context, chatID int64, botMsgID int) (SpamVote, bool, error) {
	row := d.sql.QueryRowContext(ctx, `
		SELECT `+spamVoteColumns+` FROM spam_votes WHERE chat_id = ? AND bot_msg_id = ?
	`, chatID, botMsgID)
	v, err := scanSpamVote(row)
	if errors.Is(err, sql.ErrNoRows) {
		return SpamVote{}, false, nil
	}
	if err != nil {
		return SpamVote{}, false, fmt.Errorf("get spam vote: %w", err)
	}
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

// UpsertBallot записывает голос; повторное нажатие того же юзера меняет
// голос. Живость голосования перепроверяется В ТОЙ ЖЕ транзакции: клик,
// заехавший в окне между чтением плашки и записью бюллетеня, иначе навсегда
// осиротил бы строку (сирот никто не чистит) и получил бы ложное «Голос
// учтён». ok=false — голосование уже закрыто.
func (d *DB) UpsertBallot(ctx context.Context, chatID int64, botMsgID int, voterID int64, isSpam bool) (bool, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("upsert ballot: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // откат после Commit — no-op
	var one int
	err = tx.QueryRowContext(ctx,
		`SELECT 1 FROM spam_votes WHERE chat_id = ? AND bot_msg_id = ?`,
		chatID, botMsgID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("upsert ballot: vote check: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO spam_ballots (chat_id, bot_msg_id, voter_id, is_spam)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(chat_id, bot_msg_id, voter_id) DO UPDATE SET is_spam = excluded.is_spam
	`, chatID, botMsgID, voterID, boolToInt(isSpam)); err != nil {
		return false, fmt.Errorf("upsert ballot: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("upsert ballot: commit: %w", err)
	}
	return true, nil
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

// YoungSpamVotes — живые голосования (не старше cutoff): для стартовой
// реконсиляции кворума, набранного прямо перед крахом процесса.
func (d *DB) YoungSpamVotes(ctx context.Context, cutoff time.Time) ([]SpamVote, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT `+spamVoteColumns+` FROM spam_votes WHERE created_at >= ?
	`, cutoff.Unix())
	if err != nil {
		return nil, fmt.Errorf("young spam votes: %w", err)
	}
	defer rows.Close()
	var out []SpamVote
	for rows.Next() {
		v, err := scanSpamVote(rows)
		if err != nil {
			return nil, fmt.Errorf("scan young vote: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ExpiredSpamVotes — голосования старше olderThan (для часового свипера:
// после 48 ч Telegram не даст удалить плашку, тянуть нельзя).
func (d *DB) ExpiredSpamVotes(ctx context.Context, olderThan time.Time) ([]SpamVote, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT `+spamVoteColumns+` FROM spam_votes WHERE created_at < ?
	`, olderThan.Unix())
	if err != nil {
		return nil, fmt.Errorf("expired spam votes: %w", err)
	}
	defer rows.Close()
	var out []SpamVote
	for rows.Next() {
		v, err := scanSpamVote(rows)
		if err != nil {
			return nil, fmt.Errorf("scan expired vote: %w", err)
		}
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

// ownerFlagEnabled читает булев флаг owner_settings; нет строки — false.
// col здесь и в хелперах ниже — всегда константа вызывающего, не
// пользовательский ввод.
func (d *DB) ownerFlagEnabled(ctx context.Context, ownerID int64, col string) (bool, error) {
	var on int
	err := d.sql.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT %s FROM owner_settings WHERE owner_id = ?`, col), ownerID).Scan(&on)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%s enabled: %w", col, err)
	}
	return on != 0, nil
}

// ownerFlagUsers — все юзеры со взведённым флагом одним запросом (вместо
// точечного SELECT на каждого при каждом событии).
func (d *DB) ownerFlagUsers(ctx context.Context, col string) ([]int64, error) {
	rows, err := d.sql.QueryContext(ctx,
		fmt.Sprintf(`SELECT owner_id FROM owner_settings WHERE %s != 0`, col))
	if err != nil {
		return nil, fmt.Errorf("%s users: %w", col, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan %s user: %w", col, err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// setOwnerCol апсертит одну колонку owner_settings — общее тело Set*-тогглов
// и маркера отправки сводки.
func (d *DB) setOwnerCol(ctx context.Context, ownerID int64, col string, v any) error {
	_, err := d.sql.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO owner_settings (owner_id, %[1]s) VALUES (?, ?)
		ON CONFLICT(owner_id) DO UPDATE SET %[1]s = excluded.%[1]s
	`, col), ownerID, v)
	if err != nil {
		return fmt.Errorf("set %s: %w", col, err)
	}
	return nil
}

// LastStatsPeriod — последний выбранный юзером период статистики DM-меню;
// нет строки/NULL — "". Валидацию значения делает вызывающий (parsePeriod).
func (d *DB) LastStatsPeriod(ctx context.Context, userID int64) (string, error) {
	var p string
	err := d.sql.QueryRowContext(ctx,
		`SELECT COALESCE(last_stats_period, '') FROM owner_settings WHERE owner_id = ?`,
		userID).Scan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("last stats period: %w", err)
	}
	return p, nil
}

// SetLastStatsPeriod запоминает период статистики, выбранный юзером в DM-меню.
func (d *DB) SetLastStatsPeriod(ctx context.Context, userID int64, p string) error {
	return d.setOwnerCol(ctx, userID, "last_stats_period", p)
}

// SpamNotifyEnabled — включены ли у владельца ЛС-уведомления о спаме.
func (d *DB) SpamNotifyEnabled(ctx context.Context, ownerID int64) (bool, error) {
	return d.ownerFlagEnabled(ctx, ownerID, "spam_notify")
}

// SpamNotifyOwners — все владельцы с включёнными уведомлениями о спаме.
func (d *DB) SpamNotifyOwners(ctx context.Context) ([]int64, error) {
	return d.ownerFlagUsers(ctx, "spam_notify")
}

func (d *DB) SetSpamNotify(ctx context.Context, ownerID int64, on bool) error {
	return d.setOwnerCol(ctx, ownerID, "spam_notify", boolToInt(on))
}

// ModNotifyEnabled — включены ли у владельца ЛС-уведомления о киках/банах
// и проходах капчи.
func (d *DB) ModNotifyEnabled(ctx context.Context, ownerID int64) (bool, error) {
	return d.ownerFlagEnabled(ctx, ownerID, "mod_notify")
}

// ModNotifyOwners — все владельцы с включёнными уведомлениями о модерации.
func (d *DB) ModNotifyOwners(ctx context.Context) ([]int64, error) {
	return d.ownerFlagUsers(ctx, "mod_notify")
}

func (d *DB) SetModNotify(ctx context.Context, ownerID int64, on bool) error {
	return d.setOwnerCol(ctx, ownerID, "mod_notify", boolToInt(on))
}

// CaptchaNotifyEnabled — включены ли у владельца ЛС-уведомления о КАЖДОМ
// провале капчи (mod_notify шлёт провалы только со второй попытки).
func (d *DB) CaptchaNotifyEnabled(ctx context.Context, ownerID int64) (bool, error) {
	return d.ownerFlagEnabled(ctx, ownerID, "captcha_notify")
}

// CaptchaNotifyOwners — все владельцы с подпиской на все провалы капчи.
func (d *DB) CaptchaNotifyOwners(ctx context.Context) ([]int64, error) {
	return d.ownerFlagUsers(ctx, "captcha_notify")
}

func (d *DB) SetCaptchaNotify(ctx context.Context, ownerID int64, on bool) error {
	return d.setOwnerCol(ctx, ownerID, "captcha_notify", boolToInt(on))
}

// VersionNotifyEnabled — слать ли юзеру ЛС «бот обновлён». Единственный
// OPT-OUT тумблер owner_settings: НЕТ строки = ВКЛЮЧЕНО (колонка DEFAULT 1) —
// поэтому не ownerFlagEnabled, у которого дефолт false.
func (d *DB) VersionNotifyEnabled(ctx context.Context, userID int64) (bool, error) {
	var on int
	err := d.sql.QueryRowContext(ctx,
		`SELECT version_notify FROM owner_settings WHERE owner_id = ?`, userID).Scan(&on)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("version_notify enabled: %w", err)
	}
	return on != 0, nil
}

func (d *DB) SetVersionNotify(ctx context.Context, userID int64, on bool) error {
	return d.setOwnerCol(ctx, userID, "version_notify", boolToInt(on))
}

// VersionNotifyOptOuts — юзеры, ЯВНО выключившие оповещения о версиях:
// фильтр рассылки одним запросом (инверсия ownerFlagUsers — тумблер opt-out).
func (d *DB) VersionNotifyOptOuts(ctx context.Context) ([]int64, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT owner_id FROM owner_settings WHERE version_notify = 0`)
	if err != nil {
		return nil, fmt.Errorf("version_notify opt-outs: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan version_notify opt-out: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// DailyReportEnabled — включена ли у юзера утренняя ЛС-сводка по его чатам.
// В отличие от spam/mod_notify доступна не только владельцам, но и админам.
func (d *DB) DailyReportEnabled(ctx context.Context, userID int64) (bool, error) {
	return d.ownerFlagEnabled(ctx, userID, "daily_report")
}

func (d *DB) SetDailyReport(ctx context.Context, userID int64, on bool) error {
	return d.setOwnerCol(ctx, userID, "daily_report", boolToInt(on))
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
	return d.setOwnerCol(ctx, userID, "last_report_day", day)
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

// TakeGreetingMsg атомарно изымает запомненное приветствие юзера:
// DELETE ... RETURNING делает SELECT+DELETE одной инструкцией — двойной
// вызов (гонка /kick и спам-вердикта) больше не возвращает ok=true обоим.
func (d *DB) TakeGreetingMsg(ctx context.Context, chatID, userID int64) (int, bool, error) {
	var msgID int
	err := d.sql.QueryRowContext(ctx,
		`DELETE FROM greetings WHERE chat_id = ? AND user_id = ? RETURNING message_id`,
		chatID, userID).Scan(&msgID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("take greeting: %w", err)
	}
	return msgID, true, nil
}

// GetGreetingMsg возвращает message_id приветствия БЕЗ удаления строки.
// Нужен, чтобы снять клавиатуру после ответа юзера, сохранив запись для
// будущей очистки (cleanupTargetTraces при спам-бане).
func (d *DB) GetGreetingMsg(ctx context.Context, chatID, userID int64) (int, bool, error) {
	var msgID int
	err := d.sql.QueryRowContext(ctx,
		`SELECT message_id FROM greetings WHERE chat_id = ? AND user_id = ?`,
		chatID, userID).Scan(&msgID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("get greeting: %w", err)
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

// DeleteChatSpamVotes сносит все голосования и бюллетени чата. Вызывается,
// когда бот покидает чат (dropChat): оставшаяся плашка жила бы до суточного
// свипа, и золотой голос в мёртвом чате выдал бы глобальный бан. Транзакция —
// как в TakeSpamVote: краш посреди двух DELETE не должен оставлять сирот.
func (d *DB) DeleteChatSpamVotes(ctx context.Context, chatID int64) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete chat spam votes: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // откат после успешного Commit — no-op
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM spam_ballots WHERE chat_id = ?`, chatID); err != nil {
		return fmt.Errorf("delete chat spam ballots: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM spam_votes WHERE chat_id = ?`, chatID); err != nil {
		return fmt.Errorf("delete chat spam votes: %w", err)
	}
	return tx.Commit()
}
