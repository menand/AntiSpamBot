package bot

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/menand/AntiSpamBot/internal/storage"
)

func (b *Bot) acceptTalliedSpamVerdict(v storage.SpamVote, margin int) {
	_, _ = b.acceptSpamVerdict(v, func(yes, no int) (bool, string, bool) {
		spam, decided := voteVerdict(yes, no, margin)
		return spam, fmt.Sprintf("голоса %d:%d", yes, no), decided
	})
}

func (b *Bot) acceptSpamVerdict(v storage.SpamVote, decide func(yes, no int) (bool, string, bool)) (bool, error) {
	id, err := b.db.AcceptSpamVerdict(b.runCtx, v.ChatID, v.BotMsgID, decide)
	if err != nil || id == 0 {
		if err != nil {
			b.log.Warn("accept spam verdict", "err", err, "chat", v.ChatID, "message", v.BotMsgID)
		}
		return false, err
	}
	b.applySpamVerdict(b.runCtx, id, v.ChatID, v.AuthorID)
	return true, nil
}

func (b *Bot) spamVerdictRetryLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.resumeSpamVerdicts(ctx)
		}
	}
}

func (b *Bot) resumeSpamVerdicts(ctx context.Context) {
	pending, err := b.db.ListSpamVerdicts(ctx)
	if err != nil {
		b.log.Warn("list accepted spam verdicts", "err", err)
		return
	}
	for _, v := range pending {
		if current, ready := b.prepareSpamVerdict(ctx, v.ID, v.ChatID, v.AuthorID); ready {
			b.completeSpamVerdict(ctx, current)
		}
	}
}

func (b *Bot) applySpamVerdict(ctx context.Context, id, chatID, userID int64) {
	v, ready := b.prepareSpamVerdict(ctx, id, chatID, userID)
	if !ready {
		return
	}
	if v.Outcome == storage.SpamBanApplied {
		b.goSafe("spamVerdictFanout", func() { b.completeSpamVerdict(b.runCtx, v) })
		return
	}
	b.completeSpamVerdict(ctx, v)
}

func (b *Bot) notifyDirectSpamResult(v storage.SpamVote, banned bool, verdictID int64, why string) {
	if verdictID != 0 {
		b.applySpamVerdict(b.runCtx, verdictID, v.ChatID, v.AuthorID)
		return
	}
	targets := b.spamNotifyTargets(b.runCtx)
	b.goSafe("directSpamNotice", func() { b.notifySpamVerdict(targets, v, true, banned, why, nil, nil) })
}

func (b *Bot) prepareSpamVerdict(ctx context.Context, id, chatID, userID int64) (storage.SpamVerdict, bool) {
	unlock, err := b.rights.lock(ctx, chatID, userID, false)
	if err != nil {
		return storage.SpamVerdict{}, false
	}
	defer unlock()

	v, found, err := b.db.SpamVerdict(ctx, id)
	if err != nil || !found || v.ChatID != chatID {
		if err != nil {
			b.log.Warn("load accepted spam verdict", "err", err, "id", id)
		}
		return v, false
	}
	serviceable, err := b.chatServiceableChecked(ctx, chatID)
	if err != nil {
		b.log.Warn("check chat for accepted spam verdict", "err", err, "id", id)
		return v, false
	}
	if v.IsSpam {
		current, err := b.db.SpamBanStillCurrent(ctx, chatID, chatID, userID, id)
		if err != nil {
			b.log.Warn("check pardon before spam verdict", "err", err, "id", id)
			return v, false
		}
		if !current {
			if serviceable && v.BotMsgID > 0 {
				if err := b.deleteMessage(ctx, chatID, v.BotMsgID); err != nil {
					b.log.Debug("delete pardoned vote message", "err", err, "id", id)
				}
			}
			if err := b.db.CancelSpamVerdict(ctx, id); err != nil {
				b.log.Warn("cancel pardoned spam verdict", "err", err, "id", id)
			}
			return v, false
		}
	}

	if v.Outcome == "" {
		outcome := b.executeAcceptedSpamVerdict(ctx, v, serviceable)
		if outcome == "" {
			return v, false
		}
		reason := v.BanReason
		if reason == "" {
			reason = voteReason(v.Ballots)
		}
		saved, err := b.db.RecordSpamOutcome(ctx, v, outcome, time.Now(), reason)
		if err != nil || !saved {
			if err != nil {
				b.log.Warn("record accepted spam outcome", "err", err, "id", id)
			}
			return v, false
		}
		v.Outcome = outcome
	}

	banned := v.Outcome == storage.SpamBanApplied
	if serviceable && !v.FanoutReady {
		b.finishSpamVoteMessage(ctx, v)
		if banned {
			b.cleanUpAppliedSpamBan(v.SpamVote)
		}
		if banned || v.Outcome == storage.SpamBanDenied {
			b.deleteSpamTarget(v.SpamVote)
		}
	}
	if banned && !v.FanoutReady {
		var chatIDs []int64
		if serviceable {
			chats, err := b.db.ListChats(ctx)
			if err != nil {
				b.log.Warn("list cross-ban targets", "err", err, "id", v.ID)
				return v, false
			}
			for _, chat := range chats {
				if chat.ChatID == chatID || (chat.Type != "group" && chat.Type != "supergroup") {
					continue
				}
				allowed, err := b.chatServiceableChecked(ctx, chat.ChatID)
				if err != nil {
					b.log.Warn("check cross-ban target", "err", err, "id", v.ID, "chat", chat.ChatID)
					return v, false
				}
				if allowed {
					chatIDs = append(chatIDs, chat.ChatID)
				}
			}
		}
		if err := b.db.InitSpamFanout(ctx, v.ID, chatIDs); err != nil {
			b.log.Warn("save cross-ban targets", "err", err, "id", v.ID)
			return v, false
		}
		v.FanoutReady = true
	}
	return v, true
}

