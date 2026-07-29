package settings

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	sqlcdb "github.com/EngenMe/applymind-backend/internal/db/sqlc"
)

// Repository is the persistence boundary for the settings module.
//
// Convention matches sites, cvs and coverletters: Get returns ErrNotFound when
// the row is absent. There is no Tx method — every operation here touches one
// row of one table — so NewRepository takes only the Queries.
type Repository interface {
	// Get reads the single settings row.
	Get(ctx context.Context) (*Settings, error)
	// SetProfileSummary upserts the summary and returns the row as it now
	// stands. It is an upsert rather than an update so a missing seed row heals
	// itself on the first write instead of failing forever.
	SetProfileSummary(ctx context.Context, summary string) (*Settings, error)
}

// postgresRepository is the sqlc-backed implementation.
//
// Per sqlc.yaml: timestamptz -> time.Time and emit_pointers_for_null_types gives
// *string for the nullable profile_summary column, so the generated row maps
// across field-for-field.
type postgresRepository struct {
	q *sqlcdb.Queries
}

func NewRepository(q *sqlcdb.Queries) Repository {
	return &postgresRepository{q: q}
}

func (r *postgresRepository) Get(ctx context.Context) (*Settings, error) {
	row, err := r.q.GetSettings(ctx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	s := toDomain(row)
	return &s, nil
}

func (r *postgresRepository) SetProfileSummary(ctx context.Context, summary string) (*Settings, error) {
	row, err := r.q.UpsertProfileSummary(ctx, &summary)
	if err != nil {
		// No constraint on this table can be violated by a validated string, so
		// there is nothing worth translating: anything that gets here is a real
		// database failure and becomes a 500 at the handler.
		return nil, err
	}
	s := toDomain(row)
	return &s, nil
}

// ---------------------------------------------------------------------------
// Row mapping
// ---------------------------------------------------------------------------

func toDomain(row sqlcdb.Setting) Settings {
	return Settings{
		ProfileSummary: row.ProfileSummary,
		CreatedAt:      row.CreatedAt,
		UpdatedAt:      row.UpdatedAt,
	}
}
