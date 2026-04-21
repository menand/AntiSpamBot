package bot

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"

	"github.com/menand/AntiSpamBot/internal/attempts"
	"github.com/menand/AntiSpamBot/internal/captcha"
	"github.com/menand/AntiSpamBot/internal/config"
)

type Bot struct {
	api      *telego.Bot
	cfg      *config.Config
	store    *captcha.Store
	attempts *attempts.Tracker
	log      *slog.Logger

	me     *telego.User
	runCtx context.Context
}

func New(cfg *config.Config, log *slog.Logger) (*Bot, error) {
	api, err := telego.NewBot(cfg.Token)
	if err != nil {
		return nil, fmt.Errorf("create bot: %w", err)
	}
	return &Bot{
		api:      api,
		cfg:      cfg,
		store:    captcha.NewStore(),
		attempts: attempts.NewTracker(24 * time.Hour),
		log:      log,
	}, nil
}

func (b *Bot) Username() string {
	if b.me == nil {
		return ""
	}
	return b.me.Username
}

func (b *Bot) Run(ctx context.Context) error {
	b.runCtx = ctx

	me, err := b.api.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("get me: %w", err)
	}
	b.me = me

	go b.attempts.Run(ctx)

	updates, err := b.api.UpdatesViaLongPolling(ctx, &telego.GetUpdatesParams{
		AllowedUpdates: []string{"message", "callback_query", "chat_member"},
	})
	if err != nil {
		return fmt.Errorf("long polling: %w", err)
	}

	bh, err := th.NewBotHandler(b.api, updates)
	if err != nil {
		return fmt.Errorf("bot handler: %w", err)
	}
	defer func() { _ = bh.Stop() }()

	bh.Handle(b.handleChatMember, th.AnyChatMember())
	bh.HandleCallbackQuery(b.handleCallback, th.AnyCallbackQueryWithMessage(), th.CallbackDataPrefix("cap:"))

	return bh.Start()
}
