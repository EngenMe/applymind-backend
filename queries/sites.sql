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
