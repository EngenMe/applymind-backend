-- Job boards / company career sites that applications originate from.
-- Pre-configured rows (is_preconfigured = true) ship with the product and
-- cannot be deleted, only deactivated. That rule is enforced in the service
-- layer, not here, per the ERD's "business rule (service layer)" note.
CREATE TABLE sites (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name             text        NOT NULL UNIQUE,
    domain           text        NOT NULL UNIQUE,
    is_preconfigured boolean     NOT NULL DEFAULT false,
    is_active        boolean     NOT NULL DEFAULT true,
    -- CSS/XPath selectors the browser extension uses to scrape this site.
    -- Shape is site-specific and expected to change often, hence jsonb.
    selectors        jsonb,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_sites_domain    ON sites (domain);
CREATE INDEX idx_sites_is_active ON sites (is_active);

CREATE TRIGGER trg_sites_updated_at
    BEFORE UPDATE ON sites
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
