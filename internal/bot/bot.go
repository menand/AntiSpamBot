package bot

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/menand/AntiSpamBot/internal/captcha"
	"github.com/menand/AntiSpamBot/internal/config"
	"github.com/menand/AntiSpamBot/internal/groq"
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
	version   string

	// Write-through caches over chats/user_info: skip the DB write when the
	// value didn't change. Saves 2 of the 4 SQLite writes per group message.
	cacheMu   sync.Mutex
	chatCache map[int64]storage.ChatInfo
	userCache map[int64]storage.UserInfo

	// Pending "send me the new greeting text" prompts: userID → armed state.
	// Set when an admin taps ✏️ in chat settings, consumed by the next
	// private text message from that user (or dropped after greetInputTTL).
	greetMu    sync.Mutex
	greetInput map[int64]greetInputState

	// ИИ-антиспам: Groq-клиент, дедуп запущенных проверок (chat:user) и кэш
	// «этот юзер — админ чата» (для белого списка и золотого голоса).
	groqc        *groq.Client
	spamMu       sync.Mutex
	spamInflight map[chatUser]struct{}
	adminMu      sync.Mutex
	adminCache   map[chatUser]adminCacheEntry
}

// chatUser keys the per-(chat, user) maps above.
type chatUser struct {
	chatID int64
	userID int64
}

func New(cfg *config.Config, log *slog.Logger, version string) (*Bot, error) {
	api, err := telego.NewBot(cfg.Token)
	if err != nil {
		return nil, fmt.Errorf("create bot: %w", err)
	}
	if version == "" {
		version = "dev"
	}
	return &Bot{
		api:          api,
		cfg:          cfg,
		store:        captcha.NewStore(),
		log:          log,
		version:      version,
		chatCache:    make(map[int64]storage.ChatInfo),
		userCache:    make(map[int64]storage.UserInfo),
		greetInput:   make(map[int64]greetInputState),
		groqc:        groq.New(cfg.GroqAPIKey, cfg.GroqModel),
		spamInflight: make(map[chatUser]struct{}),
		adminCache:   make(map[chatUser]adminCacheEntry),
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
	go b.dailyDigestLoop(ctx)
	go b.reconcileChats(ctx)
	go b.spamVoteSweepLoop(ctx)
	if b.groqc.Enabled() {
		b.log.Info("AI spam analysis available", "model", b.groqc.Model())
	} else {
		b.log.Info("AI spam analysis unavailable: GROQ_API_KEY is not set")
	}

	b.notifyOwners(ctx, fmt.Sprintf(
		"🟢 <b>Бот запущен</b>\nUsername: @%s\nВерсия: <code>%s</code>\nВосстановлено капч: %d",
		b.Username(), b.version, restored))

	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		b.notifyOwners(shutCtx, "🔴 <b>Бот остановлен</b>")
	}()

	updates, err := b.api.UpdatesViaLongPolling(ctx, &telego.GetUpdatesParams{
		AllowedUpdates: []string{"message", "callback_query", "chat_member", "my_chat_member"},
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
	bh.Handle(b.handleMyChatMember, th.AnyMyChatMember())
	bh.HandleCallbackQuery(b.handleCallback, th.AnyCallbackQueryWithMessage(), th.CallbackDataPrefix("cap:"))
	bh.HandleCallbackQuery(b.handleApproveCallback, th.AnyCallbackQueryWithMessage(), th.CallbackDataPrefix("capok:"))
	bh.HandleCallbackQuery(b.handleMenuCallback, th.AnyCallbackQueryWithMessage(), th.CallbackDataPrefix("menu:"))
	bh.HandleCallbackQuery(b.handleSpamVoteCallback, th.AnyCallbackQueryWithMessage(), th.CallbackDataPrefix("sv:"))
	bh.HandleMessage(b.handleStatsCommand, th.CommandEqual("stats"))
	bh.HandleMessage(b.handleChatsCommand, th.CommandEqual("chats"))
	bh.HandleMessage(b.handleLogsCommand, th.CommandEqual("logs"))
	bh.HandleMessage(b.handleInfoCommand, th.CommandEqual("info"))
	bh.HandleMessage(b.handleGreetingCommand, th.CommandEqual("greeting"))
	bh.HandleMessage(b.handlePrivateStart, th.CommandEqual("start"))
	bh.HandleMessage(b.handlePrivateStart, th.CommandEqual("help"))
	bh.HandleMessage(b.handlePrivateText, privateMessagePredicate) // greeting-text input flow
	bh.HandleMessage(b.handleGroupMessage)                         // fallback: count messages in groups

	return bh.Start()
}

// privateMessagePredicate matches non-command private-chat messages that fell
// through the command handlers above.
func privateMessagePredicate(_ context.Context, update telego.Update) bool {
	return update.Message != nil && update.Message.Chat.Type == telego.ChatTypePrivate
}

func (b *Bot) setCommands(ctx context.Context) error {
	// Clear any commands that were previously announced at the default or
	// group scope — we want the "/" menu empty in groups.
	_ = b.api.DeleteMyCommands(ctx, &telego.DeleteMyCommandsParams{
		Scope: &telego.BotCommandScopeAllGroupChats{Type: "all_group_chats"},
	})
	_ = b.api.DeleteMyCommands(ctx, &telego.DeleteMyCommandsParams{
		Scope: &telego.BotCommandScopeAllChatAdministrators{Type: "all_chat_administrators"},
	})
	_ = b.api.DeleteMyCommands(ctx, &telego.DeleteMyCommandsParams{
		Scope: &telego.BotCommandScopeDefault{Type: "default"},
	})

	// Only private chats see the "/" command menu.
	return b.api.SetMyCommands(ctx, &telego.SetMyCommandsParams{
		Scope: &telego.BotCommandScopeAllPrivateChats{Type: "all_private_chats"},
		Commands: []telego.BotCommand{
			{Command: "start", Description: "Меню"},
			{Command: "help", Description: "Справка"},
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
		p := b.store.Put(row.ChatID, row.UserID, row.MessageID, row.CorrectIdx, expires, row.ThreadID)
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
