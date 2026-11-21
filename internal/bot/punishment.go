package bot

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/menand/AntiSpamBot/internal/storage"
)

const kickBanDuration = 10 * time.Minute

func (b *Bot) beginPunishment(ctx context.Context, chatID, userID int64,
	reason, detail string, limit int,
) (storage.PendingPunishment, storage.EventKind, error) {
	p, fresh, err := b.db.BeginPunishment(ctx, storage.PendingPunishment{
		ChatID: chatID, UserID: userID, Generation: rand.Text(), Reason: reason, Detail: detail,
		BanUntil: time.Now().Add(kickBanDuration).Unix(),
	}, limit, attemptsTTL)
	if err != nil {
		return p, "", err
	}
	if !fresh && p.Reason != reason {
		return p, "", fmt.Errorf("another punishment is pending")
	}
	kind, err := b.advancePunishment(ctx, p, !fresh)
	return p, kind, err
}

func (b *Bot) decideAutomaticPunishment(ctx context.Context, chatID, userID int64,
	reason, detail string,
) (storage.PendingPunishment, storage.EventKind, error) {
	p := storage.PendingPunishment{
		ChatID: chatID, UserID: userID, Generation: rand.Text(), Reason: reason, Detail: detail,
	}
	member, err := b.api.GetChatMember(ctx, &telego.GetChatMemberParams{ChatID: tu.ID(chatID), UserID: userID})
	if err != nil {
		// Сохраняем причину, включая неправильный ответ, но не считаем
		// попытку до подтверждения членства и роли.
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		savedErr := b.db.DeferPunishment(saveCtx, p)
		return p, "", errors.Join(err, savedErr)
	}
	switch {
	case member.MemberStatus() == telego.MemberStatusBanned:
		kind, err := b.closeCheckWithoutPunishment(ctx, chatID, userID, "")
		return p, kind, err
	case !member.MemberIsMember():
		kind, err := b.closeCheckWithoutPunishment(ctx, chatID, userID, storage.EventLeft)
		return p, kind, err
	case member.MemberStatus() == telego.MemberStatusAdministrator || member.MemberStatus() == telego.MemberStatusCreator:
		b.releaseDetached(chatID, userID)
		kind, err := b.closeCheckWithoutPunishment(ctx, chatID, userID, storage.EventPass)
		return p, kind, err
	default:
		return b.beginPunishment(ctx, chatID, userID, reason, detail,
			b.effectiveMaxAttempts(b.chatSettings(ctx, chatID)))
	}
}

func (b *Bot) closeCheckWithoutPunishment(ctx context.Context, chatID, userID int64, kind storage.EventKind) (storage.EventKind, error) {
	p, exists, err := b.db.GetPunishment(ctx, chatID, userID)
	if err != nil {
		return "", err
	}
	if exists {
		return b.finishPunishment(ctx, p, kind)
	}
	if kind != "" {
		return kind, b.db.RecordEvent(ctx, chatID, userID, kind, time.Now(), "")
	}
	return "", nil
}

// advancePunishment вызывается из очереди прав участника. При восстановлении
// принадлежность временного бана подтверждает его точный серверный срок.
func (b *Bot) advancePunishment(ctx context.Context, p storage.PendingPunishment, recovering bool) (storage.EventKind, error) {
	chatID, userID := p.ChatID, p.UserID
	manual := p.Kind == storage.EventKick && p.Attempt == 0
	if !manual {
		global, err := b.db.IsSpamBannedInChat(ctx, chatID, userID)
		if err != nil {
			return "", err
		}
		if global {
			return b.finishPunishment(ctx, p, "")
		}
	}

	confirmed := p.BanConfirmed
	if recovering || manual {
		member, err := b.api.GetChatMember(ctx, &telego.GetChatMemberParams{ChatID: tu.ID(chatID), UserID: userID})
		if err != nil {
			return "", err
		}
		switch m := member.(type) {
		case *telego.ChatMemberBanned:
			if !recovering || p.Kind != storage.EventKick || p.BanUntil == 0 || m.UntilDate != p.BanUntil {
				// Чужой или более новый бан. Статуса kicked для разбана
				// недостаточно, включая старые бессрочные кики.
				if manual {
					return "", b.db.DeletePunishment(ctx, p)
				}
				return b.finishPunishment(ctx, p, "")
			}
			confirmed = true
		case *telego.ChatMemberAdministrator, *telego.ChatMemberOwner:
			if manual {
				return "", b.db.DeletePunishment(ctx, p)
			}
			kind := storage.EventKind("")
			if p.Attempt > 0 {
				kind = storage.EventPass
			}
			return b.finishPunishment(ctx, p, kind)
		default:
			if confirmed {
				// Разбан успел примениться, либо конечный срок уже истёк.
				return b.finishPunishment(ctx, p, storage.EventKick)
			}
			if !member.MemberIsMember() {
				if manual {
					return "", b.db.DeletePunishment(ctx, p)
				}
				return b.finishPunishment(ctx, p, storage.EventLeft)
			}
		}
	}

	if p.Kind == storage.EventBan {
		if err := b.banShort(ctx, chatID, userID); err != nil {
			return "", err
		}
		return b.finishPunishment(ctx, p, p.Kind)
	}

	if !confirmed {
		if time.Until(time.Unix(p.BanUntil, 0)) < minManualMuteRemaining {
			if manual {
				return "", b.db.DeletePunishment(ctx, p)
			}
			return b.finishPunishment(ctx, p, storage.EventAbort)
		}
		if err := b.banForKick(ctx, p); err != nil {
			return "", err
		}
	}
	if !p.BanConfirmed {
		if err := b.db.MarkPunishmentBanConfirmed(ctx, p); err != nil {
			return "", err
		}
		if p.Attempt == 0 {
			b.markReplyWatchesDeletedByUser(chatID, userID, replyWatchDeletedByKick)
		}
	}
	if time.Now().Unix() < p.BanUntil {
		if !confirmed {
			// Между ответом на ban и unban админ мог сменить
			// ограничение напрямую в Telegram.
			member, err := b.api.GetChatMember(ctx, &telego.GetChatMemberParams{ChatID: tu.ID(chatID), UserID: userID})
			if err != nil {
				return "", err
			}
			if restriction, ok := member.(*telego.ChatMemberBanned); ok && restriction.UntilDate != p.BanUntil {
				return b.finishPunishment(ctx, p, "")
			}
		}
		if !manual {
			global, err := b.db.IsSpamBannedInChat(ctx, chatID, userID)
			if err != nil {
				return "", err
			}
			if global {
				return b.finishPunishment(ctx, p, "")
			}
		}
		if err := b.unban(ctx, chatID, userID); err != nil {
			return "", err
		}
	}
	return b.finishPunishment(ctx, p, storage.EventKick)
}

