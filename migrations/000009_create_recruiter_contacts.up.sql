-- Optional, one-to-one with applications. Kept in a separate table for PII
-- isolation: recruiter names, emails and profile URLs are third-party personal
-- data, so they sit in one table that can be independently access-controlled,
-- exported or purged. This is a data-protection boundary, not a storage
-- optimisation.
CREATE TABLE recruiter_contacts (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id uuid        NOT NULL UNIQUE
                               REFERENCES applications (id) ON DELETE CASCADE,
    name           text,
    email          text,
    linkedin_url   text,
    notes          text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),

    -- A contact row with no way to identify or reach the contact is meaningless.
    CONSTRAINT ck_recruiter_contacts_identifiable CHECK (
        name IS NOT NULL OR email IS NOT NULL OR linkedin_url IS NOT NULL
    )
);

CREATE TRIGGER trg_recruiter_contacts_updated_at
    BEFORE UPDATE ON recruiter_contacts
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
