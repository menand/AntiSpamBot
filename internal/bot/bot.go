package bot

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/menand/AntiSpamBot/internal/captcha"
	"github.com/menand/AntiSpamBot/internal/config"
	"github.com/menand/AntiSpamBot/internal/gigachat"
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

	// Активные ожидания «ответь на приветствие» (режим reply_check).
	replies *replyStore

	// Write-through-кэши над chats/user_info: пропускаем запись в БД, когда
	// значение не изменилось. Экономят 2 из 4 SQLite-записей на групповое
	// сообщение.
	cacheMu   sync.Mutex
	chatCache map[int64]storage.ChatInfo
	userCache map[int64]storage.UserInfo

	// Взведённые запросы «пришли мне новый текст приветствия»: userID →
	// состояние. Ставится, когда админ жмёт ✏️ в настройках чата; забирается
	// следующим личным текстовым сообщением (или отваливается по greetInputTTL).
	greetMu    sync.Mutex
	greetInput map[int64]greetInputState

	// ИИ-антиспам: Groq-клиент (первичный), GigaChat (фолбек при ошибках и
	// лимитах Groq), дедуп запущенных проверок (chat:user) и кэш «этот юзер —
	// админ чата» (для белого списка и золотого голоса).
	groqc        *groq.Client
	gigac        *gigachat.Client
	spamMu       sync.Mutex
	spamInflight map[chatUser]struct{}
	// editChecked — время последней спам-проверки ПРАВКИ по (chat, user):
	// правки не инкрементят счётчик сообщений, и без кулдауна новичок мог бы
	// бесконечными правками одного сообщения жечь LLM-квоту. Unbounded по
	// тому же соглашению, что userCache (запись ~40 байт на активного юзера).
	editChecked map[chatUser]time.Time
	adminMu     sync.Mutex
	adminCache  map[chatUser]adminCacheEntry
}

// chatUser — ключ пер-(chat, user) карт выше.
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
		replies:      newReplyStore(),
		log:          log,
		version:      version,
		chatCache:    make(map[int64]storage.ChatInfo),
		userCache:    make(map[int64]storage.UserInfo),
		greetInput:   make(map[int64]greetInputState),
		groqc:        groq.New(cfg.GroqAPIKey, cfg.GroqModel),
		gigac:        gigachat.New(cfg.GigaChatAuthKey, cfg.GigaChatScope, cfg.GigaChatModel, groq.SystemPrompt),
		spamInflight: make(map[chatUser]struct{}),
		editChecked:  make(map[chatUser]time.Time),
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
	if _, err := b.restorePendingReplies(ctx); err != nil {
		b.log.Error("restore pending replies", "err", err)
	}

	b.goSafe("attemptsSweepLoop", func() { b.attemptsSweepLoop(ctx) })
	b.goSafe("dailyDigestLoop", func() { b.dailyDigestLoop(ctx) })
	b.goSafe("reconcileChats", func() { b.reconcileChats(ctx) })
	b.goSafe("spamVoteSweepLoop", func() { b.spamVoteSweepLoop(ctx) })
	b.goSafe("announceVersion", func() { b.announceVersion(ctx) })
	var providers []string
	if b.groqc.Enabled() {
		providers = append(providers, "groq:"+b.groqc.Model())
	}
	if b.gigac.Enabled() {
		providers = append(providers, "gigachat:"+b.gigac.Model())
	}
	if len(providers) == 0 {
		b.log.Info("AI spam analysis unavailable: neither GROQ_API_KEY nor GIGACHAT_AUTH_KEY is set")
	} else {
		// Порядок в списке = порядок цепочки classifySpam.
		b.log.Info("AI spam analysis available", "providers", strings.Join(providers, ", "))
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
		AllowedUpdates: []string{"message", "edited_message", "callback_query", "chat_member", "my_chat_member"},
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

	// Паника в хендлере не должна ронять весь процесс (все чаты гаснут разом).
	// Middleware покрывает только цепочки telego-хендлеров; собственные
	// горутины прикрывает goSafe — recover не пересекает границу горутины.
	// Обязательно ДО bh.Handle*: telego матчит маршруты в порядке регистрации.
	bh.Use(th.PanicRecoveryHandler(func(recovered any) error {
		b.log.Error("panic in handler",
			"recovered", recovered, "stack", string(debug.Stack()))
		return nil
	}))

	// Снятие ожидания «ответь на приветствие» — в middleware, ДО маршрутизации:
	// иначе команда (/start и т.п.) первым сообщением новичка ушла бы в свой
	// хендлер мимо handleGroupMessage, и юзера кикнуло бы за молчание, хотя он
	// написал. Дёшево (in-memory Take-промах для всех, у кого ожидания нет).
	// Только НАСТОЯЩИЙ контент (текст/медиа): сервисное сообщение (новичок
	// добавил участника — тоже с From) не должно засчитываться за ответ.
	bh.Use(func(ctx *th.Context, update telego.Update) error {
		if m := update.Message; m != nil && m.From != nil && !m.From.IsBot &&
			(m.Chat.Type == "group" || m.Chat.Type == "supergroup") &&
			messageHasUserContent(m) {
			b.replyWaitSatisfied(m.Chat.ID, m.From.ID)
		}
		return ctx.Next(update)
	})

	bh.Handle(b.handleChatMember, th.AnyChatMember())
	bh.Handle(b.handleMyChatMember, th.AnyMyChatMember())
	bh.HandleCallbackQuery(b.handleCallback, th.AnyCallbackQueryWithMessage(), th.CallbackDataPrefix("cap:"))
	bh.HandleCallbackQuery(b.handleApproveCallback, th.AnyCallbackQueryWithMessage(), th.CallbackDataPrefix("capok:"))
	bh.HandleCallbackQuery(b.handleMenuCallback, th.AnyCallbackQueryWithMessage(), th.CallbackDataPrefix("menu:"))
	bh.HandleCallbackQuery(b.handleSpamVoteCallback, th.AnyCallbackQueryWithMessage(), th.CallbackDataPrefix("sv:"))
	bh.HandleCallbackQuery(b.handleModChoiceCallback, th.AnyCallbackQueryWithMessage(), th.CallbackDataPrefix("mc:"))
	bh.HandleMessage(b.handleStatsCommand, th.CommandEqual("stats"))
	bh.HandleMessage(b.handleChatsCommand, th.CommandEqual("chats"))
	bh.HandleMessage(b.handleLogsCommand, th.CommandEqual("logs"))
	bh.HandleMessage(b.handleInfoCommand, th.CommandEqual("info"))
	bh.HandleMessage(b.handleGreetingCommand, th.CommandEqual("greeting"))
	bh.HandleMessage(b.handleKickCommand, th.CommandEqual("kick"))
	bh.HandleMessage(b.handleBanCommand, th.CommandEqual("ban"))
	bh.HandleMessage(b.handleDeleteCommand, th.CommandEqual("del"))
	bh.HandleMessage(b.handleDeleteCommand, th.CommandEqual("delete"))
	bh.HandleMessage(b.handleMuteCommand, th.CommandEqual("mute"))
	bh.HandleMessage(b.handleUnbanCommand, th.CommandEqual("unban"))
	bh.HandleMessage(b.handleUnmuteCommand, th.CommandEqual("unmute"))
	bh.HandleMessage(b.handleWhitelistCommand, th.CommandEqual("whitelist"))
	bh.HandleMessage(b.handleWhatsNewCommand, th.CommandEqual("whatsnew"))
	bh.HandleMessage(b.handleWhatsNewCommand, th.CommandEqual("whatnew"))
	bh.HandleMessage(b.handlePrivateStart, th.CommandEqual("start"))
	bh.HandleMessage(b.handleHelpCommand, th.CommandEqual("help"))
	bh.HandleMessage(b.handlePrivateText, privateMessagePredicate) // флоу ввода текста приветствия
	bh.HandleMessage(b.handleGroupMessage)                         // фолбэк: сервис-сообщения + счётчики в группах
	bh.HandleEditedMessage(b.handleEditedGroupMessage)             // спам-чек правок (обход «невинный текст → правка в спам»)

	return bh.Start()
}

