-- Reminders to chase an application that has gone quiet. Created and swept by
-- the applymind-scheduler Lambda (EventBridge cron 0 8 * * ? *).
CREATE TABLE follow_up_reminders (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id uuid        NOT NULL
                               REFERENCES applications (id) ON DELETE CASCADE,
    due_at         timestamptz NOT NULL,

    -- Set when the reminder email actually goes out via Resend.
    sent_at        timestamptz,
    -- Set when the user dismisses the reminder without acting on it.
    dismissed_at   timestamptz,

    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

-- The ERD marks application_id UNIQUE outright, which would permit only one
-- reminder per application for its entire lifetime. Replaced with a partial
-- unique index: at most one *pending* reminder at a time, but a new one may be
-- created once the previous is sent or dismissed.
CREATE UNIQUE INDEX uq_follow_up_reminders_one_pending
    ON follow_up_reminders (application_id)
    WHERE sent_at IS NULL AND dismissed_at IS NULL;

-- Supports the scheduler's "what is due and not yet sent?" sweep.
CREATE INDEX idx_follow_up_reminders_due_at_pending
    ON follow_up_reminders (due_at)
    WHERE sent_at IS NULL;

CREATE TRIGGER trg_follow_up_reminders_updated_at
    BEFORE UPDATE ON follow_up_reminders
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
