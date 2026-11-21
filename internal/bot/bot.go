package bot

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/menand/AntiSpamBot/internal/captcha"
	"github.com/menand/AntiSpamBot/internal/config"
	"github.com/menand/AntiSpamBot/internal/gemini"
	"github.com/menand/AntiSpamBot/internal/gigachat"
	"github.com/menand/AntiSpamBot/internal/groq"
	"github.com/menand/AntiSpamBot/internal/storage"
)

const attemptsTTL = 24 * time.Hour

// eventRetentionDays — сколько дней хранить события воронки: таблица events
// до сих пор росла вечно, а единственное соединение SQLite делало каждый
// digest-запрос медленнее. 180 дней с запасом покрывают все окна статистики
// (день/неделя/месяц) и списки «10 последних» модкоманд.
const eventRetentionDays = 180

// eventPruneBatch — порция одного DELETE при подрезке: одна инструкция не
// должна подолгу держать единственное соединение.
const eventPruneBatch = 5000

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

	// Активные карантины новичков (зеркало chat_quarantine) — горячий путь
	// цензора: каждое сообщение ограниченного юзера проверяется из памяти.
	quar *quarantineStore

	// Reply-watch проверяет служебный чат перед каждой пробой.
	// Неудачное удаление копий отключает пробы до перезапуска.
	replyWatchBrokenNotified atomic.Bool
	replyWatchBlocked        atomic.Bool
	replyWatchDeleteFailures atomic.Int32
	replyWatchChats          sync.Map

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

	// ИИ-антиспам: Groq-клиент (первичный), Gemini (средний фолбек),
	// GigaChat (запасной); порядок цепочки — cfg.AIProviderOrder. Дедуп
	// запущенных проверок (chat:user) и кэш «этот юзер — админ чата» (для
	// белого списка и золотого голоса).
	groqc        llmClassifier
	gemic        llmClassifier
	gigac        llmClassifier
	spamMu       sync.Mutex
	spamInflight map[chatUser]struct{}
	rights       memberRightsQueue
	// Кэш флага и контекста ИИ-проверок чата. Выключение отменяет
	// текущий контекст, включая запасные попытки цепочки провайдеров.
	spamGateMu    sync.Mutex
	spamGateCache map[int64]spamGate
	// editChecked — время последней спам-проверки ПРАВКИ по (chat, user):
	// правки не инкрементят счётчик сообщений, и без кулдауна новичок мог бы
	// бесконечными правками одного сообщения жечь LLM-квоту. Unbounded по
	// тому же соглашению, что userCache (запись ~40 байт на активного юзера).
	editChecked map[chatUser]time.Time
	// profileChecked — время последнего ИИ-чека ПРОФИЛЯ по юзеру (глобально,
	// не per-chat): чек срабатывает на каждом pass/approve, и цикл
	// «вышел-зашёл» без кулдауна жёг бы LLM-квоту на каждый проход.
	// Unbounded по тому же соглашению, что editChecked.
	profileChecked map[int64]time.Time
	// digestSent — in-process маркер «сводка за today уже ушла в чат»:
	// страховка от видимого дубля, если запись маркера в БД упала после
	// успешной отправки. Авторитетен между рестартами всё равно
	// last_daily_stats_day.
	digestMu   sync.Mutex
	digestSent map[int64]string
	// toggleMu сериализует read-modify-write тогглов настроек: telego
	// обрабатывает колбэки параллельно, и даблклик без него дважды читал бы
	// одно значение, записывая одну инверсию вместо двух.
	toggleMu   sync.Mutex
	adminMu    sync.Mutex
	adminCache map[chatUser]adminCacheEntry

	// Кэш статуса подтверждения чатов (chats.approval_status): частые чтения
	// на каждом групповом сообщении (гейт chatServiceable), редкие записи
	// (апрув/отклонение владельцем). Неограниченный размер по тому же
	// соглашению, что chatCache (запись ~1 байт на известный чат).
	approvalMu    sync.Mutex
	approvalCache map[int64]bool

	// leaveInflight — чаты, из которых бот прямо сейчас выводится по запросу
	// владельца (menu:leavec). Защита от двойного клика по «Да, выйти»: второй
	// вызов сделал бы повторный LeaveChat, который после успешного первого
	// завершился бы ошибкой и показал бы «не получилось» поверх удачного выхода.
	leaveMu       sync.Mutex
	leaveInflight map[int64]bool

	// The update handlers produce capture entries. Run stops them before
	// closing the queue and waiting for the writer.
	captureMu   sync.RWMutex
	captureCh   chan storage.StoredMessage
	captureDone chan struct{}
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
		api:            api,
		cfg:            cfg,
		store:          captcha.NewStore(),
		replies:        newReplyStore(),
		quar:           newQuarantineStore(),
		log:            log,
		version:        version,
		chatCache:      make(map[int64]storage.ChatInfo),
		userCache:      make(map[int64]storage.UserInfo),
		greetInput:     make(map[int64]greetInputState),
		groqc:          groq.New(cfg.GroqAPIKey, cfg.GroqModel),
		gemic:          gemini.New(cfg.GeminiAPIKey, cfg.GeminiModel),
		gigac:          gigachat.New(cfg.GigaChatAuthKey, cfg.GigaChatScope, cfg.GigaChatModel, groq.SystemPrompt),
		spamInflight:   make(map[chatUser]struct{}),
		editChecked:    make(map[chatUser]time.Time),
		profileChecked: make(map[int64]time.Time),
		digestSent:     make(map[int64]string),
		adminCache:     make(map[chatUser]adminCacheEntry),
		approvalCache:  make(map[int64]bool),
		spamGateCache:  make(map[int64]spamGate),
		leaveInflight:  make(map[int64]bool),
	}, nil
}

