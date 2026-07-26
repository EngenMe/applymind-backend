-- Queries for the applications module. Run `sqlc generate` after adding this
-- file; internal/db/sqlc/applications.sql.go and the additions to querier.go are
-- generated from it.

-- name: CreateApplication :one
INSERT INTO applications (
    id, company_name, job_title, job_description, job_url,
    site_id, cv_version_id, status, applied_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9
)
RETURNING *;

-- name: GetApplication :one
SELECT * FROM applications WHERE id = $1;

-- name: UpdateApplication :one
-- Captured job data only. Status never moves here — that is
-- UpdateApplicationStatus, so no transition can skip the audit trail.
UPDATE applications
SET company_name    = $2,
    job_title       = $3,
    job_description = $4,
    job_url         = $5,
    site_id         = $6,
    cv_version_id   = $7
WHERE id = $1
RETURNING *;

-- name: UpdateApplicationStatus :one
-- applied_at is write-once: COALESCE keeps the original timestamp if the
-- application has already been applied to, so bouncing through statuses later
-- never rewrites when it was actually sent.
UPDATE applications
SET status     = sqlc.arg('status'),
    applied_at = COALESCE(applications.applied_at, sqlc.narg('applied_at')::timestamptz)
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: DeleteApplication :execrows
-- Cover letters, status history, reminders and recruiter contacts go with it via
-- ON DELETE CASCADE.
DELETE FROM applications WHERE id = $1;

-- name: ListApplications :many
-- Every filter is optional: a NULL parameter disables that clause. Ordering and
-- date filtering both use the effective date — applied_at when set, created_at
-- otherwise — so a Saved application is never invisible to a date range.
SELECT * FROM applications
WHERE (sqlc.narg('status')::application_status IS NULL OR status = sqlc.narg('status')::application_status)
  AND (sqlc.narg('site_id')::uuid IS NULL OR site_id = sqlc.narg('site_id')::uuid)
  AND (sqlc.narg('cv_version_id')::uuid IS NULL OR cv_version_id = sqlc.narg('cv_version_id')::uuid)
  AND (sqlc.narg('company')::text IS NULL
       OR company_name ILIKE '%' || sqlc.narg('company')::text || '%')
  AND (sqlc.narg('search')::text IS NULL
       OR company_name ILIKE '%' || sqlc.narg('search')::text || '%'
       OR job_title ILIKE '%' || sqlc.narg('search')::text || '%')
  AND (sqlc.narg('from_date')::timestamptz IS NULL
       OR COALESCE(applied_at, created_at) >= sqlc.narg('from_date')::timestamptz)
  AND (sqlc.narg('to_date')::timestamptz IS NULL
       OR COALESCE(applied_at, created_at) <= sqlc.narg('to_date')::timestamptz)
ORDER BY COALESCE(applied_at, created_at) DESC, created_at DESC
LIMIT sqlc.arg('row_limit')::int OFFSET sqlc.arg('row_offset')::int;

-- name: FindApplicationsByCompanyName :many
-- The MVP duplicate check: exact company name, ignoring case and surrounding
-- whitespace. Embedding similarity is a later phase.
SELECT * FROM applications
WHERE lower(btrim(company_name)) = lower(btrim(sqlc.arg('company_name')::text))
ORDER BY COALESCE(applied_at, created_at) DESC;

-- name: CreateApplicationStatusHistory :one
INSERT INTO application_status_history (
    application_id, from_status, to_status, changed_by, note
) VALUES (
    $1, $2, $3, $4, $5
)
RETURNING *;

-- name: ListApplicationStatusHistory :many
SELECT * FROM application_status_history
WHERE application_id = $1
ORDER BY changed_at ASC;

-- name: CreatePendingFollowUpReminder :one
-- uq_follow_up_reminders_one_pending allows a single un-sent, un-dismissed
-- reminder per application, so a re-save is a no-op rather than an error.
INSERT INTO follow_up_reminders (application_id, due_at)
VALUES ($1, $2)
ON CONFLICT (application_id) WHERE sent_at IS NULL AND dismissed_at IS NULL
DO NOTHING
RETURNING *;

-- name: DismissPendingFollowUpReminders :exec
-- Called when an application moves off Applied: the company (or the user) has
-- moved it on, so there is nothing left to chase.
UPDATE follow_up_reminders
SET dismissed_at = now()
WHERE application_id = $1
  AND sent_at IS NULL
  AND dismissed_at IS NULL;

-- name: ResolveSiteByDomain :one
-- Turns a job URL host into a site_id when the client did not send one.
-- NOTE: if queries/sites.sql already defines an equivalent lookup, delete this
-- one and point the repository at that generated method instead — two queries
-- with the same name in the same package will not compile.
--
-- The service passes a lowercased, www-stripped host, so the stored value is
-- normalised the same way here: "www.LinkedIn.com" and "linkedin.com" both match.
SELECT * FROM sites
WHERE is_active = true
  AND regexp_replace(lower(btrim(domain)), '^www\.', '') = sqlc.arg('domain')::text
LIMIT 1;
