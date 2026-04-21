CREATE TABLE IF NOT EXISTS pending_captchas (
    chat_id     INTEGER NOT NULL,
    user_id     INTEGER NOT NULL,
    message_id  INTEGER NOT NULL,
    correct_idx INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
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
    kind    TEXT    NOT NULL, -- 'join' | 'pass' | 'kick' | 'ban'
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
