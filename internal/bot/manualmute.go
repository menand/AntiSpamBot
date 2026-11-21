package bot

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/menand/AntiSpamBot/internal/storage"
)

var errMuteUnconfirmed = errors.New("manual mute awaits reconciliation")

// Telegram считает остаток меньше 30 секунд бессрочным. Запас учитывает
// округление Unix time и задержку запроса у сервера.
const minManualMuteRemaining = 35 * time.Second

func (b *Bot) applyManualMute(ctx context.Context, chatID, userID, adminID int64, until time.Time) error {
	previous, _, err := b.db.GetManualMute(ctx, chatID, userID)
	if err != nil {
		return err
	}
	m := storage.ManualMute{
		ChatID: chatID, UserID: userID, Generation: rand.Text(), UntilDate: until.Unix(),
		Reason: storage.ReasonModPrefix + fmt.Sprintf("%d", adminID),
	}
	if err := b.db.PutManualMute(ctx, m); err != nil {
		return err
	}
	rejected, err := b.requestManualMute(ctx, m)
	if err == nil {
		if finishErr := b.finishManualMute(ctx, m, true, false); finishErr != nil {
			return fmt.Errorf("%w: %w", errMuteUnconfirmed, finishErr)
		}
		return nil
	}
	if rejected {
		cleanup, cancel := b.liveCtx()
		defer cancel()
		if rollbackErr := b.db.RejectManualMute(cleanup, m, previous); rollbackErr == nil {
			return err
		} else {
			err = errors.Join(err, rollbackErr)
		}
	}
	return fmt.Errorf("%w: %w", errMuteUnconfirmed, err)
}

// requestManualMute отличает отказ без возможного применения от обрыва
// ответа. Поздний 4xx не доказывает, что предыдущий запрос не применился.
func (b *Bot) requestManualMute(ctx context.Context, m storage.ManualMute) (rejected bool, _ error) {
	deadline := time.Unix(m.UntilDate, 0).Add(-minManualMuteRemaining)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	var uncertain bool
	var denied error
	err := retryTG(ctx, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := b.restrictUntil(ctx, m.ChatID, m.UserID, m.UntilDate)
		if err == nil {
			return nil
		}
		var apiErr *telegoapi.Error
		if errors.As(err, &apiErr) && apiErr.ErrorCode >= 400 && apiErr.ErrorCode < 500 && apiErr.ErrorCode != 408 {
			if apiErr.ErrorCode == 429 {
				return err
			}
			denied = err
			return nil // подтверждённый отказ, ретраи его не исправят
		}
		uncertain = true
		return err
	})
	return !uncertain, errors.Join(denied, err)
}

func (b *Bot) finishManualMute(ctx context.Context, m storage.ManualMute, confirmed, keepQuarantine bool) error {
	chatID, userID := m.ChatID, m.UserID
	_, hadCaptcha := b.store.Get(chatID, userID)
	_, hadReply := b.replies.Get(chatID, userID)
	card, finished, err := b.db.FinishManualMute(ctx, m, hadCaptcha || hadReply, confirmed, keepQuarantine)
	if err != nil || !finished {
		return err
	}
	b.store.CancelKickoff(chatID, userID)
	p, tookCaptcha := b.store.Take(chatID, userID)
	if tookCaptcha {
		p.Cancel()
	}
	if reply, ok := b.replies.Take(chatID, userID); ok {
		reply.Cancel()
	}
	if !keepQuarantine {
		b.quar.Remove(chatID, userID)
	}
	if card.MessageID == 0 && card.EphemeralID == 0 && tookCaptcha {
		card.MessageID, card.EphemeralID = p.MessageID, p.EphemeralID
	}
	if card.MessageID != 0 || card.EphemeralID != 0 {
		if err := b.deleteBotMessage(ctx, chatID, card.MessageID, card.EphemeralID, userID); err != nil {
			b.log.Debug("delete muted user's captcha", "err", err, "chat", chatID, "user", userID)
		}
	}
	return nil
}

