CREATE TABLE IF NOT EXISTS pending_captchas (
    chat_id     INTEGER NOT NULL,
    user_id     INTEGER NOT NULL,
    message_id  INTEGER NOT NULL,
    correct_idx INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
    thread_id   INTEGER NOT NULL DEFAULT 0, -- топик форума, в котором вошёл юзер; 0 = без топика
    ephemeral_msg_id INTEGER NOT NULL DEFAULT 0, -- ≠0: капча эфемерная, удалять по этому id
    -- Стадия серии капчи (1..3): какое сообщение серии сейчас живо. Рестарт
    -- продолжает серию с этой стадии, а не с начала.
    stage       INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (chat_id, user_id)
);

CREATE TABLE IF NOT EXISTS attempts (
    chat_id    INTEGER NOT NULL,
    user_id    INTEGER NOT NULL,
    count      INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (chat_id, user_id)
);

CREATE TABLE IF NOT EXISTS events (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    chat_id INTEGER NOT NULL,
    user_id INTEGER NOT NULL,
    kind    TEXT    NOT NULL, -- 'join' | 'pass' | 'kick' | 'ban' | 'spamban' | 'left' | 'abort' | 'mute' | 'suspect'
    at      INTEGER NOT NULL,
    -- Причина кика/бана: 'captcha' | 'noreply' | 'mod:<adminID>' |
    -- 'vote:<id,id,...>' | 'global'. NULL/'' — нет (join/pass и старые строки).
    reason  TEXT
);
CREATE INDEX IF NOT EXISTS idx_events_chat_at ON events(chat_id, at);
CREATE INDEX IF NOT EXISTS idx_events_chat_kind_at ON events(chat_id, kind, at);
-- Под коррелированные подзапросы PassedUsers/TopFailers/EventUsers
-- (поиск последнего join/pass конкретного юзера без скана всех событий чата).
CREATE INDEX IF NOT EXISTS idx_events_chat_user_kind_at ON events(chat_id, user_id, kind, at);

CREATE TABLE IF NOT EXISTS members (
    chat_id   INTEGER NOT NULL,
    user_id   INTEGER NOT NULL,
    joined_at INTEGER NOT NULL,
    PRIMARY KEY (chat_id, user_id)
);

CREATE TABLE IF NOT EXISTS message_counts (
    chat_id        INTEGER NOT NULL,
    day            TEXT    NOT NULL, -- 'YYYY-MM-DD', день по МСК (storage.DayOf)
    newcomer_count INTEGER NOT NULL DEFAULT 0,
    oldtimer_count INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (chat_id, day)
);

-- Активность по (chat, user): накопительные счётчики + время первого/последнего
-- сообщения. Нужна для детекта тишины и накопительных топов.
CREATE TABLE IF NOT EXISTS user_activity (
    chat_id          INTEGER NOT NULL,
    user_id          INTEGER NOT NULL,
    first_message_at INTEGER,
    last_message_at  INTEGER,
    message_count    INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (chat_id, user_id)
);

-- Дневные счётчики сообщений по юзерам — для запросов топа писателей за окно времени.
CREATE TABLE IF NOT EXISTS user_message_counts (
    chat_id INTEGER NOT NULL,
    user_id INTEGER NOT NULL,
    day     TEXT    NOT NULL,
    count   INTEGER NOT NULL,
    PRIMARY KEY (chat_id, user_id, day)
);
CREATE INDEX IF NOT EXISTS idx_umc_chat_day ON user_message_counts(chat_id, day);
CREATE INDEX IF NOT EXISTS idx_umc_chat_user ON user_message_counts(chat_id, user_id);
-- Кросс-чатовое доверие антиспама: выборка по одному юзеру без chat_id.
CREATE INDEX IF NOT EXISTS idx_umc_user ON user_message_counts(user_id, chat_id);

-- Кэш отображаемых имён, чтобы рендерить упоминания без похода в Telegram на каждый /stats.
CREATE TABLE IF NOT EXISTS user_info (
    user_id    INTEGER PRIMARY KEY,
    first_name TEXT,
    last_name  TEXT,
    username   TEXT,
    updated_at INTEGER NOT NULL
);
-- Резолв @username в модкомандах без полного скана вечной таблицы.
CREATE INDEX IF NOT EXISTS idx_user_info_username ON user_info(username COLLATE NOCASE);

