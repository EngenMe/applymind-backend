-- Single-row user settings.
--
-- MVP scope is one user and no login, so this table holds exactly one row and
-- has nothing to key it by. A boolean primary key defaulting to true, with a
-- CHECK that it is true, makes a second row impossible: any insert either lands
-- on id = true or is rejected. When a users table arrives this becomes
-- user_id uuid PRIMARY KEY REFERENCES users(id) and nothing else here changes.
--
-- profile_summary is the 3-4 sentences the AI job score is calculated against.
-- It is nullable so "never set" is distinguishable from "set to nothing", which
-- is what lets the applications module skip scoring rather than score a job
-- against an empty profile.
CREATE TABLE IF NOT EXISTS settings (
    id              boolean     PRIMARY KEY DEFAULT true,
    profile_summary text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT ck_settings_single_row CHECK (id)
);

-- Seed the row so a GET before the first PUT reads a real row rather than
-- nothing. UpsertProfileSummary would recreate it anyway; this makes the state
-- after migration match the state after use.
INSERT INTO settings (id) VALUES (true)
ON CONFLICT (id) DO NOTHING;
