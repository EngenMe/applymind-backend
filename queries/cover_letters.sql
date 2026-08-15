-- Queries for the coverletters module.
--
-- cover_letters is one-to-one with applications (unique(application_id)), so
-- every statement here is keyed by application_id rather than by the row's own
-- id. There is no history table and nothing to match against.
--
-- Phase 15: every statement adds user_id. This module's routes are mounted
-- independently of applications' own — nothing upstream of these queries
-- confirms the caller owns the application_id in the path — so without this
-- scope, one user could read, edit or overwrite another user's cover letter
-- simply by guessing or observing an application id.

-- name: CreateCoverLetter :one
-- The id is supplied by the service rather than defaulted, because a file
-- cover letter's S3 key is derived from it before the row is written.
--
-- The WHERE EXISTS guard is load-bearing, not decorative: cover_letters'
-- foreign key on application_id only proves that application exists, not that
-- it belongs to this user. Without this guard, one user's application_id — a
-- UUID they could observe or guess — would let another user attach a cover
-- letter to it, because the FK alone has nothing to say about ownership. A
-- mismatch here reads as "no such application" to the caller, exactly like a
-- genuinely missing one, so an unauthorized caller learns nothing about
-- whether the id is real.
INSERT INTO cover_letters (
    id,
    application_id,
    user_id,
    kind,
    body_text,
    s3_key,
    original_filename
)
SELECT $1, $2, $3, $4, $5, $6, $7
WHERE EXISTS (
    SELECT 1 FROM applications a WHERE a.id = $2 AND a.user_id = $3
)
RETURNING *;

-- name: GetCoverLetterByApplicationID :one
SELECT *
FROM cover_letters
WHERE application_id = $1 AND user_id = $2;

-- name: UpdateCoverLetterText :one
-- Scoped to kind = 'text'. A file cover letter records the bytes that were
-- actually sent and is immutable; without this predicate the row-level check
-- constraint would reject the write anyway, but with a far less useful error.
UPDATE cover_letters
SET body_text  = $2,
    updated_at = now()
WHERE application_id = $1
  AND user_id = $3
  AND kind = 'text'
RETURNING *;

-- name: DeleteCoverLetterByApplicationID :exec
-- Used to implement replace-on-save. Deleting a row that is not there is a
-- no-op, which is what the service relies on.
DELETE
FROM cover_letters
WHERE application_id = $1 AND user_id = $2;
