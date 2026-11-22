package db

const schema = `
CREATE TABLE IF NOT EXISTS messages (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    platform     TEXT NOT NULL CHECK (length(platform) > 0),
    chat_type    TEXT NOT NULL DEFAULT 'private' CHECK (chat_type IN ('private', 'group')),
    conv_id      TEXT NOT NULL CHECK (length(conv_id) > 0),
    user_id      TEXT NOT NULL,                   -- 纯净用户ID (纯QQ号或OpenID)
    name         TEXT NOT NULL,
    text         TEXT NOT NULL,
    is_bot       INTEGER NOT NULL DEFAULT 0 CHECK (is_bot IN (0, 1)),
    created_at   INTEGER NOT NULL,                -- 毫秒级时间戳
    reply_to_uid TEXT NOT NULL DEFAULT '',
    reply_to_message_id INTEGER REFERENCES messages(id) ON DELETE SET NULL,
    platform_msg_id TEXT NOT NULL DEFAULT '',
    embedding    BLOB,
    embedding_model TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_messages_lookup ON messages(platform, chat_type, conv_id, id);
CREATE INDEX IF NOT EXISTS idx_messages_time ON messages(platform, chat_type, conv_id, created_at, id);
CREATE INDEX IF NOT EXISTS idx_messages_reset ON messages(conv_id, id);
CREATE INDEX IF NOT EXISTS idx_messages_reply_to_message ON messages(reply_to_message_id) WHERE reply_to_message_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_inbound_event ON messages(platform, chat_type, conv_id, platform_msg_id)
    WHERE is_bot = 0 AND platform_msg_id <> '';
CREATE INDEX IF NOT EXISTS idx_messages_reply_lookup ON messages(platform, chat_type, conv_id, user_id, is_bot, id DESC);
CREATE INDEX IF NOT EXISTS idx_messages_vector_lookup ON messages(platform, chat_type, conv_id, embedding_model, id DESC) WHERE embedding IS NOT NULL;

CREATE TABLE IF NOT EXISTS user_facts (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    platform     TEXT NOT NULL CHECK (length(platform) > 0),
    user_id      TEXT NOT NULL CHECK (length(user_id) > 0),
    scope_type   TEXT NOT NULL DEFAULT 'global' CHECK (scope_type IN ('global', 'group')),
    scope_id     TEXT NOT NULL DEFAULT '',
    fact         TEXT NOT NULL CHECK (length(trim(fact)) > 0),
    status       TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'superseded', 'deleted')),
    quote        TEXT NOT NULL DEFAULT '',        -- 来源用户原话
    created_at   INTEGER NOT NULL,                -- 毫秒级时间戳
    updated_at   INTEGER NOT NULL,
    embedding    BLOB,
    embedding_model TEXT NOT NULL DEFAULT '',
    CHECK ((scope_type = 'global' AND scope_id = '') OR (scope_type = 'group' AND length(scope_id) > 0))
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_facts_active_unique ON user_facts(platform, user_id, scope_type, scope_id, fact) WHERE status = 'active';
CREATE INDEX IF NOT EXISTS idx_facts_page ON user_facts(platform, user_id, scope_type, scope_id, updated_at DESC, id DESC) WHERE status = 'active';
CREATE INDEX IF NOT EXISTS idx_facts_embedding_model ON user_facts(platform, user_id, embedding_model) WHERE embedding IS NOT NULL AND status = 'active';

CREATE TABLE IF NOT EXISTS user_fact_sources (
    fact_id     INTEGER NOT NULL,
    message_id  INTEGER NOT NULL,
    linked_at   INTEGER NOT NULL,
    PRIMARY KEY (fact_id, message_id),
    FOREIGN KEY (fact_id) REFERENCES user_facts(id) ON DELETE CASCADE,
    FOREIGN KEY (message_id) REFERENCES messages(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_fact_sources_message ON user_fact_sources(message_id, fact_id);
CREATE INDEX IF NOT EXISTS idx_fact_sources_page ON user_fact_sources(fact_id, linked_at DESC, message_id DESC);

CREATE TABLE IF NOT EXISTS qq_quote_records (
    msg_id        TEXT NOT NULL,
    chat_type     TEXT NOT NULL CHECK (chat_type IN ('private', 'group')),
    conv_id       TEXT NOT NULL CHECK (length(conv_id) > 0),
    direction     TEXT NOT NULL CHECK (direction IN ('inbound', 'outbound')),
    content       TEXT NOT NULL DEFAULT '',
    content_kind  TEXT NOT NULL CHECK (content_kind IN ('text', 'text_card', 'mixed_media', 'image', 'file', 'unknown')),
    text_complete INTEGER NOT NULL CHECK (text_complete IN (0, 1)),
    updated_at    INTEGER NOT NULL,
    PRIMARY KEY (msg_id, chat_type, conv_id)
);
CREATE INDEX IF NOT EXISTS idx_qq_quote_records_time ON qq_quote_records(updated_at);

CREATE TABLE IF NOT EXISTS channel_workspaces (
    channel_id     TEXT NOT NULL CHECK (length(channel_id) > 0),
    workspace_path TEXT NOT NULL CHECK (length(workspace_path) > 0),
    project_name   TEXT NOT NULL,
    is_current     INTEGER NOT NULL DEFAULT 0 CHECK (is_current IN (0, 1)),
    last_active_at INTEGER NOT NULL,
    PRIMARY KEY (channel_id, workspace_path)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_workspace_one_current ON channel_workspaces(channel_id) WHERE is_current = 1;

CREATE TABLE IF NOT EXISTS session_resets (
    conv_id    TEXT PRIMARY KEY,
    reset_at   INTEGER NOT NULL DEFAULT 0,
    reset_id   INTEGER NOT NULL CHECK (reset_id >= 0)
);

CREATE TABLE IF NOT EXISTS runtime_state (
    state_key  TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS pending_notifications (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    platform     TEXT NOT NULL,
    recipient_id TEXT NOT NULL,
    dedupe_key   TEXT NOT NULL,
    category     TEXT NOT NULL CHECK (length(category) > 0),
    body         TEXT NOT NULL CHECK (length(body) > 0),
    delivery_state TEXT NOT NULL DEFAULT 'pending' CHECK (delivery_state IN ('pending', 'sending', 'uncertain', 'uncertain_notified')),
    created_at   INTEGER NOT NULL,
    UNIQUE(platform, recipient_id, dedupe_key)
);
CREATE INDEX IF NOT EXISTS idx_pending_notifications_owner ON pending_notifications(platform, recipient_id, id);

CREATE TABLE IF NOT EXISTS conversation_drafts (
    id TEXT PRIMARY KEY,
    platform TEXT NOT NULL,
    chat_type TEXT NOT NULL CHECK (chat_type IN ('private','group')),
    conv_id TEXT NOT NULL,
    requester_id TEXT NOT NULL,
    goal TEXT NOT NULL,
    context_json TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_conversation_drafts_owner
    ON conversation_drafts(platform, chat_type, conv_id, requester_id, updated_at DESC);

CREATE TABLE IF NOT EXISTS agent_tickets (
    id              TEXT PRIMARY KEY,
    requester_id    TEXT NOT NULL,
    channel_id      TEXT NOT NULL,
    goal            TEXT NOT NULL,
    worker          TEXT NOT NULL DEFAULT '',
    workspace       TEXT NOT NULL DEFAULT '',
    model           TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL CHECK (status IN ('pending', 'running', 'stopping', 'completed', 'failed', 'aborted', 'interrupted')),
    output          TEXT NOT NULL DEFAULT '',
    allow_sensitive INTEGER NOT NULL DEFAULT 0 CHECK (allow_sensitive IN (0, 1)),
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_agent_tickets_owner ON agent_tickets(requester_id, channel_id, created_at DESC);

CREATE TABLE IF NOT EXISTS agy_sessions (
    requester_id    TEXT NOT NULL,
    channel_id      TEXT NOT NULL,
    workspace_path  TEXT NOT NULL,
    conversation_id TEXT NOT NULL,
    updated_at      INTEGER NOT NULL,
    PRIMARY KEY (requester_id, channel_id, workspace_path)
);

CREATE TABLE IF NOT EXISTS user_roles (
    platform     TEXT NOT NULL,
    user_id      TEXT NOT NULL CHECK (length(user_id) > 0),
    role         TEXT NOT NULL CHECK (role IN ('friend', 'collaborator')),
    granted_by   TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    PRIMARY KEY (platform, user_id)
);
CREATE INDEX IF NOT EXISTS idx_user_roles_role ON user_roles(platform, role, updated_at DESC);

CREATE TABLE IF NOT EXISTS private_access_requests (
    platform        TEXT NOT NULL,
    user_id         TEXT NOT NULL CHECK (length(user_id) > 0),
    display_name    TEXT NOT NULL DEFAULT '',
    last_message    TEXT NOT NULL DEFAULT '',
    first_seen_at   INTEGER NOT NULL,
    last_seen_at    INTEGER NOT NULL,
    request_count   INTEGER NOT NULL DEFAULT 1 CHECK (request_count > 0),
    status          TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'revoked')),
    reviewed_by     TEXT NOT NULL DEFAULT '',
    reviewed_at     INTEGER,
    PRIMARY KEY (platform, user_id)
);
CREATE INDEX IF NOT EXISTS idx_private_access_pending ON private_access_requests(platform, status, last_seen_at DESC);

CREATE TABLE IF NOT EXISTS private_access_invites (
    id            TEXT PRIMARY KEY,
    platform      TEXT NOT NULL,
    code_hash     TEXT NOT NULL UNIQUE,
    role          TEXT NOT NULL CHECK (role IN ('friend', 'collaborator')),
    created_by    TEXT NOT NULL,
    created_at    INTEGER NOT NULL,
    expires_at    INTEGER NOT NULL,
    max_uses      INTEGER NOT NULL CHECK (max_uses > 0),
    used_count    INTEGER NOT NULL DEFAULT 0 CHECK (used_count >= 0 AND used_count <= max_uses),
    revoked_at    INTEGER
);
CREATE INDEX IF NOT EXISTS idx_private_access_invites_active ON private_access_invites(platform, expires_at DESC, created_at DESC);
`
