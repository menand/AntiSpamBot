package bot

import (
	"context"
	"crypto/rand"
	"time"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/menand/AntiSpamBot/internal/storage"
)

func captchaRestrictionDeadline(now time.Time, delay, stageInterval time.Duration) time.Time {
	// Запас на отправки и ретраи. Максимум оставляет сутки до порога
	// Telegram в 366 дней. Ограничиваем слагаемые до сложения.
	const maxDuration = 365 * 24 * time.Hour
	const retryMargin = 2 * time.Minute
	d := min(max(delay, 0), maxDuration) + captchaStages*min(max(stageInterval, 0), maxDuration) + retryMargin
	return now.Add(min(d, maxDuration)).Truncate(time.Second)
}

func (b *Bot) newCaptchaPreparation(ctx context.Context, chatID, userID int64, threadID int, settings storage.ChatSettings) (storage.PendingRow, error) {
	deadline := captchaRestrictionDeadline(time.Now(), b.cfg.CaptchaDelay, b.effectiveStageInterval(settings))
	row := storage.PendingRow{
		ChatID: chatID, UserID: userID, ThreadID: threadID,
		Generation: rand.Text(), RestrictUntil: deadline.Unix(), ExpiresAt: deadline,
	}
	return row, b.db.BeginCaptchaPreparation(ctx, row, time.Now())
}

// restoreCaptchaPreparation закрывает прерванную подготовку. Неизвестное
// сообщение могло уже уйти, поэтому не перевыпускаем его и не армируем таймер.
func (b *Bot) restoreCaptchaPreparation(ctx context.Context, row storage.PendingRow) error {
	kind := storage.EventAbort
	if row.RestrictUntil > time.Now().Unix() {
		member, err := b.api.GetChatMember(ctx, &telego.GetChatMemberParams{
			ChatID: tu.ID(row.ChatID), UserID: row.UserID,
		})
		if err != nil {
			return err
		}
		if !member.MemberIsMember() {
			kind = storage.EventLeft
		}
		if m, ok := member.(*telego.ChatMemberRestricted); ok && isCaptchaRestriction(m, row.RestrictUntil) {
			quarantined, err := b.db.HasQuarantine(ctx, row.ChatID, row.UserID, time.Now().Unix())
			if err != nil {
				return err
			}
			if !quarantined {
				perms, err := b.fetchChatDefaultPermissions(ctx, row.ChatID)
				if err != nil {
					return err
				}
				if err := b.releaseWithPermissions(ctx, row.ChatID, row.UserID, perms); err != nil {
					return err
				}
			}
		}
	}
	return b.db.FinishCaptchaPreparation(ctx, row, kind)
}

// abortCaptchaPreparation вызывается после подтверждённого restrict.
// Строку сохраняем, если снятие мута не удалось.
func (b *Bot) abortCaptchaPreparation(row storage.PendingRow, kind storage.EventKind, kickoff context.Context) {
	if kickoff.Err() != nil {
		return
	}
	base, cancel := b.liveCtx()
	defer cancel()
	ctx, stop := context.WithCancel(base)
	defer stop()
	unlink := context.AfterFunc(kickoff, stop)
	defer unlink()
	if err := b.release(ctx, row.ChatID, row.UserID); err != nil {
		b.log.Warn("release aborted captcha preparation", "err", err, "chat", row.ChatID, "user", row.UserID)
		return
	}
	if kickoff.Err() != nil {
		return
	}
	if err := b.db.FinishCaptchaPreparation(ctx, row, kind); err != nil {
		b.log.Warn("finish aborted captcha preparation", "err", err, "chat", row.ChatID, "user", row.UserID)
	}
}
