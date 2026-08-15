package notifications

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// Service is the business logic boundary for the notifications module. Both
// Lambdas call it: the scheduler runs CheckReminders once per EventBridge
// invocation, and the API can run it on demand or serve ListDue to the
// dashboard.
//
// Phase 15: CheckReminders is deliberately the one method in this codebase
// with no userID parameter. The scheduler has no request and no single caller
// — its sweep is for everyone in one pass, same as before multi-tenancy — so it
// passes nil down to Repository.FindDue itself. ListDue has a real caller and
// takes their userID, so the dashboard only ever sees its own reminders.
type Service interface {
	// CheckReminders performs one sweep — flow 4 phases 2 to 4 — across every
	// user's reminders. It finds the reminders that have come due, dispatches a
	// notification for each and marks each one sent. A failure on one reminder
	// does not stop the rest.
	CheckReminders(ctx context.Context) (*RunSummary, error)
	// ListDue returns what this user's dashboard should surface right now,
	// including reminders the sweep has already dispatched today.
	ListDue(ctx context.Context, userID uuid.UUID) ([]Notification, error)
}

// Notifier is the delivery port.
//
// Flow 4 steps 7–10 deliver by email through Resend, which is out of MVP scope.
// Keeping delivery behind this interface means the sweep, its failure handling
// and its statistics are the ones the flow describes, with only the transport
// swapped: a Web Push or Resend implementation drops in without touching the
// service.
//
// An error means the notification was not delivered. The reminder is then left
// unstamped, so the next daily run retries it — flow 4's failure branch.
type Notifier interface {
	Notify(ctx context.Context, n Notification) error
}

const (
	// DefaultGhostThreshold is how long an application must have been silent
	// before "Mark as Ghost" is worth suggesting. The follow-up itself is due
	// after applications.DefaultFollowUpDelay (7 days); giving up is a different
	// question from chasing, so it gets its own, longer threshold.
	DefaultGhostThreshold = 21 * 24 * time.Hour
)

type service struct {
	repo           Repository
	notifier       Notifier
	logger         *slog.Logger
	ghostThreshold time.Duration
	now            func() time.Time // injectable for deterministic tests
}

type ServiceOption func(*service)

// WithNotifier sets the delivery transport. Without it the service falls back to
// LogNotifier.
func WithNotifier(n Notifier) ServiceOption {
	return func(s *service) { s.notifier = n }
}

func WithLogger(l *slog.Logger) ServiceOption {
	return func(s *service) { s.logger = l }
}

func WithGhostThreshold(d time.Duration) ServiceOption {
	return func(s *service) { s.ghostThreshold = d }
}

func WithClock(f func() time.Time) ServiceOption {
	return func(s *service) { s.now = f }
}

