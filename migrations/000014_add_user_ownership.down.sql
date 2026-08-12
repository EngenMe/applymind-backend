-- Dropping the columns takes their indexes and foreign keys with them.
DROP INDEX IF EXISTS idx_applications_user_status;

ALTER TABLE recruiter_contacts         DROP COLUMN IF EXISTS user_id;
ALTER TABLE follow_up_reminders        DROP COLUMN IF EXISTS user_id;
ALTER TABLE application_status_history DROP COLUMN IF EXISTS user_id;
ALTER TABLE cover_letters              DROP COLUMN IF EXISTS user_id;
ALTER TABLE applications               DROP COLUMN IF EXISTS user_id;
ALTER TABLE cv_versions                DROP COLUMN IF EXISTS user_id;
ALTER TABLE cvs                        DROP COLUMN IF EXISTS user_id;
ALTER TABLE sites                      DROP COLUMN IF EXISTS user_id;

-- The seed account is left in place. Rolling back the schema is not a reason
-- to delete a user row, and 000013's down migration drops the table anyway if
-- that is genuinely what is wanted.
