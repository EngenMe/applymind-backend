-- Ownership. Every table that holds user data gains a user_id.
--
-- Three steps, in this order, and the order is the whole point:
--   1. create the account that will own the data that already exists
--   2. add the column as nullable and backfill it
--   3. only then make it NOT NULL
-- Adding it as NOT NULL first would fail immediately against non-empty tables.
--
-- user_id is denormalised onto child tables — cover_letters, status history,
-- reminders, recruiter contacts — rather than reached through applications.
-- That costs a column and buys a single-clause filter on every query. In an
-- app where a forgotten join means showing one person another person's CV,
-- the redundancy is the safer default.

-- ---------------------------------------------------------------------------
-- 1. The account that inherits everything built before auth existed.
--
-- The password hash is deliberately unusable: it is not a valid bcrypt string,
-- so no password can ever verify against it. A real one is set after this
-- migration runs, out of band — a working credential does not belong in a
-- file that gets committed to a public repository.
-- ---------------------------------------------------------------------------
INSERT INTO users (email, password_hash, display_name, email_verified_at)
VALUES (
    'mohamdfarouk727@gmail.com',
    'x-set-a-real-password-after-migrating',
    'Farouk',
    now()
)
ON CONFLICT (email) DO NOTHING;

-- ---------------------------------------------------------------------------
-- 2. Add the column, backfill, enforce.
-- ---------------------------------------------------------------------------

-- sites is the exception: user_id stays nullable. NULL means a pre-configured
-- row that belongs to everyone — LinkedIn, Indeed, Greenhouse. A non-null
-- user_id is somebody's own addition, visible only to them.
ALTER TABLE sites ADD COLUMN user_id uuid REFERENCES users (id) ON DELETE CASCADE;

-- Custom sites added before auth become the seed account's; seeded rows stay
-- global.
UPDATE sites
SET user_id = (SELECT id FROM users WHERE email = 'mohamdfarouk727@gmail.com')
WHERE is_preconfigured = false;

ALTER TABLE cvs ADD COLUMN user_id uuid REFERENCES users (id) ON DELETE CASCADE;
ALTER TABLE cv_versions ADD COLUMN user_id uuid REFERENCES users (id) ON DELETE CASCADE;
ALTER TABLE applications ADD COLUMN user_id uuid REFERENCES users (id) ON DELETE CASCADE;
ALTER TABLE cover_letters ADD COLUMN user_id uuid REFERENCES users (id) ON DELETE CASCADE;
ALTER TABLE application_status_history ADD COLUMN user_id uuid REFERENCES users (id) ON DELETE CASCADE;
ALTER TABLE follow_up_reminders ADD COLUMN user_id uuid REFERENCES users (id) ON DELETE CASCADE;
ALTER TABLE recruiter_contacts ADD COLUMN user_id uuid REFERENCES users (id) ON DELETE CASCADE;

UPDATE cvs                        SET user_id = (SELECT id FROM users WHERE email = 'mohamdfarouk727@gmail.com');
UPDATE cv_versions                SET user_id = (SELECT id FROM users WHERE email = 'mohamdfarouk727@gmail.com');
UPDATE applications               SET user_id = (SELECT id FROM users WHERE email = 'mohamdfarouk727@gmail.com');
UPDATE cover_letters              SET user_id = (SELECT id FROM users WHERE email = 'mohamdfarouk727@gmail.com');
UPDATE application_status_history SET user_id = (SELECT id FROM users WHERE email = 'mohamdfarouk727@gmail.com');
UPDATE follow_up_reminders        SET user_id = (SELECT id FROM users WHERE email = 'mohamdfarouk727@gmail.com');
UPDATE recruiter_contacts         SET user_id = (SELECT id FROM users WHERE email = 'mohamdfarouk727@gmail.com');

ALTER TABLE cvs                        ALTER COLUMN user_id SET NOT NULL;
ALTER TABLE cv_versions                ALTER COLUMN user_id SET NOT NULL;
ALTER TABLE applications               ALTER COLUMN user_id SET NOT NULL;
ALTER TABLE cover_letters              ALTER COLUMN user_id SET NOT NULL;
ALTER TABLE application_status_history ALTER COLUMN user_id SET NOT NULL;
ALTER TABLE follow_up_reminders        ALTER COLUMN user_id SET NOT NULL;
ALTER TABLE recruiter_contacts         ALTER COLUMN user_id SET NOT NULL;

-- ---------------------------------------------------------------------------
-- 3. Indexes. Every query the API makes now filters by user_id, so every one
--    of these tables needs it indexed.
-- ---------------------------------------------------------------------------
CREATE INDEX idx_sites_user_id                      ON sites (user_id);
CREATE INDEX idx_cvs_user_id                        ON cvs (user_id);
CREATE INDEX idx_cv_versions_user_id                ON cv_versions (user_id);
CREATE INDEX idx_applications_user_id               ON applications (user_id);
CREATE INDEX idx_cover_letters_user_id              ON cover_letters (user_id);
CREATE INDEX idx_status_history_user_id             ON application_status_history (user_id);
CREATE INDEX idx_follow_up_reminders_user_id        ON follow_up_reminders (user_id);
CREATE INDEX idx_recruiter_contacts_user_id         ON recruiter_contacts (user_id);

-- The application list, which is the most-run query in the product: "my
-- applications, optionally filtered by status, newest first".
CREATE INDEX idx_applications_user_status ON applications (user_id, status);
