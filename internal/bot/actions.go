package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"
	tu "github.com/mymmrac/telego/telegoutil"
)

// retryAfterDelay достаёт flood-control-подсказку Telegram из ошибки API.
// Возвращает (задержку, true), когда ошибка — 429 с параметром retry_after.
func retryAfterDelay(err error) (time.Duration, bool) {
	var apiErr *telegoapi.Error
	if errors.As(err, &apiErr) && apiErr.ErrorCode == 429 && apiErr.Parameters != nil && apiErr.Parameters.RetryAfter > 0 {
		return time.Duration(apiErr.Parameters.RetryAfter) * time.Second, true
	}
	return 0, false
}

// sleepCtx ждёт d или отмены ctx — что случится раньше.
func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// tgBackoffs — общая лестница ретраев критичных Telegram-вызовов: первая
// попытка сразу, затем три ожидания. На 429 (flood control при масс-джойне)
// retryWith растягивает ожидание до телеграмного retry_after — иначе каждый
// ретрай бился бы в то же flood-окно. retry_after выжидается полностью:
// горутина, припаркованная на пару минут, дешевле человека, замьюченного
// навсегда, а отмена ctx (shutdown) прерывает сон.
var tgBackoffs = []time.Duration{0, 1 * time.Second, 2 * time.Second, 4 * time.Second}

// kickUnbanBackoffs — короткая лестница для обеих половин kick: весь путь
// провала капчи живёт в 10-секундном cleanup-контексте стадии капчи, и
// tgBackoffs (7 c чистого сна) заморили бы поздние попытки, как только
// добавится латентность вызовов. Четыре попытки при ~1.8 c сна.
var kickUnbanBackoffs = []time.Duration{0, 300 * time.Millisecond, 600 * time.Millisecond, 900 * time.Millisecond}

// retryTG вызывает метод Telegram API по общей лестнице бэкоффов и
// возвращает последнюю ошибку, если все попытки провалились.
func retryTG(ctx context.Context, call func() error) error {
	return retryWith(ctx, tgBackoffs, call)
}

func retryWith(ctx context.Context, backoffs []time.Duration, call func() error) error {
	var lastErr error
	for _, wait := range backoffs {
		if ra, ok := retryAfterDelay(lastErr); ok && ra > wait {
			wait = ra
		}
		if wait > 0 {
			if err := sleepCtx(ctx, wait); err != nil {
				// Сохраняем API-ошибку, из-за которой ретраили: голый
				// «context deadline exceeded» в логе не диагностируется.
				return fmt.Errorf("%w (last attempt error: %v)", err, lastErr)
			}
		}
		if lastErr = call(); lastErr == nil {
			return nil
		}
	}
	return lastErr
}

// isNotModified матчит телеграмную ошибку «message is not modified»: кто-то
// повторно нажал кнопку, дающую байт-в-байт то же сообщение. Ожидаемо,
// warning в логе не заслуживает.
func isNotModified(err error) bool {
	var apiErr *telegoapi.Error
	return errors.As(err, &apiErr) &&
		strings.Contains(apiErr.Description, "message is not modified")
}

// isUserNotParticipant матчит «USER_NOT_PARTICIPANT»: адресат уже покинул чат
// (типично — ушёл в окне между restrict и доставкой эфемерной капчи, а в
// масс-джойн эфемерка с EphemeralMessageParameters требует участника). Ретраить такой
// 400 бессмысленно — это не транзиентная ошибка, юзер физически вне чата;
// и это не провал капчи (kick событие не пишем), а честный EventLeft.
func isUserNotParticipant(err error) bool {
	var apiErr *telegoapi.Error
	return errors.As(err, &apiErr) &&
		strings.Contains(apiErr.Description, "USER_NOT_PARTICIPANT")
}

func (b *Bot) restrict(ctx context.Context, chatID, userID int64) error {
	// Ретраится: сетевой чих (DNS/TCP) на этом вызове означает, что юзер НЕ
	// ограничен и капча не уйдёт. Это хуже, чем задержка на ретрай.
	if err := b.restrictFor(ctx, chatID, userID, 0); err != nil {
		return fmt.Errorf("restrict after retries: %w", err)
	}
	return nil
}

// mute — рид-онли на срок d. Telegram снимает ограничение по until_date сам,
// серверно: рестарт бота на размьют не влияет, хранить нечего. Ограничения
// API: until < 30 сек или > 366 дней от текущего момента означает «навсегда»
// — верхнюю границу валидирует вызывающий (parseMuteDuration), нижнюю
// защищает пересчёт until на каждой попытке в restrictFor.
func (b *Bot) mute(ctx context.Context, chatID, userID int64, d time.Duration) error {
	if err := b.restrictFor(ctx, chatID, userID, d); err != nil {
		return fmt.Errorf("mute after retries: %w", err)
	}
	return nil
}

