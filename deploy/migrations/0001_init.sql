-- dsh-fleet initial schema.
--
-- Applied by `dshf serve` at boot, in filename order, inside one transaction
-- each. Migrations are forward-only: there are no down scripts, because a
-- control plane that has already handed out node tokens cannot meaningfully
-- roll its schema back.

BEGIN;

CREATE TABLE schema_migrations (
    version     text        PRIMARY KEY,
    applied_at  timestamptz NOT NULL DEFAULT now()
);

-- ── people ───────────────────────────────────────────────────────────────────

CREATE TABLE users (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    username      text        NOT NULL,
    -- Argon2id encoded hash. The plaintext never reaches this database.
    password_hash text        NOT NULL,
    is_admin      boolean     NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now(),
    disabled_at   timestamptz
);

-- Case-insensitive uniqueness without depending on the citext extension, which
-- is not present in every managed Postgres.
CREATE UNIQUE INDEX users_username_lower_key ON users (lower(username));

CREATE TABLE user_sessions (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- SHA-256 of the cookie value. A stolen database dump must not yield live
    -- sessions, and unlike passwords these are high-entropy so a fast hash is
    -- the right trade.
    token_hash  bytea       NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    last_seen_at timestamptz,
    user_agent  text
);

CREATE UNIQUE INDEX user_sessions_token_hash_key ON user_sessions (token_hash);
CREATE INDEX user_sessions_user_id_idx ON user_sessions (user_id);
CREATE INDEX user_sessions_expires_at_idx ON user_sessions (expires_at);

-- ── machines ─────────────────────────────────────────────────────────────────

CREATE TABLE nodes (
    -- The operator-chosen id the plugin presents in `hello`, e.g. 'laptop'.
    id              text        PRIMARY KEY,
    label           text,
    -- Argon2id encoded hash of the node token. `dshf node add` prints the
    -- plaintext once and never stores it.
    token_hash      text        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    revoked_at      timestamptz,

    -- Facts from the most recent `hello`. Null until a node first connects.
    dsh_version     text,
    plugin_version  text,
    platform        text,
    arch            text,
    cwd             text,
    caps            jsonb       NOT NULL DEFAULT '[]'::jsonb,

    -- Liveness is DERIVED from last_seen_at rather than stored as a boolean:
    -- a crashed control plane would otherwise leave every node marked online
    -- forever, and reconciling that on boot is a bug waiting to happen.
    last_seen_at    timestamptz,

    -- Denormalised copy of the newest telemetry snapshot so the console's node
    -- list is one query. History lives in node_telemetry.
    latest_snapshot jsonb
);

CREATE INDEX nodes_last_seen_at_idx ON nodes (last_seen_at DESC NULLS LAST);

CREATE TABLE node_telemetry (
    id        bigserial   PRIMARY KEY,
    node_id   text        NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    ts        timestamptz NOT NULL,
    -- Stored verbatim. The control plane must not require any particular field:
    -- a newer node adds keys without a control-plane release.
    snapshot  jsonb       NOT NULL
);

CREATE INDEX node_telemetry_node_ts_idx ON node_telemetry (node_id, ts DESC);

-- ── audit ────────────────────────────────────────────────────────────────────

-- Every request the privilege gate evaluated. This exists because a custom
-- carrier bypasses dsh's own loopback pin on the privileged method set
-- (settings.*, credentials.*, host.pickDirectory, host.openPath, agent-preset
-- authoring), so this project owns that boundary and must be able to show what
-- crossed it.
CREATE TABLE audit_log (
    id       bigserial   PRIMARY KEY,
    ts       timestamptz NOT NULL DEFAULT now(),
    user_id  uuid        REFERENCES users(id) ON DELETE SET NULL,
    node_id  text        REFERENCES nodes(id) ON DELETE SET NULL,
    ns       text        NOT NULL,
    -- Method name only. Request bodies are never recorded: they are opaque by
    -- design and may carry prompts, file contents, or credentials.
    path     text        NOT NULL,
    allowed  boolean     NOT NULL,
    status   integer,
    reason   text
);

CREATE INDEX audit_log_ts_idx ON audit_log (ts DESC);
CREATE INDEX audit_log_node_ts_idx ON audit_log (node_id, ts DESC);

INSERT INTO schema_migrations (version) VALUES ('0001_init');

COMMIT;