func (b *Bot) reconcileManualMute(ctx context.Context, snapshot storage.ManualMute) error {
	unlock, err := b.rights.lock(ctx, snapshot.ChatID, snapshot.UserID, false)
	if err != nil {
		return err
	}
	defer unlock()
	m, found, err := b.db.GetManualMute(ctx, snapshot.ChatID, snapshot.UserID)
	if err != nil || !found || m.Generation != snapshot.Generation {
		return err
	}
	member, err := b.api.GetChatMember(ctx, &telego.GetChatMemberParams{ChatID: tu.ID(m.ChatID), UserID: m.UserID})
	if err != nil {
		return err
	}
	if !member.MemberIsMember() || member.MemberStatus() == telego.MemberStatusAdministrator || member.MemberStatus() == telego.MemberStatusCreator {
		return b.finishManualMute(ctx, m, false, false)
	}
	short := time.Until(time.Unix(m.UntilDate, 0)) < minManualMuteRemaining
	if restricted, ok := member.(*telego.ChatMemberRestricted); ok {
		if isCaptchaRestriction(restricted, m.UntilDate) {
			return b.finishManualMute(ctx, m, true, false)
		}
		q, err := b.db.ActiveQuarantine(ctx, m.ChatID, m.UserID, time.Now().Unix())
		if err != nil {
			return err
		}
		isQuarantine := !q.UntilAt.IsZero() && isTextOnlyQuarantine(restricted, q.UntilAt.Unix())
		ownedCaptcha := false
		if isCaptchaRestriction(restricted, restricted.UntilDate) {
			ownedCaptcha, err = b.db.HasPendingCaptchaRestriction(ctx, m.ChatID, m.UserID, restricted.UntilDate)
			if err != nil {
				return err
			}
		}
		if short || (!ownedCaptcha && !isQuarantine) {
			return b.finishAbandonedManualMute(ctx, m, restricted.UntilDate, ownedCaptcha, isQuarantine, q)
		}
	}
	if short {
		return b.finishManualMute(ctx, m, false, false)
	}
	if _, err := b.requestManualMute(ctx, m); err != nil {
		return err
	}
	return b.finishManualMute(ctx, m, true, false)
}

func (b *Bot) finishAbandonedManualMute(ctx context.Context, m storage.ManualMute, restrictionUntil int64, ownedCaptcha, isQuarantine bool, q storage.QuarantineRow) error {
	if ownedCaptcha {
		perms, err := b.fetchChatDefaultPermissions(ctx, m.ChatID)
		if err != nil {
			return err
		}
		member, err := b.api.GetChatMember(ctx, &telego.GetChatMemberParams{ChatID: tu.ID(m.ChatID), UserID: m.UserID})
		if err != nil {
			return err
		}
		if live, ok := member.(*telego.ChatMemberRestricted); !ok || !live.IsMember || !isCaptchaRestriction(live, restrictionUntil) {
			return fmt.Errorf("captcha restriction changed before release")
		}
		if err := b.releaseWithPermissions(ctx, m.ChatID, m.UserID, perms); err != nil && !isUserNotParticipant(err) {
			return err
		}
	}
	if !isQuarantine {
		return b.finishManualMute(ctx, m, false, false)
	}
	s, err := b.db.GetChatSettings(ctx, m.ChatID)
	if err != nil {
		return err
	}
	if err := b.finishManualMute(ctx, m, false, true); err != nil {
		return err
	}
	if s.QuarantineEnabled {
		b.quar.Set(m.ChatID, m.UserID, q.UntilAt, q.Warned)
	} else {
		b.quar.Remove(m.ChatID, m.UserID)
	}
	return nil
}

func (b *Bot) reconcileManualMutes(ctx context.Context) {
	rows, err := b.db.LoadManualMutes(ctx)
	if err != nil {
		b.log.Warn("load manual mute intents", "err", err)
		return
	}
	for _, m := range rows {
		if ctx.Err() != nil {
			return
		}
		allowed, err := b.chatServiceableChecked(ctx, m.ChatID)
		if err != nil || !allowed {
			continue
		}
		budget, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = b.reconcileManualMute(budget, m)
		cancel()
		if err != nil {
			b.log.Warn("reconcile manual mute", "err", err, "chat", m.ChatID, "user", m.UserID)
		}
	}
}

func (b *Bot) manualMuteRetryLoop(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.reconcileManualMutes(ctx)
			b.reconcilePunishments(ctx)
		}
	}
}

func (b *Bot) manualMuteKeys(ctx context.Context) (map[chatUser]bool, error) {
	rows, err := b.db.LoadManualMutes(ctx)
	if err != nil {
		return nil, err
	}
	keys := make(map[chatUser]bool, len(rows))
	for _, m := range rows {
		keys[chatUser{m.ChatID, m.UserID}] = true
	}
	return keys, nil
}

// Проверки в уже работающем процессе ждут решения и сохраняют исходные
// дедлайны. Перехватываем очередь заново, чтобы не держать её во время сна.
func (b *Bot) lockCheckRights(ctx context.Context, chatID, userID int64, done <-chan struct{}) (func(), bool) {
	const interval = time.Second
	for {
		unlock, err := b.rights.lock(ctx, chatID, userID, false)
		if err != nil {
			return nil, false
		}
		if err := b.checkManualMute(ctx, chatID, userID); err == nil {
			return unlock, true
		}
		unlock()
		timer := time.NewTimer(interval)
		select {
		case <-timer.C:
		case <-done:
			timer.Stop()
			return nil, false
		case <-ctx.Done():
			timer.Stop()
			return nil, false
		}
	}
}

func (b *Bot) checkManualMute(ctx context.Context, chatID, userID int64) error {
	_, pending, err := b.db.GetManualMute(ctx, chatID, userID)
	if err == nil && pending {
		return errMuteUnconfirmed
	}
	return err
}
