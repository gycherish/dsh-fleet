-- User-owned tokens, and machines that enrol themselves with one.
--
-- Registering a machine used to mean running `dshf node add` on the control
-- plane and carrying the printed token to the machine by hand. That is a fine
-- flow for a container and a poor one for a person: it needs a shell on both
-- ends, and it cannot be completed from the phone the console was built for.
--
-- A user token closes that. Someone mints one for themselves in the console,
-- types their username and that token into the machine's plugin, and the
-- machine registers itself on its first connection.

BEGIN;

-- ── tokens people hold ───────────────────────────────────────────────────────

CREATE TABLE user_tokens (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- Operator-facing name, so a token can be recognised before it is revoked.
    name         text        NOT NULL,
    -- Argon2id encoded hash. The plaintext is printed once and never stored.
    token_hash   text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    -- Refreshed on use, so an unused token is visibly unused before it is cut.
    last_used_at timestamptz,
    revoked_at   timestamptz
);

CREATE INDEX user_tokens_user_id_idx ON user_tokens (user_id);

-- ── machines that enrolled themselves ────────────────────────────────────────

-- Which account a machine belongs to. Null for the machines that predate this
-- migration, and for any still enrolled with `dshf node add`: those authenticate
-- with a token of their own and belong to the deployment rather than a person.
ALTER TABLE nodes ADD COLUMN owner_id uuid REFERENCES users (id) ON DELETE SET NULL;

CREATE INDEX nodes_owner_id_idx ON nodes (owner_id);

-- A self-enrolled machine has no token of its own: it presents its owner's
-- token on every connection, so revoking that token disconnects every machine
-- enrolled with it. That is the intended blast radius, and it is why the column
-- has to stop being mandatory.
ALTER TABLE nodes ALTER COLUMN token_hash DROP NOT NULL;

-- Exactly one credential per machine, whichever kind it is. A row with both
-- would leave "which one authenticates it" to reading order.
ALTER TABLE nodes ADD CONSTRAINT nodes_one_credential CHECK (
    (token_hash IS NOT NULL AND owner_id IS NULL)
    OR (token_hash IS NULL AND owner_id IS NOT NULL)
);

INSERT INTO schema_migrations (version) VALUES ('0002_user_tokens');

COMMIT;
