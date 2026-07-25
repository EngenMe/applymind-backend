-- Immutable record of one uploaded CV file. Rows are never updated, only
-- inserted, which is why there is no updated_at column: uploaded_at is the
-- only timestamp that can ever be true for a given row.
CREATE TABLE cv_versions (
    id                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    cv_id             uuid        NOT NULL
                                  REFERENCES cvs (id) ON DELETE RESTRICT,
    -- SHA-256 of the file bytes, used to detect re-uploads of an identical file.
    sha256_hash       text        NOT NULL,
    file_size_bytes   bigint      NOT NULL,
    original_filename text        NOT NULL,
    -- Object key under the cvs/ prefix in the applymind-files bucket.
    s3_key            text        NOT NULL UNIQUE,
    uploaded_at       timestamptz NOT NULL DEFAULT now(),

    -- The same file content may not be uploaded twice against one CV, but
    -- two different CVs may legitimately share identical content.
    CONSTRAINT uq_cv_versions_cv_id_hash UNIQUE (cv_id, sha256_hash)
);

CREATE INDEX idx_cv_versions_cv_id ON cv_versions (cv_id);
CREATE INDEX idx_cv_versions_hash  ON cv_versions (sha256_hash);