func NewService(repo Repository, opts ...ServiceOption) Service {
	s := &service{
		repo:           repo,
		logger:         slog.Default(),
		ghostThreshold: DefaultGhostThreshold,
		now:            time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.notifier == nil {
		s.notifier = NewLogNotifier(s.logger)
	}
	return s
}

// CheckReminders implements flow 4 phases 2 to 4, for every user in one pass.
//
// Only a failure to read the due list aborts the run: at that point there is
// nothing to iterate. Everything inside the loop is per-reminder, so one bad
// application cannot cost the others their notification.
func (s *service) CheckReminders(ctx context.Context) (*RunSummary, error) {
	start := s.now().UTC()

	// nil: the sweep is not scoped to any one user. See the Service and
	// Repository comments on FindDue for why that is the deliberate exception
	// to every other userID parameter in this codebase.
	due, err := s.repo.FindDue(ctx, nil, DueFilter{AsOf: start, IncludeSent: false})
	if err != nil {
		return nil, fmt.Errorf("notifications: find due reminders: %w", err)
	}

	summary := &RunSummary{StartedAt: start, DueCount: len(due)}

	for _, reminder := range due {
		notification := s.buildNotification(reminder, start)

		if err := s.notifier.Notify(ctx, notification); err != nil {
			// Flow 4 step 10b: sent_at stays NULL, so tomorrow's run retries.
			summary.FailedCount++
			s.logger.ErrorContext(
				ctx, "notifications: reminder delivery failed",
				slog.String("reminder_id", reminder.ID.String()),
				slog.String("application_id", reminder.ApplicationID.String()),
				slog.String("company", reminder.Application.CompanyName),
				slog.String("error", err.Error()),
			)
			continue
		}

		summary.SentCount++

		// Flow 4 step 9. Delivery already happened, so a failure here cannot be
		// undone: it is logged and the reminder will be raised again tomorrow.
		// That is the duplicate risk flow 4 acknowledges and leaves to a later
		// phase.
		marked, err := s.repo.MarkSent(ctx, reminder.ID, start)
		switch {
		case err != nil:
			s.logger.WarnContext(
				ctx, "notifications: reminder sent but not marked — it will repeat tomorrow",
				slog.String("reminder_id", reminder.ID.String()),
				slog.String("error", err.Error()),
			)
		case !marked:
			// Sent or dismissed between the read and the write.
			s.logger.InfoContext(
				ctx, "notifications: reminder was no longer pending when marking sent",
				slog.String("reminder_id", reminder.ID.String()),
			)
		default:
			// Flow 4 step 10a.
			s.logger.InfoContext(
				ctx, "notifications: reminder sent",
				slog.String("reminder_id", reminder.ID.String()),
				slog.String("application_id", reminder.ApplicationID.String()),
				slog.String("company", reminder.Application.CompanyName),
				slog.Int("days_since_applied", notification.DaysSinceApplied),
			)
		}
	}

	summary.Duration = s.now().UTC().Sub(start)
	return summary, nil
}

// ListDue backs GET /notifications/due, scoped to the caller.
//
// IncludeSent is true here and false in the sweep, and that difference is the
// whole design: the sweep must not raise the same reminder twice in a day, but
// the dashboard opened at noon should still show what was raised at 08:00.
// Dismissing is what removes a reminder from this list, and that happens in the
// applications module when the application stops being Applied.
func (s *service) ListDue(ctx context.Context, userID uuid.UUID) ([]Notification, error) {
	now := s.now().UTC()

	due, err := s.repo.FindDue(ctx, &userID, DueFilter{AsOf: now, IncludeSent: true})
	if err != nil {
		return nil, fmt.Errorf("notifications: find due reminders: %w", err)
	}

	out := make([]Notification, 0, len(due))
	for _, reminder := range due {
		out = append(out, s.buildNotification(reminder, now))
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Notification construction
// ---------------------------------------------------------------------------

func (s *service) buildNotification(r FollowUpReminder, now time.Time) Notification {
	days := daysSince(silentSince(r), now)

	return Notification{
		ReminderID:       r.ID,
		ApplicationID:    r.ApplicationID,
		Title:            fmt.Sprintf("Follow up with %s", r.Application.CompanyName),
		Body:             notificationBody(r.Application, days),
		CompanyName:      r.Application.CompanyName,
		JobTitle:         r.Application.JobTitle,
		JobURL:           r.Application.JobURL,
		Status:           r.Application.Status,
		DueAt:            r.DueAt,
		DaysSinceApplied: days,
		SentAt:           r.SentAt,
		Actions:          s.suggestedActions(r, days),
	}
}

// suggestedActions is the three actions from the phase brief, in that order.
//
// Chasing and checking always make sense once a reminder is due. Giving up does
// not: "Mark as Ghost" is offered only once the application has been silent for
// ghostThreshold, so a nudge on day 7 does not invite the user to write the job
// off two weeks early.
func (s *service) suggestedActions(r FollowUpReminder, days int) []SuggestedAction {
	actions := make([]SuggestedAction, 0, 3)

	actions = append(
		actions, SuggestedAction{
			Kind:  ActionSendFollowUp,
			Label: "Send a follow-up email",
		},
	)

	if s.isGhostCandidate(days) {
		actions = append(
			actions, SuggestedAction{
				Kind:  ActionMarkGhost,
				Label: "Mark as Ghost",
			},
		)
	}

	actions = append(
		actions, SuggestedAction{
			Kind:  ActionCheckLinkedIn,
			Label: "Check LinkedIn for updates",
			URL:   r.Application.JobURL,
		},
	)

	return actions
}

func (s *service) isGhostCandidate(days int) bool {
	return time.Duration(days)*24*time.Hour >= s.ghostThreshold
}

func notificationBody(app ApplicationSummary, days int) string {
	return fmt.Sprintf(
		"You applied for %s at %s %s and there has been no reply.",
		app.JobTitle, app.CompanyName, appliedPhrase(days),
	)
}

func appliedPhrase(days int) string {
	switch {
	case days <= 0:
		return "today"
	case days == 1:
		return "yesterday"
	default:
		return fmt.Sprintf("%d days ago", days)
	}
}

// silentSince is the moment the clock started. applied_at is the honest answer,
// but it is nullable — an application saved as Saved and moved to Applied by a
// path that left it unset would have none — so the reminder's own creation time
// is the fallback. The reminder cannot predate the application, so this never
// overstates the silence.
func silentSince(r FollowUpReminder) time.Time {
	if r.Application.AppliedAt != nil {
		return *r.Application.AppliedAt
	}
	return r.CreatedAt
}

func daysSince(from, now time.Time) int {
	elapsed := now.Sub(from)
	if elapsed < 0 {
		return 0
	}
	return int(elapsed / (24 * time.Hour))
}

// ---------------------------------------------------------------------------
// Delivery
// ---------------------------------------------------------------------------

// LogNotifier is the MVP delivery implementation: it writes a structured line
// and nothing else.
//
// The user-visible browser notification is raised on the client. The dashboard
// polls GET /notifications/due and calls the Notification API itself, which is
// what flow 4's legend means by browser push being "a separate mechanism, fires
// from the extension when the user opens the dashboard". Server-initiated Web
// Push would need a stored subscription per browser and VAPID keys — neither is
// in the ERD — and email needs Resend, which is out of scope. So the sweep's job
// in the MVP is to decide what is due and record that it was raised.
type LogNotifier struct {
	logger *slog.Logger
}

func NewLogNotifier(logger *slog.Logger) *LogNotifier {
	if logger == nil {
		logger = slog.Default()
	}
	return &LogNotifier{logger: logger}
}

func (n *LogNotifier) Notify(ctx context.Context, notification Notification) error {
	n.logger.InfoContext(
		ctx, "notifications: reminder raised",
		slog.String("event", "reminder_raised"),
		slog.String("reminder_id", notification.ReminderID.String()),
		slog.String("application_id", notification.ApplicationID.String()),
		slog.String("title", notification.Title),
		slog.String("body", notification.Body),
		slog.Int("days_since_applied", notification.DaysSinceApplied),
		slog.Int("suggested_actions", len(notification.Actions)),
	)
	return nil
}