func (b *Bot) Username() string {
	if b.me == nil {
		return ""
	}
	return b.me.Username
}

func (b *Bot) Run(parent context.Context) error {
	ctx, stopRun := context.WithCancel(parent)
	defer stopRun()
	b.runCtx = ctx
	b.startedAt = time.Now()

	db, err := storage.Open(ctx, b.cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open storage: %w", err)
	}
	b.db = db
	var bh *th.BotHandler
	announced := false
	defer func() {
		b.stopUpdateHandling(stopRun, bh)
		if announced {
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			b.notifyOwners(shutCtx, "🔴 <b>Бот остановлен</b>")
		}
		_ = db.Close()
	}()

	me, err := b.api.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("get me: %w", err)
	}
	b.me = me

	muteRestoreCtx, cancelMuteRestore := context.WithTimeout(ctx, 30*time.Second)
	b.reconcileManualMutes(muteRestoreCtx)
	cancelMuteRestore()

	punishRestoreCtx, cancelPunishRestore := context.WithTimeout(ctx, 30*time.Second)
	b.reconcilePunishments(punishRestoreCtx)
	cancelPunishRestore()
	restored, err := b.retryPendingRestore(ctx, "captchas", b.restorePending)
	if err != nil {
		return err
	}
	if _, err := b.retryPendingRestore(ctx, "replies", b.restorePendingReplies); err != nil {
		return err
	}
	b.restoreQuarantine(ctx)

	// Синтетические замки golden-/reply-spam-репортов (bot_msg_id <= 0) —
	// координация ТОЛЬКО внутри живого процесса: крахнувший репорт некому
	// продолжить. Чистим синхронно и строго ДО приёма апдейтов: из
	// reconcile-горутины (идёт параллельно приёму — reconcileChats тянется
	// getChat'ами секундами) вызов мог бы снести замок живого репорта, и
	// второй исполнитель открыл бы второй бан. Ноль — легаси-замки прежнего
	// общего sentinel: пережив крах, они блокировали бы репорты автора до
	// суточного свипа (см. DeleteSpamLocks).
	if n, err := b.db.DeleteSpamLocks(ctx); err != nil {
		b.log.Warn("delete spam locks", "err", err)
	} else if n > 0 {
		b.log.Info("stale spam locks deleted", "count", n)
	}

	b.goSafe("attemptsSweepLoop", func() { b.attemptsSweepLoop(ctx) })
	b.goSafe("dailyDigestLoop", func() { b.dailyDigestLoop(ctx) })
	b.goSafe("reconcileChats", func() {
		// Реконсиляция голосов — строго ПОСЛЕ сверки реестра: чат, из
		// которого бота кикнули офлайн, ещё читается approved (my_chat_member
		// не приходит), и стартовый гейт chatServiceable в reconcileSpamVotes
		// выиграл бы гонку и исполнил вердикт мёртвого чата.
		b.reconcileChats(ctx)
		b.goSafe("manualMuteRetryLoop", func() { b.manualMuteRetryLoop(ctx) })
		b.goSafe("captchaRestoreLoop", func() { b.captchaRestoreLoop(ctx) })
		b.goSafe("admissionRetryLoop", func() { b.admissionRetryLoop(ctx) })
		b.reconcileSpamVotes(ctx)
		b.goSafe("spamVerdictRetryLoop", func() { b.spamVerdictRetryLoop(ctx) })
		// Карантин-реконсиляция — здесь же, в стартовой горутине: крэш между
		// выключением тоггла и отпуском юзеров не должен пережить рестарт
		// (restoreQuarantine уже отфильтровал такие чаты, но страховой свип
		// ловит и БД-правки мимо кода).
		b.reconcileQuarantines(ctx)
		// Стартовый свип истёкших голосований — здесь же, последним:
		// spamVoteSweepLoop начинает только с часового тика, и без этого
		// вызова голосования, проспавшие дедлайн за время простоя, висели
		// бы до первого часового тика.
		b.sweepExpiredVotes(ctx)
	})
	b.goSafe("spamVoteSweepLoop", func() { b.spamVoteSweepLoop(ctx) })
	b.goSafe("announceVersion", func() { b.announceVersion(ctx) })
	b.startCaptureWriter()
	// Архиватор работает даже при выключенном захвате, чтобы обработать
	// накопленные ранее горячие строки.
	b.goSafe("archiveLoop", func() { b.archiveLoop(ctx) })
	if b.cfg.ReplyWatchEnabled {
		// Диагностика/свип — в той же горутине, что и цикл: сетевые проверки
		// (getChat/getChatMember) не должны задерживать приём апдейтов.
		b.goSafe("replyWatchLoop", func() {
			b.setupReplyWatch(ctx)
			b.replyWatchLoop(ctx)
		})
	} else if err := b.db.DeleteAllReplyWatches(ctx); err != nil {
		// Наблюдения — короткоживущее состояние (час). Выключенная фича не
		// должна оставить «бомбу»: при повторном включении старые строки
		// разом созрели бы и завалили владельца отчётами о давно прожитых
		// сообщениях.
		b.log.Warn("reply watch: clear disabled watches", "err", err)
	}
	var providers []string
	for _, p := range b.aiProviders() {
		if p.c.Enabled() {
			providers = append(providers, p.name+":"+p.c.Model())
		}
	}
	if len(providers) == 0 {
		b.log.Info("AI spam analysis unavailable: neither GROQ_API_KEY, GEMINI_API_KEY nor GIGACHAT_AUTH_KEY is set")
	} else {
		// Порядок в списке = порядок цепочки classifyVerdict (AI_PROVIDER_ORDER).
		b.log.Info("AI spam analysis available", "providers", strings.Join(providers, ", "))
	}

	b.notifyOwners(ctx, fmt.Sprintf(
		"🟢 <b>Бот запущен</b>\nЮзернейм: @%s\nВерсия: <code>%s</code>\nВосстановлено капч: %d",
		b.Username(), b.version, restored))

	announced = true

	updates, err := b.api.UpdatesViaLongPolling(ctx, &telego.GetUpdatesParams{
		AllowedUpdates: []string{"message", "edited_message", "callback_query", "chat_member", "my_chat_member"},
	})
	if err != nil {
		return fmt.Errorf("long polling: %w", err)
	}

	bh, err = th.NewBotHandler(b.api, updates)
	if err != nil {
		return fmt.Errorf("bot handler: %w", err)
	}

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

	// Цензор карантина — ДО reply-wait middleware: сообщение новичка, ещё
	// находящегося в карантине (рестрикт «только текст» применён при проходе
	// капчи), проверяется на форварды и ссылки — серверная грануляция прав их
	// не перекрывает. Нарушение удаляется; возврат без ctx.Next глотает апдейт:
	// сообщение не попадает в счётчики, не жечёт LLM-квоту и НЕ снимает
	// reply-wait (спам-ссылка не может быть «ответом на приветствие»).
	bh.Use(func(ctx *th.Context, update telego.Update) error {
		if m := update.Message; m != nil && m.From != nil && !m.From.IsBot {
			if m.Chat.Type == "group" || m.Chat.Type == "supergroup" {
				// Тот же гейт, что у handleGroupMessage: pending/rejected и
				// чужие чаты полностью инертны, цензор там не работает.
				if !b.chatServiceable(m.Chat.ID) {
					return ctx.Next(update)
				}
				if e, ok := b.quar.Get(m.Chat.ID, m.From.ID); ok {
					// Админ/владелец карантину не цель: серверный рестрикт с
					// промоушена поднимает сам Telegram, и проверяется он
					// только для КАРАНТИННОГО юзера. Проверка живая на
					// промахе кэша (on forfeit можно), но НЕ карательная:
					// !sure (API-ошибка) трактуется «не цензурим» — удалять
					// сообщения «на всякий случай» по одному 429 нечестно
					// к админу (тот же трейд-офф, что у punishNonAdmin).
					if b.isOwner(m.From.ID) {
						return ctx.Next(update)
					}
					isAdmin, sure := b.isChatAdminVerified(ctx, m.Chat.ID, m.From.ID)
					if isAdmin || !sure {
						return ctx.Next(update)
					}
					if quarantineViolates(m) {
						b.goSafe("quarantineCensor", func() { b.censorQuarantineMessage(m, e) })
						return nil
					}
				}
			}
		}
		return ctx.Next(update)
	})

	// Снятие ожидания «ответь на приветствие» — в middleware, ДО маршрутизации:
	// иначе команда (/start и т.п.) первым сообщением новичка ушла бы в свой
	// хендлер мимо handleGroupMessage, и юзера кикнуло бы за молчание, хотя он
	// написал. Дёшево (in-memory Get-промах для всех, у кого ожидания нет).
	// Только НАСТОЯЩИЙ контент (текст/медиа): сервисное сообщение (новичок
	// добавил участника — тоже с From) не должно засчитываться за ответ.
	// Фильтр осмысленности и судьба мусора («+», «привет», смайлик, стикер,
	// голая ссылка → удаление, эфемерная подсказка, разовые +5 минут, апдейт
	// глотается) — в replyWaitMiddleware (replywait.go).
	bh.Use(func(ctx *th.Context, update telego.Update) error {
		if m := update.Message; m != nil && m.From != nil && !m.From.IsBot {
			if (m.Chat.Type == "group" || m.Chat.Type == "supergroup") &&
				messageHasUserContent(m) {
				if b.replyWaitMiddleware(m) {
					return nil
				}
			}
			// ЛС-команда перехватывается своим хендлером мимо handlePrivateText
			// — разряжаем взведённый ввод приветствия здесь, до маршрутизации,
			// иначе он молча пережил бы команду и поймал следующее сообщение.
			if m.Chat.Type == "private" {
				b.cancelGreetingInputOnCommand(ctx, *m)
			}
		}
		return ctx.Next(update)
	})

	bh.Handle(b.handleChatMember, th.AnyChatMember())
	bh.Handle(b.handleMyChatMember, th.AnyMyChatMember())
	bh.HandleCallbackQuery(b.handleCallback, th.AnyCallbackQueryWithMessage(), th.CallbackDataPrefix("cap:"))              // cap:<userID>:<optIdx>
	bh.HandleCallbackQuery(b.handleApproveCallback, th.AnyCallbackQueryWithMessage(), th.CallbackDataPrefix("capok:"))     // capok:<userID>
	bh.HandleCallbackQuery(b.handleReplyApproveCallback, th.AnyCallbackQueryWithMessage(), th.CallbackDataPrefix("rpok:")) // rpok:<userID>
	bh.HandleCallbackQuery(b.handleReplySpamCallback, th.AnyCallbackQueryWithMessage(), th.CallbackDataPrefix("rpspam:"))  // rpspam:<userID> (first click)
	bh.HandleCallbackQuery(b.handleReplySpamCallback, th.AnyCallbackQueryWithMessage(), th.CallbackDataPrefix("rpspamc:")) // rpspamc:<userID> (confirm)
	bh.HandleCallbackQuery(b.handleReplySpamCallback, th.AnyCallbackQueryWithMessage(), th.CallbackDataPrefix("rpspamx:")) // rpspamx:<userID> (cancel)
	// Общий обработчик проверяет личный диалог до разбора menu: и estats:.
	bh.HandleCallbackQuery(b.handleMenuCallback, th.AnyCallbackQueryWithMessage(), th.CallbackDataPrefix("menu:"))     // menu:main|help|add|chats|logs|aicheck|spamnotify|modnotify|capnotify|dreport|vernotify|stats|settings|welcome|wset|wtxt|daily|sil|spam|eph|qr|max|tmo|hour|swl|svm|qper|cmode|leave|leavec:<chatID>:<val> (gr|rpl|grtxt — legacy-алиасы: ре-рендер настроек / контекстный ввод)
	bh.HandleCallbackQuery(b.handleMenuCallback, th.AnyCallbackQueryWithMessage(), th.CallbackDataPrefix("estats:"))   // estats:<chatID>:<list>:<page>
	bh.HandleCallbackQuery(b.handleSpamVoteCallback, th.AnyCallbackQueryWithMessage(), th.CallbackDataPrefix("sv:"))   // sv:1|0 (vote)
	bh.HandleCallbackQuery(b.handleModChoiceCallback, th.AnyCallbackQueryWithMessage(), th.CallbackDataPrefix("mc:"))  // mc:<u|f|m|w>:<userID>|x
	bh.HandleCallbackQuery(b.handleApprovalCallback, th.AnyCallbackQueryWithMessage(), th.CallbackDataPrefix("appr:")) // appr:y|n:<chatID>
	bh.HandleMessage(b.handleStatsCommand, th.CommandEqual("stats"))
	bh.HandleMessage(b.handleChatsCommand, th.CommandEqual("chats"))
	bh.HandleMessage(b.handleLogsCommand, th.CommandEqual("logs"))
	bh.HandleMessage(b.handleInfoCommand, th.CommandEqual("info"))
	bh.HandleMessage(b.handleGreetingCommand, th.CommandEqual("greeting"))
	bh.HandleMessage(b.handleKickCommand, th.CommandEqual("kick"))
	bh.HandleMessage(b.handleBanCommand, th.CommandEqual("ban"))
	bh.HandleMessage(b.handleReplyWithBotCommand, th.CommandEqual("replywithbot"))
	bh.HandleMessage(b.handleReplyWithBotCommand, th.CommandEqual("rwb")) // алиас /replywithbot
	bh.HandleMessage(b.handleDeleteCommand, th.CommandEqual("del"))
	bh.HandleMessage(b.handleDeleteCommand, th.CommandEqual("delete"))
	bh.HandleMessage(b.handleMuteCommand, th.CommandEqual("mute"))
	bh.HandleMessage(b.handleSpamCommand, th.CommandEqual("spam"))
	bh.HandleMessage(b.handleUnbanCommand, th.CommandEqual("unban"))
	bh.HandleMessage(b.handleFullunbanCommand, th.CommandEqual("fullunban"))
	bh.HandleMessage(b.handleUnmuteCommand, th.CommandEqual("unmute"))
	bh.HandleMessage(b.handleWhitelistCommand, th.CommandEqual("whitelist"))
	bh.HandleMessage(b.handleTrustCommand, th.CommandEqual("trust"))
	bh.HandleMessage(b.handleWhatsNewCommand, th.CommandEqual("whatsnew"))
	bh.HandleMessage(b.handleWhatsNewCommand, th.CommandEqual("whatnew"))
	bh.HandleMessage(b.handlePrivateStart, th.CommandEqual("start"))
	bh.HandleMessage(b.handleHelpCommand, th.CommandEqual("help"))
	bh.HandleMessage(b.handlePrivateText, privateMessagePredicate) // флоу ввода текста приветствия
	bh.HandleMessage(b.handleGroupMessage)                         // фолбэк: сервис-сообщения + счётчики в группах
	bh.HandleEditedMessage(b.handleEditedGroupMessage)             // спам-чек правок (обход «невинный текст → правка в спам»)

	healthCtx, cancelHealth := context.WithCancel(ctx)
	defer cancelHealth()
	b.goSafe("healthbeat", func() { b.healthbeat(healthCtx, bh.IsRunning) })
	return bh.Start()
}

