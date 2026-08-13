-- Read-only API tokens, and the account the public demo runs as.
--
-- The dashboard has a demo anyone can click through without signing in. Once
-- every route requires a user, the tempting fix is to let unauthenticated
-- callers read rows that look like seed data. That is the wrong fix: as soon as
-- one code path can return rows without a user, every query written afterwards
-- has to remember which mode it is in, and eventually one of them will not.
--
-- So the demo gets an ordinary account, and the restriction lives on the
-- credential instead. Isolation stays absolute — there is no unauthenticated
-- read path anywhere.

ALTER TABLE api_tokens
    ADD COLUMN is_read_only boolean NOT NULL DEFAULT false;

-- The demo account.
--
-- The password hash is deliberately not a valid bcrypt string: this account is
-- never signed into interactively, only reached through a read-only token held
-- server-side by the dashboard. Nothing can log in as it, by construction.
--
-- Its data is seeded in a later phase, once the modules write user_id.
INSERT INTO users (email, password_hash, display_name, email_verified_at)
VALUES (
           'demo@applymind.faroukhasnaoui.tech',
           'x-no-interactive-login-demo-account',
           'Demo',
           now()
       )
ON CONFLICT (email) DO NOTHING;