// restrictFor — общее ядро restrict/mute: d == 0 — бессрочно (капча), d > 0 —
// until_date. until вычисляется в момент КАЖДОЙ попытки: retryTG честно ждёт
// весь retry_after на 429 (бывают минуты), и посчитанный заранее until мог бы
// к моменту успешного вызова опуститься ниже 30-секундного порога Telegram
// «меньше 30 сек = навсегда» — минутный мьют стал бы вечным.
func (b *Bot) restrictFor(ctx context.Context, chatID, userID int64, d time.Duration) error {
	return retryTG(ctx, func() error {
		p := &telego.RestrictChatMemberParams{
			ChatID:      tu.ID(chatID),
			UserID:      userID,
			Permissions: telego.ChatPermissions{},
		}
		if d > 0 {
			p.UntilDate = time.Now().Add(d).Unix()
		}
		return b.api.RestrictChatMember(ctx, p)
	})
}

// release снимает капча-ограничение. Применяет собственные дефолтные права
// чата (getChat), чтобы прошедший капчу получил ровно те же права, что у
// всех, — не больше. Если чат по умолчанию запрещает, скажем, опросы или
// медиа, огульные all-true права молча выдали бы их. Ретраится: при волне
// масс-джойна многие проходят капчу одновременно, и уроненный здесь 429
// оставил бы проверенного человека замьюченным — а капчи, через которую
// можно было бы повторить, уже нет.
func (b *Bot) release(ctx context.Context, chatID, userID int64) error {
	perms := b.chatDefaultPermissions(ctx, chatID)
	err := retryTG(ctx, func() error {
		return b.api.RestrictChatMember(ctx, &telego.RestrictChatMemberParams{
			ChatID:      tu.ID(chatID),
			UserID:      userID,
			Permissions: perms,
		})
	})
	if err != nil {
		return fmt.Errorf("release after retries: %w", err)
	}
	return nil
}

// chatDefaultPermissions запрашивает дефолтные права участников чата.
// Ретраится: в ту же волну масс-джойна с 429, сквозь которую ретраится
// release, одиночный getChat стабильно падал бы и молча выдавал бы
// сверх-щедрый all-true-фолбэк. Откат к разрешительному all-true — только
// когда ретраи исчерпаны: невозможность узнать дефолты не должна оставить
// проверенного человека замьюченным навсегда.
func (b *Bot) chatDefaultPermissions(ctx context.Context, chatID int64) telego.ChatPermissions {
	var perms *telego.ChatPermissions
	err := retryTG(ctx, func() error {
		chat, e := b.api.GetChat(ctx, &telego.GetChatParams{ChatID: tu.ID(chatID)})
		if e == nil && chat != nil {
			perms = chat.Permissions
		}
		return e
	})
	if err == nil && perms != nil {
		return *perms
	}
	if err != nil {
		b.log.Warn("get chat permissions, falling back to all-true",
			"err", err, "chat", chatID)
	}
	yes := true
	return telego.ChatPermissions{
		CanSendMessages:       &yes,
		CanSendAudios:         &yes,
		CanSendDocuments:      &yes,
		CanSendPhotos:         &yes,
		CanSendVideos:         &yes,
		CanSendVideoNotes:     &yes,
		CanSendVoiceNotes:     &yes,
		CanSendPolls:          &yes,
		CanSendOtherMessages:  &yes,
		CanAddWebPagePreviews: &yes,
		CanInviteUsers:        &yes,
	}
}

// releaseOnAbort снимает капча-мут с обрывочных путей runCaptcha (pending ещё
// не записан — рестарт юзера не восстановит, мут был бы вечным). На живом ctx
// работает прямо на нём: полный бюджет ретраев release, включая honor
// retry_after 429-шторма (ровно тот сценарий, где отправка капчи и падает).
// На мёртвом (shutdown) — свежий detached-бюджет: release на отменённом ctx
// был бы гарантированный no-op. 15 секунд покрывают обе retryTG-лестницы
// release (getChat + restrict, ~14 c сна в худшем случае).
func (b *Bot) releaseOnAbort(ctx context.Context, chatID, userID int64) {
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
	}
	if err := b.release(ctx, chatID, userID); err != nil {
		b.log.Warn("release on captcha abort", "err", err, "chat", chatID, "user", userID)
	}
}

