package bot

import (
	"context"
	"errors"
	"fmt"
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

func (b *Bot) restrict(ctx context.Context, chatID, userID int64) error {
	// Retry with backoff — a DNS/TCP blip on this call means the user is NOT
	// restricted and no captcha gets sent. Worse than a retry delay. On 429
	// (mass-join flood control) honor Telegram's retry_after instead of the
	// fixed backoff, otherwise every retry burns against the same flood window.
	backoffs := []time.Duration{0, 1 * time.Second, 2 * time.Second, 4 * time.Second}
	var lastErr error
	for _, wait := range backoffs {
		if ra, ok := retryAfterDelay(lastErr); ok && ra > wait {
			wait = ra
		}
		if wait > 0 {
			if err := sleepCtx(ctx, wait); err != nil {
				return fmt.Errorf("restrict: %w", err)
			}
		}
		lastErr = b.api.RestrictChatMember(ctx, &telego.RestrictChatMemberParams{
			ChatID:      tu.ID(chatID),
			UserID:      userID,
			Permissions: telego.ChatPermissions{},
		})
		if lastErr == nil {
			return nil
		}
	}
	return fmt.Errorf("restrict after retries: %w", lastErr)
}

// release lifts the captcha restriction. It applies the chat's own default
// permissions (getChat) so a passed user ends up with exactly the same rights
// as everyone else — not more. If the chat restricts e.g. polls or media by
// default, blanket all-true permissions here would silently grant them.
func (b *Bot) release(ctx context.Context, chatID, userID int64) error {
	perms := b.chatDefaultPermissions(ctx, chatID)
	err := b.api.RestrictChatMember(ctx, &telego.RestrictChatMemberParams{
		ChatID:      tu.ID(chatID),
		UserID:      userID,
		Permissions: perms,
	})
	if err != nil {
		return fmt.Errorf("release: %w", err)
	}
	return nil
}

// chatDefaultPermissions fetches the chat's default member permissions.
// Falls back to a permissive all-true set when getChat fails — being unable
// to look up the defaults must not leave a verified human muted forever.
func (b *Bot) chatDefaultPermissions(ctx context.Context, chatID int64) telego.ChatPermissions {
	chat, err := b.api.GetChat(ctx, &telego.GetChatParams{ChatID: tu.ID(chatID)})
	if err == nil && chat != nil && chat.Permissions != nil {
		return *chat.Permissions
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
	if err := b.api.BanChatMember(ctx, &telego.BanChatMemberParams{
		ChatID: tu.ID(chatID),
		UserID: userID,
	}); err != nil {
		return fmt.Errorf("ban (for kick): %w", err)
	}
	// Retry unban so a transient API error doesn't turn a kick into a permaban.
	var lastErr error
	for i := 0; i < 3; i++ {
		lastErr = b.api.UnbanChatMember(ctx, &telego.UnbanChatMemberParams{
			ChatID:       tu.ID(chatID),
			UserID:       userID,
			OnlyIfBanned: true,
		})
		if lastErr == nil {
			return nil
		}
		wait := time.Duration(i+1) * 300 * time.Millisecond
		if ra, ok := retryAfterDelay(lastErr); ok && ra > wait {
			wait = ra
		}
		if err := sleepCtx(ctx, wait); err != nil {
			return fmt.Errorf("unban (for kick): %w", err)
		}
	}
	return fmt.Errorf("unban (for kick) after retries: %w", lastErr)
}

func (b *Bot) ban(ctx context.Context, chatID, userID int64) error {
	err := b.api.BanChatMember(ctx, &telego.BanChatMemberParams{
		ChatID: tu.ID(chatID),
		UserID: userID,
	})
	if err != nil {
		return fmt.Errorf("ban: %w", err)
	}
	return nil
}

// banRevoke — перманентный бан со стиранием ВСЕХ сообщений юзера в чате.
// Используется только вердиктом ИИ-антиспама: у спамера сносится и то, что
// не попало под плашку. Капча-бан остаётся обычным ban — там сообщений нет.
func (b *Bot) banRevoke(ctx context.Context, chatID, userID int64) error {
	err := b.api.BanChatMember(ctx, &telego.BanChatMemberParams{
		ChatID:         tu.ID(chatID),
		UserID:         userID,
		RevokeMessages: true,
	})
	if err != nil {
		return fmt.Errorf("ban with revoke: %w", err)
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
