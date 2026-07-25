-- Enum types and shared helpers for the ApplyMind MVP schema.
-- gen_random_uuid() is built into PostgreSQL 13+, so no pgcrypto extension
-- is required on Neon.

-- Labels are stored verbatim as drawn in the ERD (capitalised, with spaces).
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

-- Distinguishes a pasted/typed cover letter from an uploaded file.
CREATE TYPE cover_letter_kind AS ENUM (
    'text',
    'file'
);

-- Who caused a status transition: the user manually, or an automated rule.
CREATE TYPE status_change_source AS ENUM (
    'user',
    'system'
);

-- Shared trigger function. The ERD specifies DEFAULT now() for updated_at but
-- says nothing about maintaining it on UPDATE; enforcing it in the database
-- means no service method can forget to bump it.
CREATE OR REPLACE FUNCTION set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
