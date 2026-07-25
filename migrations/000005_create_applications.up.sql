-- The central table: one row per job application.
CREATE TABLE applications (
    id                  uuid               PRIMARY KEY DEFAULT gen_random_uuid(),
    company_name        text               NOT NULL,
    job_title           text               NOT NULL,
    job_description     text               NOT NULL,
    job_url             text               NOT NULL,

    -- Which site the application originated from. RESTRICT (not CASCADE):
    -- deleting a site must never silently destroy application history.
    site_id             uuid               NOT NULL
                                           REFERENCES sites (id) ON DELETE RESTRICT,

    -- Nullable: an application in 'Saved' status has not been sent yet, so no
    -- CV version is attached. RESTRICT so a CV version that was actually sent
    -- to an employer can never be deleted out from under the record.
    cv_version_id       uuid               REFERENCES cv_versions (id) ON DELETE RESTRICT,

    status              application_status NOT NULL DEFAULT 'Applied',

    -- AI job-match score from GPT-4o-mini. Widened from the ERD's numeric(3,1),
    -- which caps at 99.9 and cannot represent a 0-100 score. numeric(4,1)
    -- accommodates either a 0-10 or 0-100 scale; narrow it later if the scale
    -- is settled. No upper CHECK for the same reason.
    ai_score            numeric(4,1),
    ai_score_explanation text,

    -- Nullable: only set once the application is actually submitted.
    applied_at          timestamptz,
    created_at          timestamptz        NOT NULL DEFAULT now(),
    updated_at          timestamptz        NOT NULL DEFAULT now(),

    CONSTRAINT ck_applications_ai_score_non_negative
        CHECK (ai_score IS NULL OR ai_score >= 0),

    -- Not in the ERD. Database-level backstop for the product goal "never
    -- accidentally apply to the same job twice"; a service-layer check alone
    -- can race under concurrent inserts from the extension.
    CONSTRAINT uq_applications_site_job_url UNIQUE (site_id, job_url)
);

CREATE INDEX idx_applications_company_name ON applications (company_name);
CREATE INDEX idx_applications_status       ON applications (status);
CREATE INDEX idx_applications_applied_at   ON applications (applied_at DESC);

CREATE TRIGGER trg_applications_updated_at
    BEFORE UPDATE ON applications
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
