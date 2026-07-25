-- One-to-one with applications (UNIQUE on application_id). Unlike CVs there is
-- deliberately no version history and no template table: a cover letter is
-- written once for one specific application.
CREATE TABLE cover_letters (
    id                uuid              PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id    uuid              NOT NULL UNIQUE
                                        REFERENCES applications (id) ON DELETE CASCADE,
    kind              cover_letter_kind NOT NULL,

    -- Exactly one of (body_text) or (s3_key + original_filename) is populated,
    -- selected by kind. Enforced by ck_cover_letters_kind_payload below.
    body_text         text,
    s3_key            text              UNIQUE,
    original_filename text,

    created_at        timestamptz       NOT NULL DEFAULT now(),
    updated_at        timestamptz       NOT NULL DEFAULT now(),

    CONSTRAINT ck_cover_letters_kind_payload CHECK (
        (kind = 'text'
            AND body_text IS NOT NULL
            AND s3_key IS NULL
            AND original_filename IS NULL)
        OR
        (kind = 'file'
            AND body_text IS NULL
            AND s3_key IS NOT NULL
            AND original_filename IS NOT NULL)
    )
);

CREATE TRIGGER trg_cover_letters_updated_at
    BEFORE UPDATE ON cover_letters
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
