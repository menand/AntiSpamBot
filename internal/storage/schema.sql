CREATE TABLE IF NOT EXISTS pending_captchas (
    chat_id     INTEGER NOT NULL,
    user_id     INTEGER NOT NULL,
    message_id  INTEGER NOT NULL,
    correct_idx INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
    thread_id   INTEGER NOT NULL DEFAULT 0, -- forum topic the user joined in; 0 = no topic
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
    kind    TEXT    NOT NULL, -- 'join' | 'pass' | 'kick' | 'ban' | 'spamban'
    at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_chat_at ON events(chat_id, at);
CREATE INDEX IF NOT EXISTS idx_events_chat_kind_at ON events(chat_id, kind, at);

CREATE TABLE IF NOT EXISTS members (
    chat_id   INTEGER NOT NULL,
    user_id   INTEGER NOT NULL,
    joined_at INTEGER NOT NULL,
    PRIMARY KEY (chat_id, user_id)
);

CREATE TABLE IF NOT EXISTS message_counts (
    chat_id        INTEGER NOT NULL,
    day            TEXT    NOT NULL, -- 'YYYY-MM-DD' UTC
    newcomer_count INTEGER NOT NULL DEFAULT 0,
    oldtimer_count INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (chat_id, day)
);

-- Per-user per-chat activity: cumulative counts + first/last message timestamps.
-- Used for silence detection and cumulative top lists.
CREATE TABLE IF NOT EXISTS user_activity (
    chat_id          INTEGER NOT NULL,
    user_id          INTEGER NOT NULL,
    first_message_at INTEGER,
    last_message_at  INTEGER,
    message_count    INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (chat_id, user_id)
);

-- Per-user per-day message counts for top-writers queries over a time window.
CREATE TABLE IF NOT EXISTS user_message_counts (
    chat_id INTEGER NOT NULL,
    user_id INTEGER NOT NULL,
    day     TEXT    NOT NULL,
    count   INTEGER NOT NULL,
    PRIMARY KEY (chat_id, user_id, day)
);
CREATE INDEX IF NOT EXISTS idx_umc_chat_day ON user_message_counts(chat_id, day);
CREATE INDEX IF NOT EXISTS idx_umc_chat_user ON user_message_counts(chat_id, user_id);

-- Cache of display names so we can render mentions without calling Telegram on every /stats.
CREATE TABLE IF NOT EXISTS user_info (
    user_id    INTEGER PRIMARY KEY,
    first_name TEXT,
    last_name  TEXT,
    username   TEXT,
    updated_at INTEGER NOT NULL
);

-- Known chats: populated opportunistically from every chat_member and message
-- update we see. Used by the owner-only /chats menu to list chats.
CREATE TABLE IF NOT EXISTS chats (
    chat_id    INTEGER PRIMARY KEY,
    title      TEXT,
    type       TEXT,
    updated_at INTEGER NOT NULL
);

-- Per-chat configurable behavior. Absent row = defaults (greeting enabled,
-- attempts/timeout fall back to global config, daily digests off).
--
-- Nullable columns (max_attempts, captcha_timeout_seconds) mean "use the
-- global env default"; a non-null value overrides globally.
CREATE TABLE IF NOT EXISTS chat_settings (
    chat_id                 INTEGER PRIMARY KEY,
    greeting_enabled        INTEGER NOT NULL DEFAULT 1,
    max_attempts            INTEGER,
    captcha_timeout_seconds INTEGER,
    daily_stats_enabled     INTEGER NOT NULL DEFAULT 0,
    daily_stats_utc_hour    INTEGER,
    last_daily_stats_day    TEXT,
    captcha_mode            TEXT,
    greeting_text           TEXT, -- NULL = built-in default greeting
    silent_announce_enabled INTEGER NOT NULL DEFAULT 1,
    spam_check_enabled      INTEGER NOT NULL DEFAULT 0,
    spam_threshold          INTEGER, -- NULL = 90; порог вероятности спама (%)
    spam_whitelist_msgs     INTEGER, -- NULL = 5; сообщений до белого списка
    spam_vote_margin        INTEGER  -- NULL = 3; перевес голосов для вердикта
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
    prob          INTEGER NOT NULL, -- вердикт Groq (%)
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
