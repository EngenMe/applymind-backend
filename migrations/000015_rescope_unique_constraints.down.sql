-- Restores the single-user constraints.
--
-- This can fail, legitimately: if two users have saved the same job URL or
-- added the same custom domain, there is no way to restore a global unique
-- constraint without deciding whose row to delete. That decision does not
-- belong in a migration, so it fails loudly instead.

DROP INDEX IF EXISTS uq_sites_user_name;
DROP INDEX IF EXISTS uq_sites_global_name;
DROP INDEX IF EXISTS uq_sites_user_domain;
DROP INDEX IF EXISTS uq_sites_global_domain;

ALTER TABLE sites ADD CONSTRAINT sites_domain_key UNIQUE (domain);
ALTER TABLE sites ADD CONSTRAINT sites_name_key   UNIQUE (name);

ALTER TABLE cv_versions DROP CONSTRAINT IF EXISTS uq_cv_versions_user_cv_hash;
ALTER TABLE cv_versions
    ADD CONSTRAINT uq_cv_versions_cv_hash UNIQUE (cv_id, sha256_hash);

ALTER TABLE applications DROP CONSTRAINT IF EXISTS uq_applications_user_site_job_url;
ALTER TABLE applications
    ADD CONSTRAINT uq_applications_site_job_url UNIQUE (site_id, job_url);
