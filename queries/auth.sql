-- Queries for the auth module.
--
-- Neither sessions nor api_tokens is ever looked up by its raw token. The
-- client holds the raw value; this stores only its SHA-256, and every lookup
-- hashes what arrived before searching. A dump of this database yields no
-- usable credentials.

-- name: CreateUser :one
INSERT INTO users (email, password_hash, display_name, email_verified_at)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetUser :one
SELECT * FROM users WHERE id = $1;

-- name: GetUserByEmail :one
-- email is citext, so this is already case-insensitive at the database level.
-- The service normalises anyway, so the two never disagree about what "the same
-- address" means.
SELECT * FROM users WHERE email = $1;

-- name: UpdateUserPassword :one
UPDATE users
SET password_hash = $2
WHERE id = $1
RETURNING *;

-- ---------------------------------------------------------------------------
-- Sessions
-- ---------------------------------------------------------------------------

-- name: CreateSession :one
INSERT INTO sessions (user_id, token_hash, expires_at, user_agent, ip_address)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: FindLiveSessionByTokenHash :one
-- The hot path: every authenticated dashboard request runs this. Expiry and
-- revocation are filtered here rather than in Go so that a stale row can never
-- be authenticated by a caller that forgot to check.
SELECT * FROM sessions
WHERE token_hash = $1
  AND revoked_at IS NULL
  AND expires_at > now();

-- name: ExtendSession :one
-- The sliding half of the 30-day window. Called only when the service decides
-- enough time has passed to be worth a write.
UPDATE sessions
SET expires_at = $2
WHERE id = $1
  AND revoked_at IS NULL
RETURNING *;

-- name: RevokeSession :execrows
-- Scoped by user_id as well as id: a session id is a uuid the caller could have
-- obtained anywhere, and revoking somebody else's session is not a feature.
UPDATE sessions
SET revoked_at = now()
WHERE id = $1
  AND user_id = $2
  AND revoked_at IS NULL;

-- name: RevokeSessionByTokenHash :execrows
-- Logout. The caller presents a token rather than an id.
UPDATE sessions
SET revoked_at = now()
WHERE token_hash = $1
  AND revoked_at IS NULL;

-- name: RevokeAllUserSessions :execrows
-- "Sign out everywhere". Also the correct response to a password change.
UPDATE sessions
SET revoked_at = now()
WHERE user_id = $1
  AND revoked_at IS NULL;

-- name: ListActiveSessions :many
SELECT * FROM sessions
WHERE user_id = $1
  AND revoked_at IS NULL
  AND expires_at > now()
ORDER BY created_at DESC;

-- name: DeleteExpiredSessions :execrows
-- Housekeeping, not authentication: expired rows are already unusable because
-- every lookup filters them out. This only stops the table growing forever.
DELETE FROM sessions
WHERE expires_at < now() - interval '30 days';

-- ---------------------------------------------------------------------------
-- API tokens
-- ---------------------------------------------------------------------------

-- name: CreateAPIToken :one
INSERT INTO api_tokens (user_id, token_hash, name, is_read_only)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: FindLiveAPITokenByTokenHash :one
-- No expiry column: an API token lives until it is revoked. That is deliberate
-- — the extension cannot prompt for a re-login, so a token that silently
-- expired would look like the extension breaking.
SELECT * FROM api_tokens
WHERE token_hash = $1
  AND revoked_at IS NULL;

-- name: TouchAPIToken :exec
-- Best-effort, called after a successful match. A failure here must never fail
-- the request it was authenticating.
UPDATE api_tokens
SET last_used_at = now()
WHERE id = $1;

-- name: RevokeAPIToken :execrows
UPDATE api_tokens
SET revoked_at = now()
WHERE id = $1
  AND user_id = $2
  AND revoked_at IS NULL;

-- name: ListActiveAPITokens :many
SELECT * FROM api_tokens
WHERE user_id = $1
  AND revoked_at IS NULL
ORDER BY created_at DESC;