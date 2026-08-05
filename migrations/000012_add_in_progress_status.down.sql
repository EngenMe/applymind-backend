-- PostgreSQL cannot drop a value from an enum, so the type is rebuilt without
-- it. Any row still carrying the value has to be moved off it first.
--
-- Applications mid-apply fall back to Saved: the job is on record, nothing was
-- submitted. Their history rows are deleted rather than rewritten, because a
-- transition into a status that no longer exists is not a fact worth keeping —
-- and rewriting to_status to 'Saved' would trip
-- ck_status_history_actual_transition wherever from_status was already Saved.
DELETE FROM application_status_history WHERE to_status = 'In Progress';
UPDATE application_status_history SET from_status = NULL WHERE from_status = 'In Progress';
UPDATE applications SET status = 'Saved' WHERE status = 'In Progress';

ALTER TYPE application_status RENAME TO application_status_old;

CREATE TYPE application_status AS ENUM (
    'Saved',
    'Applied',
    'Acknowledged',
    'In Review',
    'Interview Scheduled',
    'Interviewing',
    'Offer Received',
    'Accepted',
    'Rejected',
    'Withdrawn',
    'Ghost'
    );

ALTER TABLE applications ALTER COLUMN status DROP DEFAULT;
ALTER TABLE applications
    ALTER COLUMN status TYPE application_status USING status::text::application_status;
ALTER TABLE applications ALTER COLUMN status SET DEFAULT 'Applied';

ALTER TABLE application_status_history
    ALTER COLUMN from_status TYPE application_status USING from_status::text::application_status;
ALTER TABLE application_status_history
    ALTER COLUMN to_status TYPE application_status USING to_status::text::application_status;

DROP TYPE application_status_old;