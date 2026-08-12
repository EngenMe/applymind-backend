-- Authentication. Three tables, and a deliberate choice not to use JWTs.
--
-- Sessions and API tokens are opaque random strings looked up by hash, not
-- self-describing tokens. A JWT cannot be revoked without a denylist, which is
-- a session table wearing a disguise — so this stores sessions directly and
-- gets "log out everywhere" and "revoke this device" for the price of one
-- indexed lookup per request.
--
-- Neither table stores a token. It stores the SHA-256 of one. The raw value
-- exists only in the user's cookie or extension storage, so a database leak
-- yields no usable credentials.

-- Case-insensitive text, so Farouk@example.com and farouk@example.com are the
-- same account rather than two accounts and a confused user.
CREATE EXTENSION IF NOT EXISTS citext;

CREATE TABLE users (
    id                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    email             citext      NOT NULL UNIQUE,

    -- bcrypt/argon2id output. Never unique: two users may legitimately choose
    -- the same password, and a unique constraint would both break that and
    -- leak the fact that someone else already uses it.
    password_hash     text        NOT NULL,

    display_name      text,

    -- NULL until the address is confirmed. Kept as a timestamp rather than a
    -- boolean because "when" is worth knowing and "whether" falls out of it.
    email_verified_at timestamptz,

    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT ck_users_email_not_blank CHECK (btrim(email::text) <> '')
);

CREATE TRIGGER trg_users_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Browser sessions for the dashboard. Short-lived, cookie-backed.
CREATE TABLE sessions (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    -- The unique constraint provides the lookup index; every authenticated
    -- request hashes the presented token and finds the row by this column.
    token_hash text        NOT NULL UNIQUE,

    expires_at timestamptz NOT NULL,

    -- Set rather than deleted, so "signed out at 14:02 from Firefox" survives
    -- as a fact. Expiry and revocation are different events and the
    -- distinction matters when someone asks why they were logged out.
    revoked_at timestamptz,

    -- Enough to recognise a session in a "your active sessions" list. Not
    -- fingerprinting: no attempt to correlate these across accounts.
    user_agent text,
    ip_address inet,

    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_sessions_user_id ON sessions (user_id);

-- Backs the settings page. Partial, because a list of live sessions is the
-- only reason to scan by user and revoked rows are never part of the answer.
CREATE INDEX idx_sessions_active ON sessions (user_id) WHERE revoked_at IS NULL;

-- Long-lived tokens for the browser extension.
--
-- Separate from sessions because the lifetimes have nothing in common: a
-- session dies in days, an extension token lives until the user kills it. The
-- extension also cannot share the dashboard's cookie — different origin — so
-- it needs a credential it can hold itself.
--
-- This is what the single static APIMIND_API_KEY becomes: the same idea, but
-- per user and revocable.
CREATE TABLE api_tokens (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash   text        NOT NULL UNIQUE,

    -- Shown in the settings list: "Chrome on the laptop". Without it, revoking
    -- the right token means guessing between two identical rows.
    name         text        NOT NULL,

    -- Written on use, so a token nobody has touched in months is visible as
    -- such. Best-effort: an update failure here must never fail the request.
    last_used_at timestamptz,
    revoked_at   timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT ck_api_tokens_name_not_blank CHECK (btrim(name) <> '')
);

CREATE INDEX idx_api_tokens_user_id ON api_tokens (user_id);
