-- applications.site_id is NOT NULL, so at least one site must exist before any
-- application can be inserted. LinkedIn is the only supported site in the MVP.
-- selectors is left NULL until the extension phase defines the scrape targets.
INSERT INTO sites (name, domain, is_preconfigured, is_active, selectors)
VALUES ('LinkedIn', 'linkedin.com', true, true, NULL)
ON CONFLICT (domain) DO NOTHING;
