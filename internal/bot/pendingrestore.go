package bot

import (
	"context"
	"fmt"
	"time"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/menand/AntiSpamBot/internal/storage"
)

// Повторяем стартовую сверку до приёма апдейтов. Каждый проход проверяет
// доступность всех своих чатов до запуска таймеров, поэтому при повторе
// новые серии не пересекутся с уже восстановленными или живыми апдейтами.
func (b *Bot) retryPendingRestore(ctx context.Context, name string, restore func(context.Context) (int, error)) (int, error) {
	for {
		count, err := restore(ctx)
		if err == nil {
			return count, nil
		}
		b.log.Warn("retry pending restore", "kind", name, "err", err)
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0, ctx.Err()
		case <-timer.C:
		}
	}
}

func (b *Bot) checkPendingRestoreChat(ctx context.Context, chatID int64, serviceable map[int64]bool) error {
	if _, checked := serviceable[chatID]; checked {
		return nil
	}
	allowed, err := b.chatServiceableChecked(ctx, chatID)
	if err != nil {
		return fmt.Errorf("restore checks in chat %d: %w", chatID, err)
	}
	if !b.chatAllowed(chatID) {
		if err := b.db.SuspendPendingChecks(ctx, chatID); err != nil {
			return fmt.Errorf("suspend checks in chat %d: %w", chatID, err)
		}
	}
	serviceable[chatID] = allowed
	return nil
}

func (b *Bot) captchaRestoreLoop(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		b.reconcileCaptchaRestorations(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (b *Bot) reconcileCaptchaRestorations(ctx context.Context) {
	rows, err := b.db.LoadCaptchaRestorations(ctx)
	if err != nil {
		b.log.Warn("load unfinished captcha restorations", "err", err)
		return
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}
		budget, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := b.restoreCaptchaCheckpoint(budget, row)
		cancel()
		if err != nil {
			b.log.Warn("retry captcha restoration", "err", err, "chat", row.ChatID, "user", row.UserID)
		}
	}
}

func (b *Bot) restoreCaptchaCheckpoint(ctx context.Context, snapshot storage.PendingRow) error {
	chatID, userID := snapshot.ChatID, snapshot.UserID
	unlock, err := b.rights.lock(ctx, chatID, userID, false)
	if err != nil {
		return err
	}
	defer unlock()
	row, found, err := b.db.GetPendingCaptcha(ctx, chatID, userID)
	if err != nil || !found || row != snapshot {
		return err
	}
	allowed, err := b.chatServiceableChecked(ctx, chatID)
	if err != nil || !allowed {
		return err
	}
	if _, pending, err := b.db.GetManualMute(ctx, chatID, userID); err != nil || pending {
		return err
	}
	if _, pending, err := b.db.GetPunishment(ctx, chatID, userID); err != nil || pending {
		return err
	}
	if row.Preparing() {
		return b.restoreCaptchaPreparation(ctx, row)
	}
	if row.Suspended {
		return b.finishSuspendedCaptcha(ctx, row)
	}
	return nil
}

func (b *Bot) finishSuspendedCaptcha(ctx context.Context, row storage.PendingRow) error {
	member, err := b.api.GetChatMember(ctx, &telego.GetChatMemberParams{
		ChatID: tu.ID(row.ChatID),
		UserID: row.UserID,
	})
	if err != nil {
		return err
	}
	kind := storage.EventAbort
	if member.MemberStatus() == telego.MemberStatusLeft || member.MemberStatus() == telego.MemberStatusBanned {
		kind = storage.EventLeft
	}
	// Старые капчи были бессрочными. Новые узнаём по записанному сроку.
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
	if err := b.db.FinishSuspendedCaptcha(ctx, row, kind); err != nil {
		return err
	}
	if err := b.deleteBotMessage(ctx, row.ChatID, row.MessageID, row.EphemeralID, row.UserID); err != nil {
		b.log.Debug("delete suspended captcha message", "err", err, "chat", row.ChatID, "user", row.UserID)
	}
	return nil
}

func isCaptchaRestriction(m *telego.ChatMemberRestricted, until int64) bool {
	return m.UntilDate == until && !m.CanSendMessages && !m.CanSendAudios &&
		!m.CanSendDocuments && !m.CanSendPhotos && !m.CanSendVideos &&
		!m.CanSendVideoNotes && !m.CanSendVoiceNotes && !m.CanSendPolls &&
		!m.CanSendOtherMessages && !m.CanAddWebPagePreviews &&
		!m.CanChangeInfo && !m.CanInviteUsers && !m.CanPinMessages && !m.CanManageTopics
}