func (b *Bot) completeSpamVerdict(ctx context.Context, v storage.SpamVerdict) {
	banned := v.Outcome == storage.SpamBanApplied
	var alsoBanned []string
	if banned {
		var err error
		alsoBanned, err = b.banEverywhere(ctx, v)
		if err != nil {
			b.log.Warn("resume cross-ban", "err", err, "id", v.ID)
			return
		}
	}
	finished, err := b.db.FinishSpamVerdict(ctx, v.ID, v.ChatID)
	if err != nil || !finished {
		if err != nil {
			b.log.Warn("finish accepted spam verdict", "err", err, "id", v.ID)
		}
		return
	}
	if v.Outcome == storage.SpamUnavailable {
		b.log.Info("accepted spam verdict closed after chat approval was removed", "id", v.ID, "chat", v.ChatID)
		return
	}
	targets := b.spamNotifyTargets(ctx)
	b.goSafe("spamVerdictNotice", func() { b.notifySpamVerdict(targets, v.SpamVote, v.IsSpam, banned, v.Why, v.Ballots, alsoBanned) })
}

func (b *Bot) executeAcceptedSpamVerdict(ctx context.Context, v storage.SpamVerdict, serviceable bool) storage.SpamOutcome {
	if !serviceable {
		return storage.SpamUnavailable
	}
	if !v.IsSpam {
		return storage.SpamCleared
	}
	if v.BanReason != "" {
		applied, err := b.permanentChatBan(ctx, v.ChatID, v.AuthorID)
		if err != nil {
			b.log.Warn("check direct spam ban before recovery", "err", err, "id", v.ID)
			return ""
		}
		if applied {
			if err := b.retireSpamBanAdmission(ctx, v.ChatID, v.AuthorID); err != nil {
				b.log.Warn("retire admission after recovered spam ban", "err", err, "id", v.ID)
				return ""
			}
			return storage.SpamBanApplied
		}
	}
	if err := b.banRevoke(ctx, v.ChatID, v.AuthorID); err != nil {
		b.log.Warn("accepted spam verdict ban", "err", err, "chat", v.ChatID, "user", v.AuthorID)
		if v.BanReason != "" && definitiveBanRejection(err) {
			return storage.SpamBanDenied
		}
		if errors.Is(err, errBanUnconfirmed) {
			return ""
		}
		var apiErr *telegoapi.Error
		if errors.As(err, &apiErr) && (apiErr.ErrorCode == 400 || apiErr.ErrorCode == 401 || apiErr.ErrorCode == 403 || apiErr.ErrorCode == 404) {
			return storage.SpamBanDenied
		}
		return ""
	}
	return storage.SpamBanApplied
}

// A resumed attempt may already have received a permanent ban in Telegram.
func (b *Bot) permanentChatBan(ctx context.Context, chatID, userID int64) (bool, error) {
	member, err := b.api.GetChatMember(ctx, &telego.GetChatMemberParams{ChatID: tu.ID(chatID), UserID: userID})
	if err != nil {
		return false, err
	}
	ban, ok := member.(*telego.ChatMemberBanned)
	return ok && ban.UntilDate == 0, nil
}

func (b *Bot) retireSpamBanAdmission(ctx context.Context, chatID, userID int64) error {
	admission, found, err := b.db.GetPendingAdmission(ctx, chatID, userID)
	if err != nil || !found {
		return err
	}
	return b.retireBannedAdmission(ctx, admission)
}

func definitiveBanRejection(err error) bool {
	var apiErr *telegoapi.Error
	return !errors.Is(err, errBanUnconfirmed) && errors.As(err, &apiErr) &&
		apiErr.ErrorCode >= 400 && apiErr.ErrorCode < 500 && apiErr.ErrorCode != 408
}

func (b *Bot) finishSpamVoteMessage(ctx context.Context, v storage.SpamVerdict) {
	if v.BotMsgID <= 0 {
		return
	}
	if v.Outcome == storage.SpamBanDenied {
		text := "Вердикт принят. Забанить пользователя не удалось. Проверьте права бота."
		_, err := b.api.EditMessageText(ctx, &telego.EditMessageTextParams{
			ChatID: tu.ID(v.ChatID), MessageID: v.BotMsgID, Text: text,
			ReplyMarkup: &telego.InlineKeyboardMarkup{InlineKeyboard: [][]telego.InlineKeyboardButton{}},
		})
		if err != nil && !isNotModified(err) {
			b.log.Warn("show refused spam verdict", "err", err, "chat", v.ChatID)
			if _, err := b.api.SendMessage(ctx, tu.Message(tu.ID(v.ChatID), text)); err != nil {
				b.log.Warn("send refused spam verdict", "err", err, "chat", v.ChatID)
			}
		}
		return
	}
	if err := b.deleteMessage(ctx, v.ChatID, v.BotMsgID); err != nil {
		b.log.Debug("delete resolved spam vote message", "err", err, "chat", v.ChatID)
	}
}
