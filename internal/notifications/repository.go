package notifications

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	sqlcdb "github.com/EngenMe/applymind-backend/internal/db/sqlc"
)

// Repository is the persistence boundary for the notifications module.
//
// There is no Tx method: the sweep writes at most one row per reminder and each
// write stands alone — a reminder that fails to be stamped must not roll back
// the ones before it — so NewRepository takes only the Queries, like sites, cvs
// and coverletters.
//
// Creating and dismissing reminders is deliberately absent. Both happen during
// an application save or status change and belong to that module's transaction;
// this one only reads reminders and marks them sent.
type Repository interface {
	// FindDue returns reminders that have come due, oldest first, with their
	// application snapshot attached. An empty result is the normal outcome on
	// most days, not an error.
	FindDue(ctx context.Context, f DueFilter) ([]FollowUpReminder, error)
	// MarkSent stamps sent_at. It reports (false, nil) when there was nothing to
	// stamp — already sent, or dismissed between the sweep's read and this write
	// — because a repeated run must be a no-op rather than a failure.
	MarkSent(ctx context.Context, id uuid.UUID, at time.Time) (bool, error)
}

// postgresRepository is the sqlc-backed implementation.
//
// Per sqlc.yaml: uuid -> google/uuid.UUID, timestamptz -> time.Time (pointer
// when nullable), so the generated row maps across with no conversion beyond
// the application_status enum cast.
type postgresRepository struct {
	q *sqlcdb.Queries
}

func NewRepository(q *sqlcdb.Queries) Repository {
	return &postgresRepository{q: q}
}

func (r *postgresRepository) FindDue(ctx context.Context, f DueFilter) ([]FollowUpReminder, error) {
	rows, err := r.q.FindDueFollowUpReminders(
		ctx, sqlcdb.FindDueFollowUpRemindersParams{
			AsOf:        f.AsOf,
			IncludeSent: f.IncludeSent,
		},
	)
	if err != nil {
		return nil, err
	}

	out := make([]FollowUpReminder, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomain(row))
	}
	return out, nil
}

func (r *postgresRepository) MarkSent(ctx context.Context, id uuid.UUID, at time.Time) (bool, error) {
	_, err := r.q.MarkFollowUpReminderSent(
		ctx, sqlcdb.MarkFollowUpReminderSentParams{
			ID:     id,
			SentAt: at,
		},
	)
	// The sent_at IS NULL / dismissed_at IS NULL guard matched nothing: somebody
	// got there first. Intended, not a failure.
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ---------------------------------------------------------------------------
// Row mapping
// ---------------------------------------------------------------------------

func toDomain(row sqlcdb.FindDueFollowUpRemindersRow) FollowUpReminder {
	return FollowUpReminder{
		ID:            row.ID,
		ApplicationID: row.ApplicationID,
		DueAt:         row.DueAt,
		SentAt:        row.SentAt,
		DismissedAt:   row.DismissedAt,
		CreatedAt:     row.CreatedAt,
		UpdatedAt:     row.UpdatedAt,
		Application: ApplicationSummary{
			CompanyName: row.CompanyName,
			JobTitle:    row.JobTitle,
			JobURL:      row.JobUrl,
			Status:      string(row.Status),
			AppliedAt:   row.AppliedAt,
		},
	}
}
