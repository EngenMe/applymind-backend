-- name: ListActiveSites :many
-- Returns every site the extension is currently allowed to capture from.
SELECT * FROM sites
WHERE is_active = true
ORDER BY name;

-- name: GetSiteByDomain :one
-- Resolves a captured page's domain to a site row. Returns pgx.ErrNoRows when
-- the domain is not a configured site.
SELECT * FROM sites
WHERE domain = $1;

-- name: ListSites :many
-- Every site, active or not. Backs the dashboard settings page, which has to
-- show the deactivated ones in order to switch them back on.
SELECT * FROM sites
ORDER BY name;

-- name: GetSite :one
SELECT * FROM sites
WHERE id = $1;

-- name: CreateSite :one
-- id, created_at and updated_at come from column defaults.
INSERT INTO sites (name, domain, is_preconfigured, is_active)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: EnsureSite :one
-- Idempotent insert used to seed the pre-configured list on boot. DO NOTHING
-- means no row is returned when the domain is already registered, which the
-- repository reads as "nothing to do" rather than an error.
--
-- Note this only guards the domain constraint; name is UNIQUE too, and a
-- collision there surfaces as 23505.
INSERT INTO sites (name, domain, is_preconfigured, is_active)
VALUES ($1, $2, $3, $4)
ON CONFLICT (domain) DO NOTHING
RETURNING *;

-- name: SetSiteActive :one
-- updated_at is maintained by trg_sites_updated_at.
UPDATE sites
SET is_active = $2
WHERE id = $1
RETURNING *;

-- name: DeleteSite :execrows
-- Fails with 23503 when applications still reference the site: the foreign key
-- is ON DELETE RESTRICT. The pre-configured rule is enforced in the service
-- layer, not here, per the ERD.
DELETE FROM sites
WHERE id = $1;
