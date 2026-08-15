-- Phase 15: every read is scoped to "global (user_id IS NULL) or mine" — a
-- pre-configured site belongs to everyone, a custom one belongs to whoever
-- added it. Writes that create or remove a row are scoped to the caller alone;
-- EnsureSite is the one exception, since it only ever writes global rows at
-- boot and has no caller to scope to.

-- name: ListActiveSites :many
-- Returns every site the extension is currently allowed to capture from —
-- the pre-configured list plus this user's own active custom sites.
SELECT * FROM sites
WHERE is_active = true
  AND (user_id IS NULL OR user_id = sqlc.arg('user_id')::uuid)
ORDER BY name;

-- name: GetSiteByDomain :one
-- Resolves a captured page's domain to a site row, global or the caller's own.
-- Returns pgx.ErrNoRows when the domain is not a configured site for this user.
SELECT * FROM sites
WHERE domain = $1
  AND (user_id IS NULL OR user_id = sqlc.arg('user_id')::uuid);

-- name: ListSites :many
-- Every site this user can see, active or not: the pre-configured list plus
-- their own custom sites. Backs the dashboard settings page, which has to show
-- the deactivated ones in order to switch them back on.
SELECT * FROM sites
WHERE user_id IS NULL OR user_id = sqlc.arg('user_id')::uuid
ORDER BY name;

-- name: GetSite :one
SELECT * FROM sites
WHERE id = $1
  AND (user_id IS NULL OR user_id = sqlc.arg('user_id')::uuid);

-- name: CreateSite :one
-- Always owned: a site created through this statement is never global. The
-- pre-configured list is seeded exclusively through EnsureSite below.
INSERT INTO sites (user_id, name, domain, is_preconfigured, is_active)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: EnsureSite :one
-- Idempotent insert used to seed the pre-configured list on boot. DO NOTHING
-- means no row is returned when the domain is already registered, which the
-- repository reads as "nothing to do" rather than an error.
--
-- Unchanged by phase 15: this always writes a global row (user_id defaults to
-- NULL) and has no caller to scope to — it runs once at boot, before any
-- request exists.
--
-- The WHERE clause on the conflict target is load-bearing. Migration 000015
-- replaced the plain unique(domain) with two rescoped constraints — one on
-- (user_id, domain), and a partial unique index on (domain) that applies only
-- to the global rows, where user_id IS NULL. Postgres will not infer a partial
-- index as an arbiter unless the statement restates its predicate, so a bare
-- ON CONFLICT (domain) matches no constraint at all and fails at runtime with
-- 42P10 rather than at generate time.
--
-- Note this only guards the domain constraint; name is UNIQUE too, and a
-- collision there surfaces as 23505.
INSERT INTO sites (name, domain, is_preconfigured, is_active)
VALUES ($1, $2, $3, $4)
ON CONFLICT (domain) WHERE user_id IS NULL DO NOTHING
RETURNING *;

-- name: SetSiteActive :one
-- Global-or-mine, matching every other read here: a global row can be
-- deactivated by any user (the ERD's own rule — "pre-configured rows can only
-- be deactivated" carries no per-user override, so this remains one shared
-- switch, same as before phase 15), and a custom row only by its owner.
-- updated_at is maintained by trg_sites_updated_at.
UPDATE sites
SET is_active = $2
WHERE id = $1
  AND (user_id IS NULL OR user_id = sqlc.arg('user_id')::uuid)
RETURNING *;

-- name: DeleteSite :execrows
-- Owned rows only. The service already refuses to delete a pre-configured
-- site before this runs (ErrPreconfigured), so in practice this only ever
-- targets a row with user_id set — but scoping it here too means a caller
-- can never delete another user's custom site by guessing its id.
-- Fails with 23503 when applications still reference the site: the foreign key
-- is ON DELETE RESTRICT.
DELETE FROM sites
WHERE id = $1 AND user_id = sqlc.arg('user_id')::uuid;
