package applications

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	sqlcdb "github.com/EngenMe/applymind-backend/internal/db/sqlc"
)

// Repository is the persistence boundary for the applications module.
//
// Convention matches cvs and coverletters: Get* returns ErrNotFound when the row
// is absent, because the caller asked for something specific; Find* returns an
// empty slice or (nil, nil) when nothing matches, because "no match" is a normal
// answer — the duplicate check is built on exactly that.
type Repository interface {
	// Tx runs fn inside a single database transaction and commits if it returns
	// nil. The Repository handed to fn is scoped to that transaction; use it
	// rather than the outer one for every write inside fn.
	//
	// This is what makes Flow 1 step 23 atomic for the three tables this module
	// owns: applications, application_status_history and follow_up_reminders.
	Tx(ctx context.Context, fn func(Repository) error) error

	Create(ctx context.Context, in NewApplication) (*Application, error)
	Get(ctx context.Context, id uuid.UUID) (*Application, error)
	List(ctx context.Context, f ListFilter) ([]Application, error)
	Update(ctx context.Context, id uuid.UUID, in UpdateFields) (*Application, error)
	UpdateStatus(ctx context.Context, id uuid.UUID, status Status, appliedAt *time.Time) (*Application, error)
	Delete(ctx context.Context, id uuid.UUID) error
	// FindByCompanyName backs the duplicate check: trimmed, case-insensitive,
	// exact company match.
	FindByCompanyName(ctx context.Context, company string) ([]Application, error)

	// SetAIScore writes the GPT-4o-mini job-match score onto an application.
	//
	// It is a write of its own rather than columns on Create so that an
	// application saved while scoring was unavailable can be scored later
	// without a second insert path: ai_score is nullable for exactly that, and
	// this is the method a retry would call. Create runs it inside its own
	// transaction, so on the happy path the row is never visible unscored.
	//
	// explanation is a pointer because the model may return a number without
	// prose; nil leaves ai_score_explanation NULL.
	SetAIScore(ctx context.Context, id uuid.UUID, score float64, explanation *string) (*Application, error)

	CreateStatusHistory(ctx context.Context, in NewStatusHistory) (*StatusHistory, error)
	ListStatusHistory(ctx context.Context, applicationID uuid.UUID) ([]StatusHistory, error)

	// EnsurePendingReminder schedules a follow-up unless one is already pending.
	EnsurePendingReminder(ctx context.Context, applicationID uuid.UUID, dueAt time.Time) error
	// DismissPendingReminders closes any un-sent, un-dismissed reminder.
	DismissPendingReminders(ctx context.Context, applicationID uuid.UUID) error

	// FindSiteByDomain returns (nil, nil) when no active site is registered for
	// the domain, so the service can turn that into a 400 with a useful message.
	FindSiteByDomain(ctx context.Context, domain string) (*Site, error)
}

// postgresRepository is the sqlc-backed implementation.
//
// Unlike cvs and coverletters it also holds the pool, because this module is the
// first one that needs to open a transaction of its own. A repository created by
// Tx carries a nil pool and a tx-scoped Queries — see Tx.
//
// Per sqlc.yaml: uuid -> google/uuid.UUID, timestamptz -> time.Time (pointer
// when nullable), and emit_pointers_for_null_types gives *string / *float64 for
// nullable columns, so no conversion layer is needed beyond the enum casts.
type postgresRepository struct {
	pool *pgxpool.Pool
	q    *sqlcdb.Queries
}

func NewRepository(pool *pgxpool.Pool, q *sqlcdb.Queries) Repository {
	return &postgresRepository{pool: pool, q: q}
}

