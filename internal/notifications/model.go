package notifications

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Domain models. No database or HTTP tags — mapping lives in repository.go and
// handler.go respectively, matching the applications, sites, cvs and
// coverletters modules.

// FollowUpReminder is one follow_up_reminders row together with the snapshot of
// its application needed to describe it. Flow 4 reads the reminder (step 3) and
// then its application (step 5); here the two arrive in one query, because the
// second read has no purpose other than filling in this snapshot.
//
// A reminder is *pending* while both SentAt and DismissedAt are nil — the same
// definition as the uq_follow_up_reminders_one_pending partial unique index,
// and the same one applications.EnsurePendingReminder relies on.
type FollowUpReminder struct {
	ID            uuid.UUID
	ApplicationID uuid.UUID
	DueAt         time.Time
	// SentAt is stamped by the daily sweep once the notification has been
	// dispatched, so the same reminder is not raised again tomorrow.
	SentAt *time.Time
	// DismissedAt is stamped by the applications module when the application
	// leaves Applied — somebody replied, so there is nothing left to chase.
	DismissedAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Application ApplicationSummary
}

// Pending reports whether the reminder is still outstanding.
func (r FollowUpReminder) Pending() bool {
	return r.SentAt == nil && r.DismissedAt == nil
}

// ApplicationSummary is a read-only projection of the applications row behind a
// reminder.
//
// Status is a plain string rather than applications.Status on purpose: this
// module only reads it and never validates or writes it, and the two modules do
// not depend on each other — the same reasoning behind applications keeping its
// own read-only Site projection.
type ApplicationSummary struct {
	CompanyName string
	JobTitle    string
	JobURL      string
	Status      string
	AppliedAt   *time.Time
}

// ActionKind identifies a suggested action for the dashboard, which decides how
// to render it. The strings are the wire contract, so they are stable even if
// the labels are reworded.
type ActionKind string

const (
	ActionSendFollowUp  ActionKind = "send_follow_up_email"
	ActionMarkGhost     ActionKind = "mark_as_ghost"
	ActionCheckLinkedIn ActionKind = "check_linkedin"
)

// SuggestedAction is one thing the user could do about a silent application.
// URL is set only when the action has somewhere to go — currently the job
// posting, for the LinkedIn check.
type SuggestedAction struct {
	Kind  ActionKind
	Label string
	URL   string
}

// Notification is what the user is shown about an application that has gone
// quiet.
//
// It is built on demand and never stored: the ERD has no notifications table,
// and follow_up_reminders already records everything durable about a reminder
// (when it was due, when it went out, whether it was dismissed). The same value
// is handed to the Notifier by the daily sweep and serialised by
// GET /notifications/due for the dashboard.
type Notification struct {
	ReminderID       uuid.UUID
	ApplicationID    uuid.UUID
	Title            string
	Body             string
	CompanyName      string
	JobTitle         string
	JobURL           string
	Status           string
	DueAt            time.Time
	DaysSinceApplied int
	// SentAt is non-nil when the daily sweep has already dispatched this one.
	// The dashboard still receives it — see Service.ListDue — so it can show a
	// reminder raised at 08:00 to a user who opens the tab at noon.
	SentAt  *time.Time
	Actions []SuggestedAction
}

// AlreadyNotified reports whether the daily sweep has dispatched this
// notification already.
func (n Notification) AlreadyNotified() bool { return n.SentAt != nil }

// DueFilter selects what counts as due. AsOf is flow 4's NOW(), injected rather
// than taken from the database so a sweep is reproducible in tests.
//
// IncludeSent keeps reminders the sweep has already dispatched. The sweep sets
// it false (flow 4 step 3: sent_at IS NULL, which is what stops a second run on
// the same day re-notifying); the dashboard poll sets it true, because a
// reminder does not stop being relevant the moment it is logged.
//
// Dismissed reminders are excluded either way. Flow 4's SELECT predates the
// dismissed_at column and does not mention it, but applications marks a
// reminder dismissed precisely because it should never be raised again.
type DueFilter struct {
	AsOf        time.Time
	IncludeSent bool
}

// RunSummary is flow 4 step 11: the statistics one sweep produced, logged as
// structured JSON in step 12.
//
// SentCount counts notifications the Notifier accepted. A reminder whose
// delivery succeeded but whose sent_at stamp failed is still counted here — it
// was sent — and is logged as a warning, since the next daily run will raise it
// again. That is flow 4's acknowledged duplicate risk, unmitigated in MVP.
type RunSummary struct {
	StartedAt   time.Time
	Duration    time.Duration
	DueCount    int
	SentCount   int
	FailedCount int
}

// Domain errors.
var (
	// ErrNotFound is kept for the Get* convention shared with the other
	// modules; nothing in this phase returns it, because a reminder that has
	// vanished between the sweep's read and its write is a no-op, not a failure.
	ErrNotFound = errors.New("notifications: reminder not found")
)
