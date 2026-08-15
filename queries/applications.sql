-- Queries for the applications module.
--
-- Phase 15: every statement is now scoped by user_id — either as a WHERE clause
-- on an existing row, or as a column on a new one. The one exception is
-- ResolveSiteByDomain, which matches a global (user_id IS NULL) site OR one the
-- caller owns, because a pre-configured site belongs to everyone.

-- name: CreateApplication :one
-- Both WHERE EXISTS clauses are load-bearing, not decorative: site_id and
-- cv_version_id are foreign keys, and a foreign key only proves the row
-- exists — it says nothing about who owns it. site_id must be global
-- (user_id IS NULL) or the caller's own, matching every other site read in
-- this codebase. cv_version_id is nullable — no CV attached is always fine —
-- but when it is set, it must belong to this user; skip that check and one
-- user's application can end up pointing at another user's CV, which is a
-- worse leak than a rejected save.
INSERT INTO applications (
    id, user_id, company_name, job_title, job_description, job_url,
    site_id, cv_version_id, status, applied_at
)
SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10
WHERE EXISTS (
    SELECT 1 FROM sites AS chk_site
    WHERE chk_site.id = $7 AND (chk_site.user_id IS NULL OR chk_site.user_id = $2)
)
AND ($8::uuid IS NULL OR EXISTS (
    SELECT 1 FROM cv_versions AS chk_cv
    WHERE chk_cv.id = $8 AND chk_cv.user_id = $2
))
RETURNING *;

-- name: GetApplication :one
SELECT * FROM applications WHERE applications.id = $1 AND applications.user_id = $2;

-- name: UpdateApplication :one
-- Captured job data only. Status never moves here — that is
-- UpdateApplicationStatus, so no transition can skip the audit trail.
--
-- Same ownership guards as CreateApplication, for the same reason: this is
-- also the query Complete uses to attach a CV version (Flow 2), so the check
-- has to live here too, not only on the insert path.
UPDATE applications
SET company_name    = $2,
    job_title       = $3,
    job_description = $4,
    job_url         = $5,
    site_id         = $6,
    cv_version_id   = $7
WHERE applications.id = $1
  AND applications.user_id = $8
  AND EXISTS (
      SELECT 1 FROM sites AS chk_site
      WHERE chk_site.id = $6 AND (chk_site.user_id IS NULL OR chk_site.user_id = $8)
  )
  AND ($7::uuid IS NULL OR EXISTS (
      SELECT 1 FROM cv_versions AS chk_cv
      WHERE chk_cv.id = $7 AND chk_cv.user_id = $8
  ))
RETURNING *;

-- name: UpdateApplicationStatus :one
-- applied_at is write-once: COALESCE keeps the original timestamp if the
-- application has already been applied to, so bouncing through statuses later
-- never rewrites when it was actually sent.
UPDATE applications
SET status     = sqlc.arg('status'),
    applied_at = COALESCE(applications.applied_at, sqlc.narg('applied_at')::timestamptz)
WHERE applications.id = sqlc.arg('id') AND applications.user_id = sqlc.arg('user_id')
RETURNING *;

-- name: DeleteApplication :execrows
-- Cover letters, status history, reminders and recruiter contacts go with it via
-- ON DELETE CASCADE.
DELETE FROM applications WHERE applications.id = $1 AND applications.user_id = $2;

-- name: ListApplications :many
-- Every filter is optional: a NULL parameter disables that clause. Ordering and
-- date filtering both use the effective date — applied_at when set, created_at
-- otherwise — so a Saved application is never invisible to a date range.
SELECT * FROM applications
WHERE user_id = sqlc.arg('user_id')
  AND (sqlc.narg('status')::application_status IS NULL OR status = sqlc.narg('status')::application_status)
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
-- whitespace, scoped to the caller — someone else applying to the same company
-- is not this user's duplicate.
SELECT * FROM applications
WHERE user_id = sqlc.arg('user_id')
  AND lower(btrim(company_name)) = lower(btrim(sqlc.arg('company_name')::text))
