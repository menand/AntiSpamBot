package bot

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/menand/AntiSpamBot/internal/captcha"
	"github.com/menand/AntiSpamBot/internal/storage"
)

const admissionPendingText = "Решение сохранено. Права ещё восстанавливаются, бот повторит запрос."

var errAdmissionBusy = errors.New("admission in progress")

// takeCaptchaAnswer держит исходную капчу доступной при ошибке записи.
// Правильный ответ заменяет строку проверки до остановки её таймера.
func (b *Bot) takeCaptchaAnswer(ctx context.Context, chatID, userID int64,
	msg *telego.Message, correct func(*captcha.Pending) bool,
) (*captcha.Pending, error) {
	if err := b.checkManualMute(ctx, chatID, userID); err != nil {
		return nil, err
	}
	if _, pending, err := b.db.GetPunishment(ctx, chatID, userID); err != nil || pending {
		return nil, err
	}
	var saveErr error
	p, _ := b.store.TakeMatch(chatID, userID, func(p *captcha.Pending) bool {
		if msg == nil || staleCaptchaClick(p, msg) {
			return false
		}
		if !correct(p) {
			return true
		}
		s, err := b.db.GetChatSettings(ctx, chatID)
		if err != nil {
			saveErr = err
			return false
		}
		row := storage.PendingAdmission{
			ChatID: chatID, UserID: userID, Generation: rand.Text(), Action: storage.AdmissionCaptcha,
			MessageID: p.MessageID, EphemeralID: p.EphemeralID, ThreadID: p.ThreadID,
			ReleaseNeeded: true, ClosesCheck: true,
		}
		if s.QuarantineEnabled {
			row.QuarantineUntil = time.Now().Add(b.effectiveQuarantineDuration(s)).Unix()
		}
		accepted, err := b.db.AcceptCaptcha(ctx, row)
		saveErr = err
		return accepted
	})
	if p != nil {
		p.Cancel()
	}
	return p, saveErr
}

func (b *Bot) onSuccess(ctx context.Context, p *captcha.Pending, answer string) error {
	b.log.Info("captcha passed", "chat", p.ChatID, "user", p.UserID, "answer", answer)
	return b.executePendingAdmission(ctx, p.ChatID, p.UserID)
}

func (b *Bot) retryAdmission(ctx context.Context, chatID, userID int64) error {
	unlock, err := b.rights.lock(ctx, chatID, userID, false)
	if err != nil {
		return err
	}
	defer unlock()
	return b.executePendingAdmission(ctx, chatID, userID)
}

func (b *Bot) executePendingAdmission(ctx context.Context, chatID, userID int64) error {
	row, exists, err := b.db.GetPendingAdmission(ctx, chatID, userID)
	if err != nil || !exists {
		return err
	}
	if (row.Action == storage.AdmissionUnbanChat || row.Action == storage.AdmissionLocalUnban) && row.PardonID > 0 {
		newer, err := b.db.NewerSpamDecision(ctx, chatID, userID, row.PardonID)
		if err != nil {
			return err
		}
		if newer {
			return b.db.DeletePendingAdmission(ctx, row)
		}
	}
	return b.executeAdmission(ctx, &row)
}

