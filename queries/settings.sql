-- name: GetSettings :one
SELECT * FROM settings
WHERE id = true;

-- name: UpsertProfileSummary :one
-- Upsert rather than update: if the row seeded by migration 000011 is ever
-- missing, the first write puts it back instead of failing forever.
INSERT INTO settings (id, profile_summary)
VALUES (true, sqlc.arg(profile_summary))
ON CONFLICT (id) DO UPDATE
SET profile_summary = EXCLUDED.profile_summary,
    updated_at      = now()
RETURNING *;