-- Известные чаты: пополняются попутно из каждого видимого нами chat_member-
-- и message-апдейта. Используются меню /chats для списка чатов (владелец бота
-- видит все, админ чата — свои).
CREATE TABLE IF NOT EXISTS chats (
    chat_id         INTEGER PRIMARY KEY,
    title           TEXT,
    type            TEXT,
    username        TEXT,
    -- Когда бота впервые увидели в чате (my_chat_member «бот присутствует»).
    -- NULL = чат жил до введения колонки; /info тогда показывает фолбэк по
    -- самому раннему событию. Пишется один раз, переносится при апгрейде
    -- basic group → supergroup (MigrateChat строку реестра удаляет).
    bot_added_at    INTEGER,
    updated_at      INTEGER NOT NULL,
    -- Статус подтверждения чата владельцем бота: 'approved' | 'pending' |
    -- 'rejected'. 'approved' (в т.ч. строки, созданные до фичи) — бот
    -- работает; 'pending' — ждёт решения владельца, чат инертен;
    -- 'rejected' — владелец отклонил, бот не активен (обычно выходит).
    approval_status TEXT NOT NULL DEFAULT 'approved'
);

-- Настраиваемое пер-чатовое поведение. Нет строки = дефолты (приветствие
-- включено, attempts/интервал серий берутся из глобального конфига, ежедневные
-- дайджесты выключены).
--
-- Nullable-колонки (max_attempts, captcha_interval_minutes) означают
-- «использовать глобальный env-дефолт»; non-null значение его переопределяет.
CREATE TABLE IF NOT EXISTS chat_settings (
    chat_id                 INTEGER PRIMARY KEY,
    greeting_enabled        INTEGER NOT NULL DEFAULT 1,
    max_attempts            INTEGER,
    captcha_timeout_seconds INTEGER, -- легаси (секунды одиночного таймаута); заменён на captcha_interval_minutes
    captcha_interval_minutes INTEGER, -- NULL = дефолт; интервал между сообщениями серии капчи/напоминаний
    daily_stats_enabled     INTEGER NOT NULL DEFAULT 0,
    daily_stats_utc_hour    INTEGER,
    last_daily_stats_day    TEXT,
    captcha_mode            TEXT,
    greeting_text           TEXT, -- NULL = встроенное приветствие по умолчанию
    -- JSON-массив telego.MessageEntity кастомного приветствия (жирный/курсив,
    -- офсеты в UTF-16); NULL = шаблон плоский, рендер экранирует как раньше.
    greeting_entities       TEXT,
    silent_announce_enabled INTEGER NOT NULL DEFAULT 1,
    spam_check_enabled      INTEGER NOT NULL DEFAULT 0,
    spam_threshold          INTEGER, -- легаси (вердикт теперь бинарный); колонка живёт ради миграций
    spam_whitelist_msgs     INTEGER, -- NULL = 5; сообщений до белого списка
    spam_vote_margin        INTEGER, -- NULL = 3; перевес голосов для вердикта
    reply_check_enabled     INTEGER NOT NULL DEFAULT 0, -- режим «требовать ответа»
    reply_check_seconds     INTEGER, -- легаси (серия напоминаний живёт на captcha_interval_minutes); не читается
    ephemeral_enabled       INTEGER NOT NULL DEFAULT 0  -- служебные сообщения эфемерно (Bot API 10.2)
);

-- Приветствия бота по (chat, user): помним message_id, чтобы при спам-бане
-- снести и «Добро пожаловать, X!» — revoke стирает только сообщения юзера.
-- Повторный вход перезаписывает строку; старше 48 ч чистятся свипером
-- (Telegram всё равно не даёт боту удалять сообщения старше 48 ч).
CREATE TABLE IF NOT EXISTS greetings (
    chat_id    INTEGER NOT NULL,
    user_id    INTEGER NOT NULL,
    message_id INTEGER NOT NULL,
    sent_at    INTEGER NOT NULL,
    PRIMARY KEY (chat_id, user_id)
);

