-- Queries for the coverletters module.
--
-- cover_letters is one-to-one with applications (unique(application_id)), so
-- every statement here is keyed by application_id rather than by the row's own
-- id. There is no history table and nothing to match against.

-- name: CreateCoverLetter :one
-- The id is supplied by the service rather than defaulted, because a file
-- cover letter's S3 key is derived from it before the row is written.
INSERT INTO cover_letters (
    id,
    application_id,
    kind,
    body_text,
    s3_key,
    original_filename
)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetCoverLetterByApplicationID :one
SELECT *
FROM cover_letters
WHERE application_id = $1;

-- name: UpdateCoverLetterText :one
-- Scoped to kind = 'text'. A file cover letter records the bytes that were
-- actually sent and is immutable; without this predicate the row-level check
-- constraint would reject the write anyway, but with a far less useful error.
UPDATE cover_letters
SET body_text  = $2,
    updated_at = now()
WHERE application_id = $1
  AND kind = 'text'
RETURNING *;

-- name: DeleteCoverLetterByApplicationID :exec
-- Used to implement replace-on-save. Deleting a row that is not there is a
-- no-op, which is what the service relies on.
DELETE
FROM cover_letters
WHERE application_id = $1;
