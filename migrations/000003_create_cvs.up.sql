-- A logical CV ("Backend Go CV", "Generalist CV"). The actual uploaded files
-- live in cv_versions; this row is the stable identity across versions.
CREATE TABLE cvs (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text        NOT NULL UNIQUE,
    -- Free-form user label, e.g. "golang", "frontend". Optional.
    tag        text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TRIGGER trg_cvs_updated_at
    BEFORE UPDATE ON cvs
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