func (r *postgresRepository) Tx(ctx context.Context, fn func(Repository) error) error {
	// Already inside a transaction: run in the same one rather than deadlocking
	// against ourselves on a second connection.
	if r.pool == nil {
		return fn(r)
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once committed

	if err := fn(&postgresRepository{q: r.q.WithTx(tx)}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ---------------------------------------------------------------------------
// Applications
// ---------------------------------------------------------------------------

func (r *postgresRepository) Create(ctx context.Context, in NewApplication) (*Application, error) {
	row, err := r.q.CreateApplication(
		ctx, sqlcdb.CreateApplicationParams{
			ID:             in.ID,
			CompanyName:    in.CompanyName,
			JobTitle:       in.JobTitle,
			JobDescription: in.JobDescription,
			JobUrl:         in.JobURL,
			SiteID:         in.SiteID,
			CvVersionID:    in.CVVersionID,
			Status:         sqlcdb.ApplicationStatus(in.Status),
			AppliedAt:      in.AppliedAt,
		},
	)
	if err != nil {
		return nil, translateWriteError(err)
	}
	app := toDomain(row)
	return &app, nil
}

func (r *postgresRepository) Get(ctx context.Context, id uuid.UUID) (*Application, error) {
	row, err := r.q.GetApplication(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	app := toDomain(row)
	return &app, nil
}

func (r *postgresRepository) List(ctx context.Context, f ListFilter) ([]Application, error) {
	params := sqlcdb.ListApplicationsParams{
		SiteID:      f.SiteID,
		CvVersionID: f.CVVersionID,
		Company:     f.Company,
		Search:      f.Search,
		FromDate:    f.From,
		ToDate:      f.To,
		RowLimit:    int32(f.Limit),
		RowOffset:   int32(f.Offset),
	}
	if f.Status != nil {
		status := sqlcdb.ApplicationStatus(*f.Status)
		params.Status = &status
	}

	rows, err := r.q.ListApplications(ctx, params)
	if err != nil {
		return nil, err
	}
	return toDomainSlice(rows), nil
}

func (r *postgresRepository) Update(ctx context.Context, id uuid.UUID, in UpdateFields) (*Application, error) {
	row, err := r.q.UpdateApplication(
		ctx, sqlcdb.UpdateApplicationParams{
			ID:             id,
			CompanyName:    in.CompanyName,
			JobTitle:       in.JobTitle,
			JobDescription: in.JobDescription,
			JobUrl:         in.JobURL,
			SiteID:         in.SiteID,
			CvVersionID:    in.CVVersionID,
		},
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, translateWriteError(err)
	}
	app := toDomain(row)
	return &app, nil
}

func (r *postgresRepository) UpdateStatus(
	ctx context.Context,
	id uuid.UUID,
	status Status,
	appliedAt *time.Time,
) (*Application, error) {
	row, err := r.q.UpdateApplicationStatus(
		ctx, sqlcdb.UpdateApplicationStatusParams{
			ID:        id,
			Status:    sqlcdb.ApplicationStatus(status),
			AppliedAt: appliedAt,
		},
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, translateWriteError(err)
	}
	app := toDomain(row)
	return &app, nil
}

func (r *postgresRepository) SetAIScore(
	ctx context.Context,
	id uuid.UUID,
	score float64,
	explanation *string,
) (*Application, error) {
	row, err := r.q.SetApplicationAIScore(
		ctx, sqlcdb.SetApplicationAIScoreParams{
			ID:                 id,
			AiScore:            &score,
			AiScoreExplanation: explanation,
		},
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, translateWriteError(err)
	}
	app := toDomain(row)
	return &app, nil
}

func (r *postgresRepository) Delete(ctx context.Context, id uuid.UUID) error {
	affected, err := r.q.DeleteApplication(ctx, id)
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *postgresRepository) FindByCompanyName(ctx context.Context, company string) ([]Application, error) {
	rows, err := r.q.FindApplicationsByCompanyName(ctx, company)
	if err != nil {
		return nil, err
	}
	return toDomainSlice(rows), nil
}

// ---------------------------------------------------------------------------
// Status history
// ---------------------------------------------------------------------------

func (r *postgresRepository) CreateStatusHistory(ctx context.Context, in NewStatusHistory) (*StatusHistory, error) {
	params := sqlcdb.CreateApplicationStatusHistoryParams{
		ApplicationID: in.ApplicationID,
		ToStatus:      sqlcdb.ApplicationStatus(in.ToStatus),
		ChangedBy:     sqlcdb.StatusChangeSource(in.ChangedBy),
		Note:          in.Note,
	}
	if in.FromStatus != nil {
		from := sqlcdb.ApplicationStatus(*in.FromStatus)
		params.FromStatus = &from
	}

	row, err := r.q.CreateApplicationStatusHistory(ctx, params)
	if err != nil {
		return nil, translateWriteError(err)
	}
	h := toDomainHistory(row)
	return &h, nil
}

func (r *postgresRepository) ListStatusHistory(ctx context.Context, applicationID uuid.UUID) (
	[]StatusHistory,
	error,
) {
	rows, err := r.q.ListApplicationStatusHistory(ctx, applicationID)
	if err != nil {
		return nil, err
	}
	out := make([]StatusHistory, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainHistory(row))
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Follow-up reminders
// ---------------------------------------------------------------------------

func (r *postgresRepository) EnsurePendingReminder(
	ctx context.Context,
	applicationID uuid.UUID,
	dueAt time.Time,
) error {
	_, err := r.q.CreatePendingFollowUpReminder(
		ctx, sqlcdb.CreatePendingFollowUpReminderParams{
			ApplicationID: applicationID,
			DueAt:         dueAt,
		},
	)
	// DO NOTHING returns no row when a reminder is already pending, which is the
	// intended outcome rather than a failure.
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return translateWriteError(err)
	}
	return nil
}

func (r *postgresRepository) DismissPendingReminders(ctx context.Context, applicationID uuid.UUID) error {
	return r.q.DismissPendingFollowUpReminders(ctx, applicationID)
}

// ---------------------------------------------------------------------------
// Sites
// ---------------------------------------------------------------------------

func (r *postgresRepository) FindSiteByDomain(ctx context.Context, domain string) (*Site, error) {
	row, err := r.q.ResolveSiteByDomain(ctx, domain)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &Site{ID: row.ID, Name: row.Name, Domain: row.Domain}, nil
}

// ---------------------------------------------------------------------------
// Error and row mapping
// ---------------------------------------------------------------------------

// PostgreSQL SQLSTATE codes worth distinguishing from a generic failure.
const (
	pgForeignKeyViolation = "23503"
	pgUniqueViolation     = "23505"
)

// translateWriteError turns the constraint violations this module can
// legitimately hit into domain errors. Everything else stays as-is and becomes a
// 500 at the handler.
//
// The constraint names are matched by substring because the exact names come
// from migration 000005; if that migration names them differently, this is the
// only place to adjust.
func translateWriteError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}

	name := strings.ToLower(pgErr.ConstraintName)
	switch pgErr.Code {
	case pgUniqueViolation:
		// unique(site_id, job_url) — this exact posting is already saved.
		return ErrDuplicateJobURL
	case pgForeignKeyViolation:
		switch {
		case strings.Contains(name, "cv_version"):
			return ErrCVVersionNotFound
		case strings.Contains(name, "site"):
			return ErrSiteNotFound
		case strings.Contains(name, "application"):
			// A history row or reminder pointing at an application that is gone.
			return ErrNotFound
		}
		return err
	default:
		return err
	}
}

func toDomain(row sqlcdb.Application) Application {
	return Application{
		ID:                 row.ID,
		CompanyName:        row.CompanyName,
		JobTitle:           row.JobTitle,
		JobDescription:     row.JobDescription,
		JobURL:             row.JobUrl,
		SiteID:             row.SiteID,
		CVVersionID:        row.CvVersionID,
		Status:             Status(row.Status),
		AIScore:            row.AiScore,
		AIScoreExplanation: row.AiScoreExplanation,
		AppliedAt:          row.AppliedAt,
		CreatedAt:          row.CreatedAt,
		UpdatedAt:          row.UpdatedAt,
	}
}

func toDomainSlice(rows []sqlcdb.Application) []Application {
	out := make([]Application, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomain(row))
	}
	return out
}

func toDomainHistory(row sqlcdb.ApplicationStatusHistory) StatusHistory {
	h := StatusHistory{
		ID:            row.ID,
		ApplicationID: row.ApplicationID,
		ToStatus:      Status(row.ToStatus),
		ChangedBy:     ChangeSource(row.ChangedBy),
		Note:          row.Note,
		ChangedAt:     row.ChangedAt,
	}
	if row.FromStatus != nil {
		from := Status(*row.FromStatus)
		h.FromStatus = &from
	}
	return h
}
