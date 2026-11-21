package bot

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
)

const (
	pollTimeout = 30
	pollRetry   = 2 * time.Second
)

// Telegram confirms the current batch on the next call with a higher offset.
// Keep that call behind the handlers, so a crash can replay an unfinished batch.
func (b *Bot) pollUpdates(ctx context.Context, routes *th.HandlerGroup, polled func()) error {
	handled, err := b.db.HandledTelegramUpdates(ctx, b.me.ID)
	if err != nil {
		return fmt.Errorf("load completed Telegram updates: %w", err)
	}
	params := telego.GetUpdatesParams{
		Timeout:        pollTimeout,
		AllowedUpdates: []string{"message", "edited_message", "callback_query", "chat_member", "my_chat_member"},
	}
	for ctx.Err() == nil {
		request, cancel := context.WithTimeout(ctx, (pollTimeout+10)*time.Second)
		updates, err := b.api.GetUpdates(request, &params)
		cancel()
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			b.log.Warn("get updates", "err", err)
			delay := pollRetry
			if retryAfter, ok := retryAfterDelay(err); ok {
				delay = max(delay, retryAfter)
			}
			_ = sleepCtx(ctx, delay)
			continue
		}
		polled()
		if params.Offset > 0 || (len(updates) == 0 && len(handled) > 0) {
			if err := b.db.ForgetConfirmedTelegramUpdates(ctx, b.me.ID, params.Offset); err != nil {
				b.log.Warn("forget confirmed Telegram updates", "err", err)
			}
			for id := range handled {
				if params.Offset == 0 || id < params.Offset {
					delete(handled, id)
				}
			}
		}

		var handlers sync.WaitGroup
		for _, update := range updates {
			if handled[update.UpdateID] {
				continue
			}
			handlers.Add(1)
			b.goSafe("handleUpdate", func() {
				defer handlers.Done()
				b.handlePolledUpdate(ctx, routes, update)
			})
		}
		handlers.Wait()

		// An empty successful response has already confirmed the previous batch.
		// Clear the offset so Telegram can choose a fresh update ID after a long idle.
		params.Offset = 0
		for _, update := range updates {
			params.Offset = max(params.Offset, update.UpdateID+1)
		}
	}
	return nil //nolint:nilerr // cancellation is normal during shutdown
}

func (b *Bot) handlePolledUpdate(ctx context.Context, routes *th.HandlerGroup, update telego.Update) {
	for ctx.Err() == nil {
		err := routes.HandleUpdate(ctx, b.api, update)
		if err == nil {
			break
		}
		b.log.Warn("handle update", "update", update.UpdateID, "err", err)
		_ = sleepCtx(ctx, pollRetry)
	}
	for ctx.Err() == nil {
		err := b.db.MarkTelegramUpdateHandled(ctx, b.me.ID, update.UpdateID)
		if err == nil {
			return
		}
		b.log.Warn("save completed Telegram update", "update", update.UpdateID, "err", err)
		_ = sleepCtx(ctx, pollRetry)
	}
}
