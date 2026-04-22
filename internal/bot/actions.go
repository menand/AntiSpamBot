package bot

import (
	"context"
	"fmt"
	"time"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

func (b *Bot) restrict(ctx context.Context, chatID, userID int64) error {
	// Retry with backoff — a DNS/TCP blip on this call means the user is NOT
	// restricted and no captcha gets sent. Worse than a retry delay.
	backoffs := []time.Duration{0, 1 * time.Second, 2 * time.Second, 4 * time.Second}
	var lastErr error
	for _, wait := range backoffs {
		if wait > 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("restrict: %w", ctx.Err())
			case <-time.After(wait):
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

func (b *Bot) release(ctx context.Context, chatID, userID int64) error {
	yes := true
	err := b.api.RestrictChatMember(ctx, &telego.RestrictChatMemberParams{
		ChatID: tu.ID(chatID),
		UserID: userID,
		Permissions: telego.ChatPermissions{
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
		},
	})
	if err != nil {
		return fmt.Errorf("release: %w", err)
	}
	return nil
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
		select {
		case <-ctx.Done():
			return fmt.Errorf("unban (for kick): %w", ctx.Err())
		case <-time.After(time.Duration(i+1) * 300 * time.Millisecond):
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
