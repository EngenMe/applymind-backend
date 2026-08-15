-- cvs.name was the one table 000015 left globally unique while rescoping every
-- other single-user constraint to per-user. That was an omission, not a
-- decision: cvs.Service.availableCVName falls back to a bare "CV" (then
-- "CV (2)", "CV (3)"...) whenever an upload gives it nothing better to name
-- the group after, and two different users landing on that same fallback is
-- exactly the collision 000015 already fixed for sites.name — see that
-- migration's reasoning, which this mirrors.
--
-- The original constraint's name isn't known here — migration 000003, which
-- created the table, wasn't available when this was written — so it's found
-- and dropped by column rather than by a guessed name. If cvs.name was never
-- separately unique (e.g. only enforced via an index), this is a no-op.
DO $$
DECLARE
    conname text;
BEGIN
    SELECT tc.constraint_name INTO conname
    FROM information_schema.table_constraints tc
    JOIN information_schema.constraint_column_usage ccu
        ON tc.constraint_name = ccu.constraint_name
       AND tc.table_schema = ccu.table_schema
    WHERE tc.table_name = 'cvs'
      AND tc.constraint_type = 'UNIQUE'
      AND ccu.column_name = 'name';

    IF conname IS NOT NULL THEN
        EXECUTE format('ALTER TABLE cvs DROP CONSTRAINT %I', conname);
    END IF;
END $$;

ALTER TABLE cvs
    ADD CONSTRAINT uq_cvs_user_name
    UNIQUE (user_id, name);
