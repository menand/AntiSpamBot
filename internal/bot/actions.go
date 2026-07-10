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

// retryAfterDelay extracts Telegram's flood-control hint from an API error.
// Returns (delay, true) when the error is a 429 with a retry_after parameter.
func retryAfterDelay(err error) (time.Duration, bool) {
	var apiErr *telegoapi.Error
	if errors.As(err, &apiErr) && apiErr.ErrorCode == 429 && apiErr.Parameters != nil && apiErr.Parameters.RetryAfter > 0 {
		return time.Duration(apiErr.Parameters.RetryAfter) * time.Second, true
	}
	return 0, false
}

// sleepCtx waits for d or until ctx is done, whichever comes first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// tgBackoffs is the shared retry schedule for critical Telegram calls: the
// first attempt fires immediately, then three waits. On 429 (mass-join flood
// control) retryWith stretches a wait to Telegram's retry_after instead,
// otherwise every retry burns against the same flood window. retry_after is
// honored in full — a goroutine parked for a few minutes is cheaper than a
// human muted forever, and ctx cancellation (shutdown) aborts the sleep.
var tgBackoffs = []time.Duration{0, 1 * time.Second, 2 * time.Second, 4 * time.Second}

// kickUnbanBackoffs is the short ladder for the unban inside kick: the whole
// captcha fail path lives in waitTimeout's 10s cleanup context, and tgBackoffs
// (7s of pure sleep) would starve the later attempts once call latency is
// added. Four attempts within ~1.8s of sleep.
var kickUnbanBackoffs = []time.Duration{0, 300 * time.Millisecond, 600 * time.Millisecond, 900 * time.Millisecond}

// retryTG calls a Telegram API method with the shared backoff schedule and
// returns the last error when every attempt fails.
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
				// Keep the API error that caused the retrying — "context
				// deadline exceeded" alone is undiagnosable in the log.
				return fmt.Errorf("%w (last attempt error: %v)", err, lastErr)
			}
		}
		if lastErr = call(); lastErr == nil {
			return nil
		}
	}
	return lastErr
}

// isNotModified matches Telegram's "message is not modified" error: someone
// re-tapped a button that renders the exact same message. Expected, not worth
// a warning in the log.
func isNotModified(err error) bool {
	var apiErr *telegoapi.Error
	return errors.As(err, &apiErr) &&
		strings.Contains(apiErr.Description, "message is not modified")
}

func (b *Bot) restrict(ctx context.Context, chatID, userID int64) error {
	// Retried — a DNS/TCP blip on this call means the user is NOT restricted
	// and no captcha gets sent. Worse than a retry delay.
	err := retryTG(ctx, func() error {
		return b.api.RestrictChatMember(ctx, &telego.RestrictChatMemberParams{
			ChatID:      tu.ID(chatID),
			UserID:      userID,
			Permissions: telego.ChatPermissions{},
		})
	})
	if err != nil {
		return fmt.Errorf("restrict after retries: %w", err)
	}
	return nil
}

// release lifts the captcha restriction. It applies the chat's own default
// permissions (getChat) so a passed user ends up with exactly the same rights
// as everyone else — not more. If the chat restricts e.g. polls or media by
// default, blanket all-true permissions here would silently grant them.
// Retried — during a mass-join wave many users pass at the same time, and a
// dropped 429 here would leave a verified human muted with no captcha left
// to retry through.
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

// chatDefaultPermissions fetches the chat's default member permissions.
// Retried: during the same mass-join 429 wave that release retries through,
// a single-shot getChat would reliably fail and silently over-grant the
// all-true fallback. Falls back to a permissive all-true set only when the
// retries are exhausted — being unable to look up the defaults must not
// leave a verified human muted forever.
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

func (b *Bot) kick(ctx context.Context, chatID, userID int64) error {
	// Обе половины кика ретраятся на короткой лестнице: без ретрая бана
	// сетевой чих оставляет юзера замьюченным «зомби» в чате, а событие kick
	// уже записано. Короткая лестница ×2 (~3.6 c сна) укладывается в 10 c
	// cleanup-ctx вместе с латентностью вызовов.
	err := retryWith(ctx, kickUnbanBackoffs, func() error {
		return b.api.BanChatMember(ctx, &telego.BanChatMemberParams{
			ChatID: tu.ID(chatID),
			UserID: userID,
		})
	})
	if err != nil {
		return fmt.Errorf("ban (for kick) after retries: %w", err)
	}
	// Retry the unban so a transient API error doesn't turn a kick into a
	// permaban. Deliberately on the short ladder: the whole fail path shares
	// waitTimeout's 10s cleanup context with deleteMessage + ban + DB writes,
	// and the long tgBackoffs would starve the attempts that matter.
	err = retryWith(ctx, kickUnbanBackoffs, func() error {
		return b.api.UnbanChatMember(ctx, &telego.UnbanChatMemberParams{
			ChatID:       tu.ID(chatID),
			UserID:       userID,
			OnlyIfBanned: true,
		})
	})
	if err != nil {
		return fmt.Errorf("unban (for kick) after retries: %w", err)
	}
	return nil
}

// ban permanently bans a user. Retried — it is the terminal action of the
// captcha fail path, so nothing after it can be starved by the backoff.
func (b *Bot) ban(ctx context.Context, chatID, userID int64) error {
	err := retryTG(ctx, func() error {
		return b.api.BanChatMember(ctx, &telego.BanChatMemberParams{
			ChatID: tu.ID(chatID),
			UserID: userID,
		})
	})
	if err != nil {
		return fmt.Errorf("ban after retries: %w", err)
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