func (b *Bot) banForKick(ctx context.Context, p storage.PendingPunishment) error {
	ctx, cancel := context.WithDeadline(ctx, time.Unix(p.BanUntil, 0).Add(-minManualMuteRemaining))
	defer cancel()
	backoffs := kickUnbanBackoffs
	if p.Attempt == 0 {
		backoffs = tgBackoffs
	}
	return retryWith(ctx, backoffs, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return b.api.BanChatMember(ctx, &telego.BanChatMemberParams{
			ChatID: tu.ID(p.ChatID), UserID: p.UserID, UntilDate: p.BanUntil, RevokeMessages: p.Attempt == 0,
		})
	})
}

func (b *Bot) finishPunishment(ctx context.Context, p storage.PendingPunishment, kind storage.EventKind) (storage.EventKind, error) {
	finished, err := b.db.FinishPunishment(ctx, p, kind)
	if err != nil {
		return "", err
	}
	if !finished {
		return "", fmt.Errorf("punishment generation changed")
	}
	return kind, nil
}

func (b *Bot) reconcilePunishments(ctx context.Context) {
	rows, err := b.db.LoadPunishments(ctx)
	if err != nil {
		b.log.Warn("load pending punishments", "err", err)
		return
	}
	for _, saved := range rows {
		if ctx.Err() != nil {
			return
		}
		allowed, err := b.chatServiceableChecked(ctx, saved.ChatID)
		if err != nil || !allowed {
			continue
		}
		budget, cancel := context.WithTimeout(ctx, 15*time.Second)
		err = b.reconcilePunishment(budget, saved)
		cancel()
		if err != nil && !errors.Is(err, errMuteUnconfirmed) {
			b.log.Warn("reconcile punishment", "err", err, "chat", saved.ChatID, "user", saved.UserID)
		}
	}
}

func (b *Bot) reconcilePunishment(ctx context.Context, saved storage.PendingPunishment) error {
	unlock, err := b.rights.lock(ctx, saved.ChatID, saved.UserID, false)
	if err != nil {
		return err
	}
	defer unlock()
	if err := b.checkManualMute(ctx, saved.ChatID, saved.UserID); err != nil {
		return err
	}
	p, exists, err := b.db.GetPunishment(ctx, saved.ChatID, saved.UserID)
	if err != nil || !exists || p.Generation != saved.Generation {
		return err
	}
	var kind storage.EventKind
	if p.Kind == "" {
		p, kind, err = b.decideAutomaticPunishment(ctx, p.ChatID, p.UserID, p.Reason, p.Detail)
	} else {
		kind, err = b.advancePunishment(ctx, p, true)
	}
	if err != nil || (kind != storage.EventKick && kind != storage.EventBan) {
		return err
	}
	switch {
	case p.Reason == storage.ReasonCaptcha:
		b.notifyCaptchaFail(p.ChatID, p.UserID, kind, p.Detail, p.Attempt)
	case strings.HasPrefix(p.Reason, storage.ReasonModPrefix):
		b.cleanupTargetTraces(p.ChatID, p.UserID)
		b.notifyModAction(p.ChatID, p.UserID, kind, p.Reason)
	default:
		b.notifyModAction(p.ChatID, p.UserID, kind, p.Reason)
	}
	return nil
}

func (b *Bot) supersedePunishment(ctx context.Context, chatID, userID int64) {
	p, exists, err := b.db.GetPunishment(ctx, chatID, userID)
	if err == nil && exists {
		_, err = b.db.FinishPunishment(ctx, p, "")
	}
	if err != nil {
		b.log.Warn("supersede punishment", "err", err, "chat", chatID, "user", userID)
	}
}

// Только решение, ещё не дошедшее до Telegram, можно снять по самому
// апдейту выхода. У начатого кика может оставаться своя фаза разбана.
func (b *Bot) retireDeferredOnDeparture(chatID, userID int64) {
	ctx := b.runCtx
	p, exists, err := b.db.GetPunishment(ctx, chatID, userID)
	if err != nil || !exists || p.Kind != "" {
		return
	}
	unlock, err := b.rights.lock(ctx, chatID, userID, false)
	if err != nil {
		return
	}
	defer unlock()
	current, exists, err := b.db.GetPunishment(ctx, chatID, userID)
	if err != nil || !exists || current.Generation != p.Generation || current.Kind != "" {
		return
	}
	if _, err := b.finishPunishment(ctx, current, storage.EventLeft); err != nil {
		b.log.Warn("close deferred punishment on departure", "err", err, "chat", chatID, "user", userID)
	}
}