func (b *Bot) executeAdmission(ctx context.Context, row *storage.PendingAdmission) error {
	chatID, userID := row.ChatID, row.UserID
	if row.BanPending {
		member, err := b.api.GetChatMember(ctx, &telego.GetChatMemberParams{ChatID: tu.ID(chatID), UserID: userID})
		if err != nil {
			return fmt.Errorf("reconcile ban before admission: %w", err)
		}
		if banned, ok := member.(*telego.ChatMemberBanned); ok && banned.UntilDate == 0 {
			return b.retireBannedAdmission(ctx, *row)
		}
		row.BanPending = false
		if err := b.db.UpdateAdmissionRights(ctx, *row); err != nil {
			return err
		}
	}
	if err := b.checkManualMute(ctx, chatID, userID); err != nil {
		return err
	}
	if _, pending, err := b.db.GetPunishment(ctx, chatID, userID); err != nil || pending {
		if err != nil {
			return err
		}
		return fmt.Errorf("punishment reconciliation pending")
	}
	if row.Action != storage.AdmissionCaptcha && !row.UnbanDone {
		if err := b.unban(ctx, chatID, userID); err != nil {
			return fmt.Errorf("unban pending: %w", err)
		}
		row.UnbanDone = true
		if err := b.db.UpdateAdmissionRights(ctx, *row); err != nil {
			return err
		}
	}

	s, err := b.db.GetChatSettings(ctx, chatID)
	if err != nil {
		return err
	}
	if row.QuarantineUntil != 0 {
		var applied bool
		if s.QuarantineEnabled && time.Until(time.Unix(row.QuarantineUntil, 0)) >= time.Minute {
			err := b.quarantineUntil(ctx, chatID, userID, row.QuarantineUntil)
			if err == nil {
				fresh, err := b.db.GetChatSettings(ctx, chatID)
				if err != nil {
					return err
				}
				applied = fresh.QuarantineEnabled
				s = fresh
			} else {
				b.log.Warn("quarantine after captcha failed", "err", err, "chat", chatID, "user", userID)
			}
		}
		if !applied {
			row.QuarantineUntil = 0
			if err := b.db.UpdateAdmissionRights(ctx, *row); err != nil {
				return err
			}
		}
	}
	if row.ReleaseNeeded && row.QuarantineUntil == 0 {
		if err := b.release(ctx, chatID, userID); err != nil && !isUserNotParticipant(err) {
			return fmt.Errorf("release pending: %w", err)
		}
	}

	var replyDeadline time.Time
	if row.Action == storage.AdmissionCaptcha {
		s, err = b.db.GetChatSettings(ctx, chatID)
		if err != nil {
			return err
		}
	}
	mode := effectiveWelcomeMode(s)
	if row.Action == storage.AdmissionCaptcha && mode == welcomeReply {
		replyDeadline = time.Now().Add(b.effectiveStageInterval(s))
	}
	if b.rights.muteWaiting(chatID, userID) {
		return errAdmissionBusy
	}
	finished, err := b.db.FinishAdmission(ctx, *row, replyDeadline)
	if err != nil {
		return err
	}
	if !finished {
		return errAdmissionBusy
	}
	if row.QuarantineUntil != 0 {
		b.quar.Set(chatID, userID, time.Unix(row.QuarantineUntil, 0), false)
	} else {
		b.quar.Remove(chatID, userID)
	}
	var reply *replyPending
	if !replyDeadline.IsZero() {
		reply = b.replies.Put(chatID, userID, replyDeadline, row.ThreadID, 1, 0)
	}

	if row.MessageID > 0 || row.EphemeralID > 0 {
		if err := b.deleteBotMessage(ctx, chatID, row.MessageID, row.EphemeralID, userID); err != nil {
			b.log.Debug("delete admitted captcha", "err", err, "chat", chatID, "user", userID)
		}
	}
	if row.Action != storage.AdmissionCaptcha {
		return nil
	}
	if reply != nil {
		b.sendAdmissionReplyGreeting(ctx, s, reply)
	} else if mode == welcomeGreet {
		b.maybeSendGreeting(ctx, s, mode, chatID, userID, row.ThreadID)
	}
	b.maybeProfileCheck(chatID, userID, row.ThreadID)
	return nil
}

func (b *Bot) retireBannedAdmission(ctx context.Context, row storage.PendingAdmission) error {
	if err := b.db.DeletePendingAdmission(ctx, row); err != nil {
		return err
	}
	if row.MessageID > 0 || row.EphemeralID > 0 {
		if err := b.deleteBotMessage(ctx, row.ChatID, row.MessageID, row.EphemeralID, row.UserID); err != nil {
			b.log.Debug("delete banned user's captcha", "err", err, "chat", row.ChatID, "user", row.UserID)
		}
	}
	return nil
}

func (b *Bot) sendAdmissionReplyGreeting(ctx context.Context, s storage.ChatSettings, p *replyPending) {
	chatID, userID := p.ChatID, p.UserID
	if current, ok := b.replies.Get(chatID, userID); !ok || current != p {
		return
	}
	if !b.sendReplyGreeting(ctx, s, p) {
		if taken, ok := b.replies.TakeMatch(chatID, userID, func(live *replyPending) bool { return live == p }); ok {
			taken.Cancel()
			if err := b.db.FinishUnconfirmedReplyWelcome(ctx, storage.PendingReply{ChatID: chatID, UserID: userID}); err != nil {
				b.log.Warn("close undelivered reply welcome", "err", err, "chat", chatID, "user", userID)
			}
		}
		return
	}
	b.goSafe("replyWaitLoop", func() { b.replyWaitLoop(chatID, userID, p) })
}