func (b *Bot) stopUpdateHandling(stop context.CancelFunc, handler *th.BotHandler) {
	stop()
	if handler != nil {
		_ = handler.Stop()
	}
	b.stopCaptureWriter()
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
			{Command: "chats", Description: "Мои чаты (для админов и владельца)"},
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
	mutes, err := b.manualMuteKeys(ctx)
	if err != nil {
		return 0, err
	}
	punishments, err := b.db.LoadPunishments(ctx)
	if err != nil {
		return 0, err
	}
	for _, p := range punishments {
		mutes[chatUser{p.ChatID, p.UserID}] = true
	}
	serviceable := make(map[int64]bool)
	for _, row := range rows {
		if mutes[chatUser{row.ChatID, row.UserID}] {
			continue
		}
		if err := b.checkPendingRestoreChat(ctx, row.ChatID, serviceable); err != nil {
			return 0, err
		}
	}
	now := time.Now()
	restored := 0
	// Общий бюджет проверки членства. Таймер перед санкцией повторяет
	// проверку роли, если на старте её подтвердить не удалось.
	lctx, lcancel := context.WithTimeout(ctx, 30*time.Second)
	defer lcancel()
	for _, row := range rows {
		if mutes[chatUser{row.ChatID, row.UserID}] {
			continue
		}
		if !serviceable[row.ChatID] {
			if b.chatAllowed(row.ChatID) && !row.Suspended {
				if err := b.db.DeletePendingIfMsg(ctx, row.ChatID, row.UserID, row.MessageID, row.EphemeralID); err != nil {
					b.log.Warn("delete pending captcha of unapproved chat", "err", err, "chat", row.ChatID, "user", row.UserID)
				}
			}
			continue
		}
		if row.Preparing() || row.Suspended {
			if err := b.restoreCaptchaCheckpoint(lctx, row); err != nil {
				b.log.Warn("restore captcha checkpoint", "err", err, "chat", row.ChatID, "user", row.UserID)
			}
			continue
		}
		if _, active := b.store.Get(row.ChatID, row.UserID); active {
			continue
		}
		if b.restoredCaptchaUserDeparted(lctx, row) {
			continue
		}
		expires := row.ExpiresAt
		expired := expires.Before(now)
		stage := row.Stage
		if expired {
			expires = now.Add(1 * time.Second)
			// Простой съел серию: рестарт считает её исчерпанной и по грейсу
			// исполняет наказание — как прежний одиночный таймаут. Кламп к
			// финальной стадии заставляет цикл карать, а не слать очередное
			// напоминание за время, которое юзер и так досидел офлайн.
			stage = captchaStages
		}
		p := b.store.Put(row.ChatID, row.UserID, row.MessageID, row.CorrectIdx, expires, row.ThreadID, row.EphemeralID, stage)
		// Живые строки доигрывают остаток серии со своей стадии; истёкшие —
		// наказываются секундным грейсом выше.
		b.goSafe("captchaStageLoop", func() { b.captchaStageLoop(row.ChatID, row.UserID, p) })
		restored++
	}
	b.log.Info("restored pending captchas", "count", restored, "total", len(rows))
	return restored, nil
}

