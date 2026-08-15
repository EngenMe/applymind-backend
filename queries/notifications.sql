-- Queries for the notifications module. Reminder *creation* and *dismissal* stay
-- in queries/applications.sql: they happen during a save or a status change,
-- inside that module's transaction.

-- FindDueFollowUpReminders is flow 4 steps 3 and 5 collapsed into one round
-- trip: the due reminders plus the application data needed to describe each one.
--
-- Dismissed reminders are always excluded. @include_sent = false is the sweep
-- (flow 4's sent_at IS NULL, which makes a second run on the same day a no-op);
-- true is the dashboard poll, which still wants a reminder the sweep already
-- dispatched this morning.
--
-- Phase 15: @user_id is nullable, on purpose. The scheduler has no request, no
-- caller, no single user to scope to — its sweep runs for everyone in one pass,
-- so it passes NULL and every user's due reminders come back together. The
-- dashboard's GET /notifications/due has a real caller and passes their id, so
-- it only ever sees its own. Both are "the same query", just with the filter
-- turned off for the one caller that legitimately has no single user in mind.
--
-- name: FindDueFollowUpReminders :many
SELECT r.id,
       r.application_id,
       r.due_at,
       r.sent_at,
       r.dismissed_at,
       r.created_at,
       r.updated_at,
       a.company_name,
       a.job_title,
       a.job_url,
       a.status,
       a.applied_at
FROM follow_up_reminders r
         JOIN applications a ON a.id = r.application_id
WHERE r.dismissed_at IS NULL
  AND r.due_at <= @as_of::timestamptz
  AND (@include_sent::boolean OR r.sent_at IS NULL)
  AND (sqlc.narg('user_id')::uuid IS NULL OR r.user_id = sqlc.narg('user_id')::uuid)
ORDER BY r.due_at ASC;

-- MarkFollowUpReminderSent is flow 4 step 9, minus the resend_email_id
-- assignment: no such column exists in migration 000008 or the ERD, and no
-- email is sent in the MVP.
--
-- The sent_at IS NULL guard makes this idempotent — a concurrent or repeated
-- run updates nothing and returns no row rather than moving the timestamp.
--
-- Unscoped by user on purpose: the sweep already resolved which reminder to
-- mark via FindDueFollowUpReminders (NULL user_id, everyone's), and by the
-- time this runs the reminder id is trusted, not caller-supplied.
--
-- name: MarkFollowUpReminderSent :one
UPDATE follow_up_reminders
SET sent_at = @sent_at::timestamptz
WHERE id = @id
  AND sent_at IS NULL
  AND dismissed_at IS NULL
RETURNING *;