ORDER BY COALESCE(applied_at, created_at) DESC;

-- name: CreateApplicationStatusHistory :one
INSERT INTO application_status_history (
    application_id, user_id, from_status, to_status, changed_by, note
) VALUES (
    $1, $2, $3, $4, $5, $6
)
RETURNING *;

-- name: ListApplicationStatusHistory :many
SELECT * FROM application_status_history
WHERE application_id = $1 AND user_id = $2
ORDER BY changed_at ASC;

-- name: CreatePendingFollowUpReminder :one
-- uq_follow_up_reminders_one_pending allows a single un-sent, un-dismissed
-- reminder per application, so a re-save is a no-op rather than an error. The
-- constraint stays scoped by application_id alone (000015's reasoning: an
-- application already belongs to exactly one user), so user_id here is a
-- column to fill in, not part of what makes a reminder unique.
INSERT INTO follow_up_reminders (application_id, user_id, due_at)
VALUES ($1, $2, $3)
ON CONFLICT (application_id) WHERE sent_at IS NULL AND dismissed_at IS NULL
DO NOTHING
RETURNING *;

-- name: DismissPendingFollowUpReminders :exec
-- Called when an application moves off Applied: the company (or the user) has
-- moved it on, so there is nothing left to chase. user_id is redundant with
-- application_id ownership already established by the caller, but it costs
-- nothing to assert here too.
UPDATE follow_up_reminders
SET dismissed_at = now()
WHERE application_id = $1
  AND user_id = $2
  AND sent_at IS NULL
  AND dismissed_at IS NULL;

-- name: ResolveSiteByDomain :one
-- Turns a job URL host into a site_id when the client did not send one.
--
-- Matches a global site (user_id IS NULL — the pre-configured list every user
-- sees) or one the caller registered themselves. Without the second half of
-- that OR, a user's own custom site could never be resolved from a job URL,
-- only from an explicit site_id.
--
-- The service passes a lowercased, www-stripped host, so the stored value is
-- normalised the same way here: "www.LinkedIn.com" and "linkedin.com" both match.
SELECT * FROM sites
WHERE is_active = true
  AND regexp_replace(lower(btrim(domain)), '^www\.', '') = sqlc.arg('domain')::text
  AND (user_id IS NULL OR user_id = sqlc.arg('user_id')::uuid)
LIMIT 1;

-- name: CVVersionOwnedByUser :one
-- A cheap pre-check so the service can tell an unowned/missing cv_version_id
-- apart from an unowned/missing site_id before either reaches the atomic
-- INSERT/UPDATE guard, which cannot distinguish the two once it collapses to
-- "no rows written". This query exists purely for that distinguishable error
-- message; the INSERT/UPDATE guards remain the actual enforcement against a
-- race between this check and the write.
SELECT EXISTS (
    SELECT 1 FROM cv_versions WHERE id = sqlc.arg('id') AND user_id = sqlc.arg('user_id')
) AS owned;

-- name: FindSiteByID :one
-- Validates a client-supplied site_id before it is trusted: global (user_id IS
-- NULL) or the caller's own. Without this, resolveSiteID would accept any
-- site_id the caller sent — including one belonging to somebody else — since
-- the column only exists to be trusted, not verified, once it reaches the
-- insert. Mirrors ResolveSiteByDomain's same-scope reasoning, keyed by id
-- instead of domain.
SELECT * FROM sites
WHERE id = sqlc.arg('id') AND (user_id IS NULL OR user_id = sqlc.arg('user_id')::uuid);

-- name: SetApplicationAIScore :one
-- Writes the GPT-4o-mini job-match score. Called inside the same transaction as
-- CreateApplication (Flow 1 step 23), and available on its own as the retry path
-- for an application saved while scoring was unavailable.
UPDATE applications
SET ai_score             = sqlc.narg('ai_score'),
    ai_score_explanation = sqlc.narg('ai_score_explanation'),
    updated_at           = now()
WHERE applications.id = sqlc.arg('id') AND applications.user_id = sqlc.arg('user_id')
RETURNING *;
