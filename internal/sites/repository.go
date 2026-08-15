package sites

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	sqlcdb "github.com/EngenMe/applymind-backend/internal/db/sqlc"
)

// Repository is the persistence boundary for the sites module.
//
// Convention matches applications, cvs and coverletters: Get* returns
// ErrNotFound when the row is absent, because the caller asked for something
// specific. Ensure is the one exception — it returns (nil, nil) when there was
// nothing to do, which is the normal outcome of seeding a table that has already
// been seeded.
//
// Phase 15: every read takes a userID and matches a global row (user_id IS
// NULL — the pre-configured list) or one this user owns. Ensure is unaffected:
// it only ever writes global rows, at boot, before any request exists.
//
// There is no Tx method: every operation here writes at most one row, so this
// module never needs a transaction of its own and NewRepository takes only the
// Queries, like cvs and coverletters.
type Repository interface {
	Create(ctx context.Context, in NewSite) (*Site, error)
	// Ensure inserts a site unless one is already registered for that domain,
	// and returns (nil, nil) when it was already there. Idempotent, so seeding
	// can run on every boot without a first-run flag.
	Ensure(ctx context.Context, in NewSite) (*Site, error)
	List(ctx context.Context, userID uuid.UUID, f ListFilter) ([]Site, error)
	Get(ctx context.Context, userID, id uuid.UUID) (*Site, error)
	// GetByDomain looks up an already-normalised host, global or this user's
	// own. It ignores is_active: an inactive site still exists, and the caller
	// decides whether that matters.
	GetByDomain(ctx context.Context, userID uuid.UUID, domain string) (*Site, error)
	SetActive(ctx context.Context, userID, id uuid.UUID, active bool) (*Site, error)
	// Delete removes a site owned by userID. It returns ErrInUse when
	// applications still reference it — the foreign key is ON DELETE RESTRICT
	// per the ERD.
	Delete(ctx context.Context, userID, id uuid.UUID) error
}

// postgresRepository is the sqlc-backed implementation.
//
// Per sqlc.yaml: uuid -> google/uuid.UUID and timestamptz -> time.Time, so the
// generated row maps across field-for-field with no conversion beyond selectors.
type postgresRepository struct {
	q *sqlcdb.Queries
}

func NewRepository(q *sqlcdb.Queries) Repository {
	return &postgresRepository{q: q}
}

func (r *postgresRepository) Create(ctx context.Context, in NewSite) (*Site, error) {
	row, err := r.q.CreateSite(
		ctx, sqlcdb.CreateSiteParams{
			UserID:          in.UserID,
			Name:            in.Name,
			Domain:          in.Domain,
			IsPreconfigured: in.IsPreconfigured,
			IsActive:        in.IsActive,
		},
	)
	if err != nil {
		return nil, translateWriteError(err)
	}
	site := toDomain(row)
	return &site, nil
}

func (r *postgresRepository) Ensure(ctx context.Context, in NewSite) (*Site, error) {
	row, err := r.q.EnsureSite(
		ctx, sqlcdb.EnsureSiteParams{
			Name:            in.Name,
			Domain:          in.Domain,
			IsPreconfigured: in.IsPreconfigured,
			IsActive:        in.IsActive,
		},
	)
	if err != nil {
		// ON CONFLICT (domain) DO NOTHING returns no row when the domain is
		// already registered, which is the intended outcome, not a failure.
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		// name is UNIQUE too, and the ON CONFLICT clause does not cover it. A
		// user who added a custom site called "Indeed" would otherwise break
		// startup, so a name collision is also treated as "already present".
		if errors.Is(translateWriteError(err), ErrDuplicate) {
			return nil, nil
		}
		return nil, err
	}
	site := toDomain(row)
	return &site, nil
}

func (r *postgresRepository) List(ctx context.Context, userID uuid.UUID, f ListFilter) ([]Site, error) {
	if f.ActiveOnly {
		rows, err := r.q.ListActiveSites(ctx, userID)
		if err != nil {
			return nil, err
		}
		return toDomainSlice(rows), nil
	}

	rows, err := r.q.ListSites(ctx, userID)
	if err != nil {
		return nil, err
	}
	return toDomainSlice(rows), nil
}

func (r *postgresRepository) Get(ctx context.Context, userID, id uuid.UUID) (*Site, error) {
	row, err := r.q.GetSite(ctx, sqlcdb.GetSiteParams{ID: id, UserID: userID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	site := toDomain(row)
	return &site, nil
}

func (r *postgresRepository) GetByDomain(ctx context.Context, userID uuid.UUID, domain string) (*Site, error) {
	row, err := r.q.GetSiteByDomain(ctx, sqlcdb.GetSiteByDomainParams{Domain: domain, UserID: userID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	site := toDomain(row)
	return &site, nil
}

func (r *postgresRepository) SetActive(ctx context.Context, userID, id uuid.UUID, active bool) (*Site, error) {
	row, err := r.q.SetSiteActive(
		ctx, sqlcdb.SetSiteActiveParams{
			ID:       id,
			IsActive: active,
			UserID:   userID,
		},
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, translateWriteError(err)
	}
	site := toDomain(row)
	return &site, nil
}

func (r *postgresRepository) Delete(ctx context.Context, userID, id uuid.UUID) error {
	affected, err := r.q.DeleteSite(ctx, sqlcdb.DeleteSiteParams{ID: id, UserID: userID})
	if err != nil {
		return translateWriteError(err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// Error and row mapping
// ---------------------------------------------------------------------------

// PostgreSQL SQLSTATE codes worth distinguishing from a generic failure.
const (
	pgForeignKeyViolation = "23503"
	pgUniqueViolation     = "23505"
)

// translateWriteError turns the two constraint violations this module can
// legitimately hit into domain errors. Everything else stays as-is and becomes a
// 500 at the handler.
//
// The only foreign key pointing at sites is applications.site_id, so a 23503 on
// this table can only mean the site is still in use.
func translateWriteError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}

	switch pgErr.Code {
	case pgUniqueViolation:
		return ErrDuplicate
	case pgForeignKeyViolation:
		return ErrInUse
	default:
		return err
	}
}

func toDomain(row sqlcdb.Site) Site {
	return Site{
		ID:              row.ID,
		Name:            row.Name,
		Domain:          row.Domain,
		IsPreconfigured: row.IsPreconfigured,
		IsActive:        row.IsActive,
		Selectors:       json.RawMessage(row.Selectors),
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
	}
}

func toDomainSlice(rows []sqlcdb.Site) []Site {
	out := make([]Site, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomain(row))
	}
	return out
}