func (b *Bot) reconcileAdmissions(ctx context.Context) {
	rows, err := b.db.LoadPendingAdmissions(ctx)
	if err != nil {
		b.log.Warn("list pending admissions", "err", err)
		return
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}
		if allowed, err := b.chatServiceableChecked(ctx, row.ChatID); err != nil || !allowed {
			continue
		}
		budget, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := b.retryAdmission(budget, row.ChatID, row.UserID)
		cancel()
		if err != nil && !errors.Is(err, errAdmissionBusy) {
			b.log.Warn("retry admission", "err", err, "chat", row.ChatID, "user", row.UserID)
		}
	}
}

func (b *Bot) reportCaptchaSaveError(ctx *th.Context, query telego.CallbackQuery, err error) {
	b.log.Warn("save captcha admission", "err", err)
	message := "Не удалось сохранить ответ. Попробуйте ещё раз."
	if errors.Is(err, errMuteUnconfirmed) {
		message = "Бот сверяет решение администратора о муте."
	}
	_ = b.api.AnswerCallbackQuery(ctx, tu.CallbackQuery(query.ID).WithText(message).WithShowAlert())
}

func (b *Bot) answerAdmissionCallback(ctx *th.Context, query telego.CallbackQuery, err error, success string) {
	answer := tu.CallbackQuery(query.ID).WithText(success)
	if err != nil {
		b.log.Warn("complete captcha admission", "err", err)
		text := admissionPendingText
		if errors.Is(err, errAdmissionBusy) {
			text = "Решение сохранено. Администратор меняет права участника."
		}
		answer = answer.WithText(text).WithShowAlert()
	}
	if ackErr := b.api.AnswerCallbackQuery(ctx, answer); ackErr != nil && err != nil {
		// После ретраев Telegram может уже отвергнуть callback как
		// просроченный; частичный результат всё равно нужен нажавшему.
		threadID := 0
		if msg := query.Message.Message(); msg != nil {
			threadID = threadOf(*msg)
		}
		b.sendPlain(query.Message.GetChat().ID, threadID, query.From.ID, admissionPendingText)
	}
}

func (b *Bot) admissionRetryLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		b.reconcileAdmissions(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (b *Bot) execPersistentUnmod(action string, chatID, userID int64) (string, error) {
	ctx, cancel := b.liveCtx()
	defer cancel()
	unlock, err := b.rights.lock(ctx, chatID, userID, false)
	if err != nil {
		return "", err
	}
	defer unlock()
	if err := b.checkManualMute(ctx, chatID, userID); err != nil {
		return "Бот сверяет предыдущий мут. Повторите команду после сверки.", err
	}
	row, err := b.prepareManualAdmission(ctx, action, chatID, userID, 0, nil)
	if err != nil {
		return "", err
	}
	if confirmed, err := b.applyManualAdmission(ctx, row); err != nil {
		if confirmed {
			return "Разбан подтверждён. Снятие ограничений ещё не подтверждено, бот повторит запрос.", err
		}
		if action == storage.AdmissionWhitelist {
			return "Доверие сохранено. Бот повторит разбан и проверку ограничений.", err
		}
		return admissionPendingText, err
	}
	if action == storage.AdmissionWhitelist {
		return "🤝 " + b.mentionFor(userID) + " в доверенных — войдёт без капчи.", nil
	}
	return "♻️ " + b.mentionFor(userID) + " разбанен — может зайти заново.", nil
}

func (b *Bot) prepareManualAdmission(ctx context.Context, action string, chatID, userID, pardonID int64, otherChats []int64) (storage.PendingAdmission, error) {
	row, err := b.db.AcceptManualAdmission(ctx, storage.PendingAdmission{
		ChatID: chatID, UserID: userID, Action: action, Generation: rand.Text(), PardonID: pardonID,
	}, otherChats)
	if err != nil {
		return row, err
	}
	b.store.CancelKickoff(chatID, userID)
	if p, ok := b.store.Take(chatID, userID); ok {
		p.Cancel()
	}
	if p, ok := b.replies.Take(chatID, userID); ok {
		p.Cancel()
	}
	return row, nil
}

func (b *Bot) applyManualAdmission(ctx context.Context, row storage.PendingAdmission) (bool, error) {
	err := b.executeAdmission(ctx, &row)
	return row.UnbanDone, err
}
