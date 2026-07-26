package coverletters

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	sqlcdb "github.com/EngenMe/applymind-backend/internal/db/sqlc"
)

// Repository is the persistence boundary for the coverletters module.
//
// Convention matches the cvs module: Get* returns ErrNotFound when the row is
// absent, because the caller asked for something specific. There is no Find*
// here — this module has no decision tree, so "no row" is never a normal
// branch, only "this application has no cover letter yet".
type Repository interface {
	Create(ctx context.Context, in NewCoverLetter) (*CoverLetter, error)
	GetByApplicationID(ctx context.Context, applicationID uuid.UUID) (*CoverLetter, error)
	// UpdateTextBody only touches rows with kind = 'text'; the check constraint
	// on cover_letters would reject a body_text on a file row anyway.
	UpdateTextBody(ctx context.Context, applicationID uuid.UUID, body string) (*CoverLetter, error)
	// DeleteByApplicationID is how a replace is performed. Deleting a row that
	// does not exist is not an error.
	DeleteByApplicationID(ctx context.Context, applicationID uuid.UUID) error
}

// postgresRepository is the sqlc-backed implementation.
//
// Per sqlc.yaml the generated row type uses google/uuid.UUID and time.Time
// directly, and emit_pointers_for_null_types makes body_text, s3_key and
// original_filename come out as *string — which is exactly the domain shape,
// so there is no conversion layer beyond the enum cast.
type postgresRepository struct {
	q *sqlcdb.Queries
}

func NewRepository(q *sqlcdb.Queries) Repository {
	return &postgresRepository{q: q}
}

// PostgreSQL SQLSTATE codes worth distinguishing from a generic failure.
const (
	pgForeignKeyViolation = "23503"
	pgUniqueViolation     = "23505"
)

func (r *postgresRepository) Create(ctx context.Context, in NewCoverLetter) (*CoverLetter, error) {
	row, err := r.q.CreateCoverLetter(
		ctx, sqlcdb.CreateCoverLetterParams{
			ID:               in.ID,
			ApplicationID:    in.ApplicationID,
			Kind:             sqlcdb.CoverLetterKind(in.Kind),
			BodyText:         in.BodyText,
			S3Key:            in.S3Key,
			OriginalFilename: in.OriginalFilename,
		},
	)
	if err != nil {
		return nil, translateWriteError(err)
	}
	cl := toDomain(row)
	return &cl, nil
}

func (r *postgresRepository) GetByApplicationID(ctx context.Context, applicationID uuid.UUID) (*CoverLetter, error) {
	row, err := r.q.GetCoverLetterByApplicationID(ctx, applicationID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	cl := toDomain(row)
	return &cl, nil
}

func (r *postgresRepository) UpdateTextBody(ctx context.Context, applicationID uuid.UUID, body string) (
	*CoverLetter,
	error,
) {
	row, err := r.q.UpdateCoverLetterText(
		ctx, sqlcdb.UpdateCoverLetterTextParams{
			ApplicationID: applicationID,
			BodyText:      &body,
		},
	)
	if err != nil {
		// The statement is scoped to kind = 'text', so no rows here means either
		// no cover letter or a file one. The service checks the kind before
		// calling, so ErrNotFound is the accurate reading of a race.
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	cl := toDomain(row)
	return &cl, nil
}

func (r *postgresRepository) DeleteByApplicationID(ctx context.Context, applicationID uuid.UUID) error {
	return r.q.DeleteCoverLetterByApplicationID(ctx, applicationID)
}

// ---------------------------------------------------------------------------
// Error and row mapping
// ---------------------------------------------------------------------------

// translateWriteError turns the two constraint violations this module can
// legitimately hit into domain errors. Everything else stays as-is and becomes
// a 500 at the handler.
//
// The applications module does not exist yet, so the foreign key is the only
// available check that the application is real — deliberately not a separate
// SELECT, which would race anyway.
func translateWriteError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case pgForeignKeyViolation:
		return ErrApplicationNotFound
	case pgUniqueViolation:
		return ErrAlreadyExists
	default:
		return err
	}
}

func toDomain(row sqlcdb.CoverLetter) CoverLetter {
	return CoverLetter{
		ID:               row.ID,
		ApplicationID:    row.ApplicationID,
		Kind:             Kind(row.Kind),
		BodyText:         row.BodyText,
		S3Key:            row.S3Key,
		OriginalFilename: row.OriginalFilename,
		CreatedAt:        row.CreatedAt,
		UpdatedAt:        row.UpdatedAt,
	}
}