// restoredCaptchaUserDeparted закрывает истёкшую за простой капчу юзера,
// который за это время вышел/был убран: left вместо фантомного кика. Капча-
// мьют пережил рестарт (Telegram хранит restriction) — снимаем его. true —
// исход решён здесь; ошибка API (включая исчерпанный общий бюджет проб) —
// false (наказываем по грейсу, как раньше).
func (b *Bot) restoredCaptchaUserDeparted(ctx context.Context, row storage.PendingRow) bool {
	m, err := b.api.GetChatMember(ctx, &telego.GetChatMemberParams{
		ChatID: tu.ID(row.ChatID),
		UserID: row.UserID,
	})
	if err != nil {
		b.log.Debug("restored captcha liveness", "err", err,
			"chat", row.ChatID, "user", row.UserID)
		return false
	}
	if m.MemberIsMember() {
		return false
	}
	if err := b.db.DeletePending(ctx, row.ChatID, row.UserID); err != nil {
		b.log.Warn("delete expired pending of departed user", "err", err)
	}
	b.recordLeftEvent(ctx, row.ChatID, row.UserID, "left while offline")
	// releaseOnAbort на живом ctx съел бы бюджет рестарта лестницей ретраев —
	// гоняем асинхронно на своём bounded-бюджете.
	if m.MemberStatus() == telego.MemberStatusLeft {
		rowChatID, rowUserID := row.ChatID, row.UserID
		if unlock, err := b.rights.lock(ctx, rowChatID, rowUserID, false); err == nil {
			b.goSafe("releaseDepartedRestore", func() {
				defer unlock()
				rctx, rcancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer rcancel()
				b.releaseOnAbort(rctx, rowChatID, rowUserID)
			})
		}
	}
	b.log.Info("expired captcha closed — user left while offline",
		"chat", row.ChatID, "user", row.UserID)
	return true
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
	quarantineRetry := time.NewTicker(quarantineReconcileInterval)
	defer quarantineRetry.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-quarantineRetry.C:
			b.reconcileQuarantines(ctx)
		case <-t.C:
			if err := b.db.SweepAttempts(ctx, attemptsTTL); err != nil {
				b.log.Warn("sweep attempts", "err", err)
			}
			// Тот же полусуточный тик подрезает историю событий порциями:
			// вечный рост events на единственном соединении был бомбой
			// замедленного действия для digest-запросов.
			if _, err := b.db.PruneEvents(ctx,
				time.Now().AddDate(0, 0, -eventRetentionDays), eventPruneBatch); err != nil {
				b.log.Warn("prune events", "err", err)
			}
			// И истёкшие карантины: срок кончился, рестрикт снял Telegram,
			// столбик не должен расти вечно. Зеркало чистится лениво при чтении.
			if err := b.db.PruneQuarantines(ctx, time.Now().Unix()); err != nil {
				b.log.Warn("prune quarantines", "err", err)
			}
			// Связки «ЛС-сообщение владельца → сообщение в чате»: месячная
			// карточка — уже не повод публиковать что-то «от имени бота»
			// задним числом.
			if _, err := b.db.PruneOwnerDMLinks(ctx, time.Now().Add(-ownerDMLinkTTL)); err != nil {
				b.log.Warn("prune owner dm links", "err", err)
			}
			// Там же — страхующий свип карантинов выключенных чатов (живой
			// путь отпуска при тоггл-выключении есть в menu.go; здесь —
			// истории незавершённых отпусков).
			b.reconcileQuarantines(ctx)
		}
	}
}
