-- Append-only audit trail of every status transition. No updated_at: rows are
-- inserted and never modified.
CREATE TABLE application_status_history (
    id             uuid                 PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id uuid                 NOT NULL
                                        REFERENCES applications (id) ON DELETE CASCADE,

    -- NULL on the very first transition (application created into its
    -- initial status, with nothing preceding it).
    from_status    application_status,
    to_status      application_status   NOT NULL,
    changed_by     status_change_source NOT NULL,
    note           text,
    changed_at     timestamptz          NOT NULL DEFAULT now(),

    -- A transition must actually change something.
    CONSTRAINT ck_status_history_actual_transition
        CHECK (from_status IS DISTINCT FROM to_status)
);

CREATE INDEX idx_status_history_application_id ON application_status_history (application_id);
CREATE INDEX idx_status_history_changed_at     ON application_status_history (changed_at DESC);