-- Активные голосования «спам/не спам»: плашка бота под подозрительным
-- сообщением. Всё состояние в БД — переживает рестарты без таймеров.
CREATE TABLE IF NOT EXISTS spam_votes (
    chat_id       INTEGER NOT NULL,
    bot_msg_id    INTEGER NOT NULL, -- сообщение-плашка с кнопками
    target_msg_id INTEGER NOT NULL, -- подозрительное сообщение
    author_id     INTEGER NOT NULL,
    -- Кто запустил голосование командой /spam; 0 = плашка ИИ (спам-чек или
    -- профиль). Инициатор исключён из голосования в своём репорте.
    initiator_id  INTEGER NOT NULL DEFAULT 0,
    prob          INTEGER NOT NULL, -- legacy NOT NULL: всегда 100, никогда не показывается
    created_at    INTEGER NOT NULL,
    PRIMARY KEY (chat_id, bot_msg_id)
);

CREATE TABLE IF NOT EXISTS spam_ballots (
    chat_id    INTEGER NOT NULL,
    bot_msg_id INTEGER NOT NULL,
    voter_id   INTEGER NOT NULL,
    is_spam    INTEGER NOT NULL,
    PRIMARY KEY (chat_id, bot_msg_id, voter_id)
);

-- Общая база спамеров: вердикт «спам» в любом чате баним во всех чатах бота,
-- а при входе такого юзера в новый чат — мгновенный бан вместо капчи.
-- Ручной разбан админом в любом чате снимает флаг (DeleteSpamBanned из
-- handleChatMember) — иначе ошибочный вердикт был бы неисправим.
CREATE TABLE IF NOT EXISTS spam_banned (
    user_id INTEGER PRIMARY KEY,
    chat_id INTEGER NOT NULL, -- чат, где вынесен вердикт
    at      INTEGER NOT NULL
);

-- Ожидания «ответь на приветствие» (режим reply_check): после капчи юзер
-- обязан написать что-нибудь до expires_at, иначе кик. Переживают рестарт
-- по образцу pending_captchas. Приветствие-якорь сносится при кике за
-- молчание по id из таблицы greetings (TakeGreetingMsg).
CREATE TABLE IF NOT EXISTS pending_replies (
    chat_id         INTEGER NOT NULL,
    user_id         INTEGER NOT NULL,
    expires_at      INTEGER NOT NULL,
    -- Стадия серии напоминаний (1..3): какое приветствие-якорь сейчас живо.
    stage           INTEGER NOT NULL DEFAULT 1,
    -- Топик форума для повторных отправок якоря; 0 = без топика.
    thread_id       INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (chat_id, user_id)
);

-- Доверенные (chat, user): добавлены админом командой /whitelist — входят в
-- чат без капчи, reply-ожидания и профиль-чека; доверие в чате перекрывает и
-- глобальную базу спамеров при входе в ЭТОТ чат. Снимается только вручную
-- (удалением строки из БД).
CREATE TABLE IF NOT EXISTS trusted_users (
    chat_id INTEGER NOT NULL,
    user_id INTEGER NOT NULL,
    at      INTEGER NOT NULL,
    PRIMARY KEY (chat_id, user_id)
);

-- Служебные метки бота (ключ-значение): например, announced_version —
-- последняя версия, о которой разосланы ЛС-оповещения.
CREATE TABLE IF NOT EXISTS bot_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Глобальные (не пер-чатовые) настройки ЛС-уведомлений. Имя историческое:
-- spam_notify/mod_notify доступны только владельцам (OWNER_IDS), а
-- daily_report — и админам чатов, так что owner_id хранит любой user id.
CREATE TABLE IF NOT EXISTS owner_settings (
    owner_id        INTEGER PRIMARY KEY,
    spam_notify     INTEGER NOT NULL DEFAULT 0, -- слать в ЛС подозрения и вердикты
    mod_notify      INTEGER NOT NULL DEFAULT 0, -- слать в ЛС кики/баны (капча со 2-й попытки, молчание, /kick, /ban) и проходы капчи
    daily_report    INTEGER NOT NULL DEFAULT 0, -- слать в ЛС утреннюю сводку за вчера по чатам юзера
    last_report_day TEXT,                       -- маркер отправки сводки (день МСК), NULL = ещё не слали
    captcha_notify  INTEGER NOT NULL DEFAULT 0, -- слать в ЛС ВСЕ провалы капчи (mod_notify шлёт только повторные)
    version_notify  INTEGER NOT NULL DEFAULT 1, -- слать ЛС «бот обновлён»; единственный opt-out тумблер (нет строки = ВКЛ)
    last_stats_period TEXT                      -- последний выбранный период статистики DM-меню, NULL = неделя
);
