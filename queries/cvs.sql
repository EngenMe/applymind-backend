-- ApplyMind — cvs module queries
--
-- Note: application status is cast to text so this module does not depend on the
-- generated enum type from the applications module.

-- name: CreateCV :one
INSERT INTO cvs (name, tag)
VALUES ($1, $2)
RETURNING *;

-- name: GetCV :one
SELECT * FROM cvs WHERE id = $1;

-- name: GetCVByName :one
SELECT * FROM cvs WHERE name = $1;

-- name: ListCVs :many
SELECT * FROM cvs ORDER BY created_at DESC;

-- name: DeleteCV :exec
DELETE FROM cvs WHERE id = $1;

-- name: CreateCVVersion :one
INSERT INTO cv_versions (id, cv_id, sha256_hash, file_size_bytes, original_filename, s3_key)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetCVVersion :one
SELECT * FROM cv_versions WHERE id = $1;

-- name: ListVersionsForCV :many
SELECT * FROM cv_versions
WHERE cv_id = $1
ORDER BY uploaded_at DESC;

-- name: ListAllCVVersions :many
SELECT * FROM cv_versions
ORDER BY cv_id, uploaded_at DESC;

-- name: FindCVVersionByHash :one
SELECT * FROM cv_versions
WHERE sha256_hash = $1
ORDER BY uploaded_at DESC
LIMIT 1;

-- name: FindLatestCVVersionByFilename :one
SELECT * FROM cv_versions
WHERE original_filename = $1
ORDER BY uploaded_at DESC
LIMIT 1;

-- name: FindCVVersionByCVAndSize :one
SELECT * FROM cv_versions
WHERE cv_id = $1 AND file_size_bytes = $2
ORDER BY uploaded_at DESC
LIMIT 1;

-- name: FindCVVersionByCVAndHash :one
SELECT * FROM cv_versions
WHERE cv_id = $1 AND sha256_hash = $2
LIMIT 1;

-- name: ListApplicationsUsingCVVersion :many
SELECT
    a.id,
    a.company_name,
    a.job_title,
    a.status::text AS status,
    a.applied_at
FROM applications a
WHERE a.cv_version_id = $1
ORDER BY a.applied_at DESC NULLS LAST;

-- name: GetLastCVUsage :one
SELECT
    a.company_name,
    a.applied_at
FROM applications a
JOIN cv_versions v ON v.id = a.cv_version_id
WHERE v.cv_id = $1 AND a.applied_at IS NOT NULL
ORDER BY a.applied_at DESC
LIMIT 1;
