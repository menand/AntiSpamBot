package bot

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/menand/AntiSpamBot/internal/captcha"
	"github.com/menand/AntiSpamBot/internal/config"
	"github.com/menand/AntiSpamBot/internal/storage"
)

const attemptsTTL = 24 * time.Hour

type Bot struct {
	api   *telego.Bot
	cfg   *config.Config
	store *captcha.Store
	db    *storage.DB
	log   *slog.Logger

	me        *telego.User
	runCtx    context.Context
	startedAt time.Time
}

func New(cfg *config.Config, log *slog.Logger) (*Bot, error) {
	api, err := telego.NewBot(cfg.Token)
	if err != nil {
		return nil, fmt.Errorf("create bot: %w", err)
	}
	return &Bot{
		api:   api,
		cfg:   cfg,
		store: captcha.NewStore(),
		log:   log,
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
	b.startedAt = time.Now()

	db, err := storage.Open(ctx, b.cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open storage: %w", err)
	}
	b.db = db
	defer func() { _ = db.Close() }()

	me, err := b.api.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("get me: %w", err)
	}
	b.me = me

	restored, err := b.restorePending(ctx)
	if err != nil {
		b.log.Error("restore pending captchas", "err", err)
	}

	go b.attemptsSweepLoop(ctx)

	b.notifyOwners(ctx, fmt.Sprintf(
		"🟢 <b>Бот запущен</b>\nUsername: @%s\nВосстановлено капч: %d",
		b.Username(), restored))

	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		b.notifyOwners(shutCtx, "🔴 <b>Бот остановлен</b>")
	}()

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

	if err := b.setCommands(ctx); err != nil {
		b.log.Warn("set commands", "err", err)
	}

	bh.Handle(b.handleChatMember, th.AnyChatMember())
	bh.HandleCallbackQuery(b.handleCallback, th.AnyCallbackQueryWithMessage(), th.CallbackDataPrefix("cap:"))
	bh.HandleCallbackQuery(b.handleMenuCallback, th.AnyCallbackQueryWithMessage(), th.CallbackDataPrefix("menu:"))
	bh.HandleMessage(b.handleStatsCommand, th.CommandEqual("stats"))
	bh.HandleMessage(b.handleChatsCommand, th.CommandEqual("chats"))
	bh.HandleMessage(b.handleLogsCommand, th.CommandEqual("logs"))
	bh.HandleMessage(b.handleInfoCommand, th.CommandEqual("info"))
	bh.HandleMessage(b.handleGreetingCommand, th.CommandEqual("greeting"))
	bh.HandleMessage(b.handlePrivateStart, th.CommandEqual("start"))
	bh.HandleMessage(b.handlePrivateStart, th.CommandEqual("help"))
	bh.HandleMessage(b.handleGroupMessage) // fallback: count messages in groups

	return bh.Start()
}

func (b *Bot) setCommands(ctx context.Context) error {
	return b.api.SetMyCommands(ctx, &telego.SetMyCommandsParams{
		Commands: []telego.BotCommand{
			{Command: "start", Description: "Меню"},
			{Command: "help", Description: "Справка"},
			{Command: "stats", Description: "Статистика чата (для админов)"},
			{Command: "greeting", Description: "Приветствие после капчи: on/off (для админов)"},
			{Command: "chats", Description: "Мои чаты (для владельцев бота)"},
			{Command: "info", Description: "Uptime бота (для владельцев)"},
			{Command: "logs", Description: "Прислать лог-файл (для владельцев бота)"},
		},
	})
}

func (b *Bot) restorePending(ctx context.Context) (int, error) {
	rows, err := b.db.LoadAllPending(ctx)
	if err != nil {
		return 0, err
	}
	now := time.Now()
	for _, row := range rows {
		expires := row.ExpiresAt
		if expires.Before(now) {
			// Already expired while the bot was down — treat as timeout immediately.
			expires = now.Add(1 * time.Second)
		}
		p := b.store.Put(row.ChatID, row.UserID, row.MessageID, row.CorrectIdx, expires)
		go b.waitTimeout(p)
	}
	b.log.Info("restored pending captchas", "count", len(rows))
	return len(rows), nil
}

func (b *Bot) isOwner(userID int64) bool {
	_, ok := b.cfg.OwnerIDs[userID]
	return ok
}

func (b *Bot) notifyOwners(ctx context.Context, text string) {
	for ownerID := range b.cfg.OwnerIDs {
		_, err := b.api.SendMessage(ctx, tu.Message(tu.ID(ownerID), text).
			WithParseMode(telego.ModeHTML))
		if err != nil {
			b.log.Warn("notify owner failed — did they /start the bot in DM?",
				"err", err, "owner", ownerID)
		}
	}
}

func (b *Bot) attemptsSweepLoop(ctx context.Context) {
	t := time.NewTicker(attemptsTTL / 2)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := b.db.SweepAttempts(ctx, attemptsTTL); err != nil {
				b.log.Warn("sweep attempts", "err", err)
			}
		}
	}
}
