-- Unique constraints become per-user.
--
-- Every one of these was written when the database had exactly one user, so
-- "this job URL is already saved" and "this job URL is already saved *by you*"
-- were the same statement. They are not the same statement any more, and
-- leaving them global would mean one user's application silently blocking
-- another's — the worst kind of bug, because it looks like a duplicate check
-- working correctly.

-- ---------------------------------------------------------------------------
-- applications: the same posting applied to by two people is not a conflict.
-- ---------------------------------------------------------------------------
ALTER TABLE applications DROP CONSTRAINT uq_applications_site_job_url;

ALTER TABLE applications
    ADD CONSTRAINT uq_applications_user_site_job_url
    UNIQUE (user_id, site_id, job_url);

-- ---------------------------------------------------------------------------
-- cv_versions: two people uploading the identical file is coincidence, not a
-- duplicate. Only a repeat within one account is.
-- ---------------------------------------------------------------------------
ALTER TABLE cv_versions DROP CONSTRAINT IF EXISTS uq_cv_versions_cv_hash;
ALTER TABLE cv_versions DROP CONSTRAINT IF EXISTS cv_versions_cv_id_sha256_hash_key;

ALTER TABLE cv_versions
    ADD CONSTRAINT uq_cv_versions_user_cv_hash
    UNIQUE (user_id, cv_id, sha256_hash);

-- ---------------------------------------------------------------------------
-- sites: the tricky one.
--
-- Pre-configured rows (user_id IS NULL) must stay globally unique — two
-- LinkedIn rows would be a bug. Custom rows must be unique only within their
-- owner, so two users can each add the same niche job board.
--
-- One constraint cannot express both, because in Postgres NULLs never collide
-- in a unique index: unique(user_id, domain) would happily allow a hundred
-- rows for linkedin.com as long as user_id was NULL each time. Hence two
-- partial indexes, splitting on exactly that.
-- ---------------------------------------------------------------------------
ALTER TABLE sites DROP CONSTRAINT IF EXISTS sites_domain_key;
ALTER TABLE sites DROP CONSTRAINT IF EXISTS sites_name_key;

CREATE UNIQUE INDEX uq_sites_global_domain
    ON sites (domain) WHERE user_id IS NULL;

CREATE UNIQUE INDEX uq_sites_user_domain
    ON sites (user_id, domain) WHERE user_id IS NOT NULL;

-- Name follows the same rule. It is a display label, so a collision between
-- two users' custom sites is harmless, but two global "LinkedIn" rows are not.
CREATE UNIQUE INDEX uq_sites_global_name
    ON sites (name) WHERE user_id IS NULL;

CREATE UNIQUE INDEX uq_sites_user_name
    ON sites (user_id, name) WHERE user_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- cover_letters, follow_up_reminders, recruiter_contacts
--
-- These carry unique(application_id) — one cover letter per application, one
-- pending reminder per application. Those stay exactly as they are: an
-- application already belongs to exactly one user, so the constraint is
-- already per-user by construction. Adding user_id to them would be noise.
-- ---------------------------------------------------------------------------