func (b *Bot) kick(ctx context.Context, chatID, userID int64) error {
	// Обе половины кика ретраятся: без ретрая бана сетевой чих оставляет
	// юзера замьюченным «зомби» в чате, а событие kick уже записано (почему
	// короткая лестница — см. kickUnbanBackoffs: ×2 укладывается в 10 c
	// cleanup-ctx).
	err := retryWith(ctx, kickUnbanBackoffs, func() error {
		return b.api.BanChatMember(ctx, &telego.BanChatMemberParams{
			ChatID: tu.ID(chatID),
			UserID: userID,
		})
	})
	if err != nil {
		return fmt.Errorf("ban (for kick) after retries: %w", err)
	}
	return b.unban(ctx, chatID, userID)
}

// unban снимает бан (вторая половина кика), ретраясь на короткой лестнице,
// чтобы транзиентная ошибка API не превратила кик в перманентный бан
// (почему короткая — см. kickUnbanBackoffs). OnlyIfBanned делает вызов
// идемпотентным.
func (b *Bot) unban(ctx context.Context, chatID, userID int64) error {
	err := retryWith(ctx, kickUnbanBackoffs, func() error {
		return b.api.UnbanChatMember(ctx, &telego.UnbanChatMemberParams{
			ChatID:       tu.ID(chatID),
			UserID:       userID,
			OnlyIfBanned: true,
		})
	})
	if err != nil {
		return fmt.Errorf("unban after retries: %w", err)
	}
	return nil
}

// banShort — перманентный бан на короткой лестнице: путь провала капчи и
// реплай-чека живёт в 10-секундном cleanup-контексте, полный tgBackoffs
// (7 c чистого сна плюс растяжки retry_after) там не помещается — обрыв
// посреди лестницы означал бы вечный мьют без шанса на восстановление.
// На 429 retryWith по-прежнему растягивает ожидание до retry_after.
func (b *Bot) banShort(ctx context.Context, chatID, userID int64) error {
	err := retryWith(ctx, kickUnbanBackoffs, func() error {
		return b.api.BanChatMember(ctx, &telego.BanChatMemberParams{
			ChatID: tu.ID(chatID),
			UserID: userID,
		})
	})
	if err != nil {
		return fmt.Errorf("ban (short ladder) after retries: %w", err)
	}
	return nil
}

// banRevoke — перманентный бан со стиранием ВСЕХ сообщений юзера в чате.
// Используется только вердиктом ИИ-антиспама: у спамера сносится и то, что
// не попало под плашку. Капча-бан остаётся обычным ban — там сообщений нет.
// Ретраится обязательно: TakeSpamVote одноразовый, и упавший на сетевом чихе
// одиночный вызов терял бы вердикт сообщества безвозвратно.
func (b *Bot) banRevoke(ctx context.Context, chatID, userID int64) error {
	err := retryTG(ctx, func() error {
		return b.api.BanChatMember(ctx, &telego.BanChatMemberParams{
			ChatID:         tu.ID(chatID),
			UserID:         userID,
			RevokeMessages: true,
		})
	})
	if err != nil {
		return fmt.Errorf("ban with revoke after retries: %w", err)
	}
	return nil
}

// kickRevoke — кик со стиранием ВСЕХ сообщений юзера (для админской команды
// /kick): banRevoke + unban, чтобы юзер мог перезайти. Обе половины ретраятся.
func (b *Bot) kickRevoke(ctx context.Context, chatID, userID int64) error {
	if err := b.banRevoke(ctx, chatID, userID); err != nil {
		return err
	}
	return b.unban(ctx, chatID, userID)
}

func (b *Bot) deleteMessage(ctx context.Context, chatID int64, messageID int) error {
	err := b.api.DeleteMessage(ctx, &telego.DeleteMessageParams{
		ChatID:    tu.ID(chatID),
		MessageID: messageID,
	})
	if err != nil {
		return fmt.Errorf("delete message: %w", err)
	}
	return nil
}

// deleteBotMessage удаляет своё сообщение, обычное или эфемерное: ephID ≠ 0 —
// эфемерное (нужен receiver — юзер, которому оно было видно), иначе обычный
// deleteMessage по msgID.
func (b *Bot) deleteBotMessage(ctx context.Context, chatID int64, msgID, ephID int, receiverID int64) error {
	if ephID == 0 {
		return b.deleteMessage(ctx, chatID, msgID)
	}
	err := b.api.DeleteEphemeralMessage(ctx, &telego.DeleteEphemeralMessageParams{
		ChatID:             tu.ID(chatID),
		EphemeralMessageID: ephID,
		ReceiverUserID:     receiverID,
	})
	if err != nil {
		return fmt.Errorf("delete ephemeral message: %w", err)
	}
	return nil
}