// privateMessagePredicate матчит некомандные сообщения в личке, которые
// провалились сквозь командные хендлеры выше.
func privateMessagePredicate(_ context.Context, update telego.Update) bool {
	return update.Message != nil && update.Message.Chat.Type == telego.ChatTypePrivate
}

func (b *Bot) setCommands(ctx context.Context) error {
	// Сносим команды, ранее объявленные в default- и групповых scope — меню
	// «/» в группах должно быть пустым.
	_ = b.api.DeleteMyCommands(ctx, &telego.DeleteMyCommandsParams{
		Scope: &telego.BotCommandScopeAllGroupChats{Type: "all_group_chats"},
	})
	_ = b.api.DeleteMyCommands(ctx, &telego.DeleteMyCommandsParams{
		Scope: &telego.BotCommandScopeAllChatAdministrators{Type: "all_chat_administrators"},
	})
	_ = b.api.DeleteMyCommands(ctx, &telego.DeleteMyCommandsParams{
		Scope: &telego.BotCommandScopeDefault{Type: "default"},
	})

	// Меню команд «/» видно только в личке.
	return b.api.SetMyCommands(ctx, &telego.SetMyCommandsParams{
		Scope: &telego.BotCommandScopeAllPrivateChats{Type: "all_private_chats"},
		Commands: []telego.BotCommand{
			{Command: "start", Description: "Меню"},
			{Command: "help", Description: "Все команды бота"},
			{Command: "whatsnew", Description: "Что нового в боте"},
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
			// Истекла, пока бот лежал — считаем таймаутом немедленно.
			expires = now.Add(1 * time.Second)
		}
		p := b.store.Put(row.ChatID, row.UserID, row.MessageID, row.CorrectIdx, expires, row.ThreadID, row.EphemeralID)
		b.goSafe("waitTimeout", func() { b.waitTimeout(p) })
	}
	b.log.Info("restored pending captchas", "count", len(rows))
	return len(rows), nil
}

// goSafe запускает fn в горутине с recover: паника логируется, процесс живёт.
// Трейд-офф: упавший фоновый луп тихо умирает до рестарта (виден только
// log.Error) — это лучше, чем ронять бота во всех чатах разом.
func (b *Bot) goSafe(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				b.log.Error("panic in goroutine", "name", name,
					"recovered", r, "stack", string(debug.Stack()))
			}
		}()
		fn()
	}()
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
