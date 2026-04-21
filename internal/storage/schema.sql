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
