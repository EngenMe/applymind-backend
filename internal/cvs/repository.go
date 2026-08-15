package cvs

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	sqlcdb "github.com/EngenMe/applymind-backend/internal/db/sqlc"
)

// Repository is the persistence boundary for the cvs module.
//
// Convention used throughout: Get* returns ErrCVNotFound / ErrVersionNotFound
// when the row is absent, because the caller asked for something specific.
// Find* returns (nil, nil) when nothing matches, because "no match" is a normal
// branch of the Flow 3 decision tree rather than an error.
//
// Phase 15: every method takes a userID and every query is scoped by it. A
// version another user owns is not merely off-limits to serve — under Get* it
// must read as "not found", not "found, but not yours", because the second
// answer confirms the row exists to someone who should not be able to tell.
type Repository interface {
	CreateCV(ctx context.Context, userID uuid.UUID, name string, tag *string) (*CV, error)
	GetCV(ctx context.Context, userID, id uuid.UUID) (*CV, error)
	GetCVByName(ctx context.Context, userID uuid.UUID, name string) (*CV, error)
	ListCVs(ctx context.Context, userID uuid.UUID) ([]CV, error)
	DeleteCV(ctx context.Context, userID, id uuid.UUID) error

	CreateVersion(ctx context.Context, in NewVersion) (*CVVersion, error)
	GetVersion(ctx context.Context, userID, id uuid.UUID) (*CVVersion, error)
	ListVersionsForCV(ctx context.Context, userID, cvID uuid.UUID) ([]CVVersion, error)
	ListAllVersions(ctx context.Context, userID uuid.UUID) ([]CVVersion, error)

	// FindVersionByHash implements Flow 3, Decision 2.
	FindVersionByHash(ctx context.Context, userID uuid.UUID, hash string) (*CVVersion, error)
	// FindLatestVersionByFilename implements Flow 3, Decision 3.
	FindLatestVersionByFilename(ctx context.Context, userID uuid.UUID, filename string) (*CVVersion, error)
	// FindVersionByCVAndSize implements Flow 3, Decision 4.
	FindVersionByCVAndSize(ctx context.Context, userID, cvID uuid.UUID, size int64) (*CVVersion, error)
	// FindVersionByCVAndHash guards the unique(user_id, cv_id, sha256_hash) constraint.
	FindVersionByCVAndHash(ctx context.Context, userID, cvID uuid.UUID, hash string) (*CVVersion, error)

	ListApplicationsUsingVersion(ctx context.Context, userID, versionID uuid.UUID) ([]ApplicationUsage, error)
	// LastUsageForCV returns the most recent application that used any version of
	// this CV, or (nil, nil) if it has never been used.
	LastUsageForCV(ctx context.Context, userID, cvID uuid.UUID) (*ApplicationUsage, error)
}

// postgresRepository is the sqlc-backed implementation.
//
// Per sqlc.yaml: uuid -> github.com/google/uuid.UUID directly, timestamptz ->
// time.Time directly (pointer when nullable), and emit_pointers_for_null_types
// means nullable text columns already come out as *string. There is no
// pgtype.UUID / pgtype.Text / pgtype.Timestamptz anywhere in the generated
// code for this project, so no conversion layer is needed here — the
// generated row types are used directly.
type postgresRepository struct {
	q *sqlcdb.Queries
}

func NewRepository(q *sqlcdb.Queries) Repository {
	return &postgresRepository{q: q}
}

func (r *postgresRepository) CreateCV(ctx context.Context, userID uuid.UUID, name string, tag *string) (*CV, error) {
	row, err := r.q.CreateCV(
		ctx, sqlcdb.CreateCVParams{
			UserID: userID,
			Name:   name,
			Tag:    tag,
		},
	)
	if err != nil {
		return nil, err
	}
	cv := toDomainCV(row)
	return &cv, nil
}

func (r *postgresRepository) GetCV(ctx context.Context, userID, id uuid.UUID) (*CV, error) {
	row, err := r.q.GetCV(ctx, sqlcdb.GetCVParams{ID: id, UserID: userID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrCVNotFound
		}
		return nil, err
	}
	cv := toDomainCV(row)
	return &cv, nil
}

func (r *postgresRepository) GetCVByName(ctx context.Context, userID uuid.UUID, name string) (*CV, error) {
	row, err := r.q.GetCVByName(ctx, sqlcdb.GetCVByNameParams{UserID: userID, Name: name})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrCVNotFound
		}
		return nil, err
	}
	cv := toDomainCV(row)
	return &cv, nil
}

func (r *postgresRepository) ListCVs(ctx context.Context, userID uuid.UUID) ([]CV, error) {
	rows, err := r.q.ListCVs(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]CV, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainCV(row))
	}
	return out, nil
}

func (r *postgresRepository) DeleteCV(ctx context.Context, userID, id uuid.UUID) error {
	return r.q.DeleteCV(ctx, sqlcdb.DeleteCVParams{ID: id, UserID: userID})
}

func (r *postgresRepository) CreateVersion(ctx context.Context, in NewVersion) (*CVVersion, error) {
	row, err := r.q.CreateCVVersion(
		ctx, sqlcdb.CreateCVVersionParams{
			ID:               in.ID,
			UserID:           in.UserID,
			CvID:             in.CVID,
			Sha256Hash:       in.SHA256Hash,
			FileSizeBytes:    in.FileSizeBytes,
			OriginalFilename: in.OriginalFilename,
			S3Key:            in.S3Key,
		},
	)
	if err != nil {
		return nil, err
	}
	v := toDomainVersion(row)
	return &v, nil
}

func (r *postgresRepository) GetVersion(ctx context.Context, userID, id uuid.UUID) (*CVVersion, error) {
	row, err := r.q.GetCVVersion(ctx, sqlcdb.GetCVVersionParams{ID: id, UserID: userID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrVersionNotFound
		}
		return nil, err
	}
	v := toDomainVersion(row)
	return &v, nil
}

func (r *postgresRepository) ListVersionsForCV(ctx context.Context, userID, cvID uuid.UUID) ([]CVVersion, error) {
	rows, err := r.q.ListVersionsForCV(ctx, sqlcdb.ListVersionsForCVParams{CvID: cvID, UserID: userID})
	if err != nil {
		return nil, err
	}
	return toDomainVersions(rows), nil
}

func (r *postgresRepository) ListAllVersions(ctx context.Context, userID uuid.UUID) ([]CVVersion, error) {
	rows, err := r.q.ListAllCVVersions(ctx, userID)
	if err != nil {
		return nil, err
	}
	return toDomainVersions(rows), nil
}

func (r *postgresRepository) FindVersionByHash(ctx context.Context, userID uuid.UUID, hash string) (
	*CVVersion,
	error,
) {
	row, err := r.q.FindCVVersionByHash(ctx, sqlcdb.FindCVVersionByHashParams{UserID: userID, Sha256Hash: hash})
	return firstVersionOrNil(row, err)
}

func (r *postgresRepository) FindLatestVersionByFilename(ctx context.Context, userID uuid.UUID, filename string) (
	*CVVersion,
	error,
) {
	row, err := r.q.FindLatestCVVersionByFilename(
		ctx, sqlcdb.FindLatestCVVersionByFilenameParams{
			UserID:           userID,
			OriginalFilename: filename,
		},
	)
	return firstVersionOrNil(row, err)
}

func (r *postgresRepository) FindVersionByCVAndSize(ctx context.Context, userID, cvID uuid.UUID, size int64) (
	*CVVersion,
	error,
) {
	row, err := r.q.FindCVVersionByCVAndSize(
		ctx, sqlcdb.FindCVVersionByCVAndSizeParams{
			CvID:          cvID,
			UserID:        userID,
			FileSizeBytes: size,
		},
	)
	return firstVersionOrNil(row, err)
}

func (r *postgresRepository) FindVersionByCVAndHash(ctx context.Context, userID, cvID uuid.UUID, hash string) (
	*CVVersion,
	error,
) {
	row, err := r.q.FindCVVersionByCVAndHash(
		ctx, sqlcdb.FindCVVersionByCVAndHashParams{
			CvID:       cvID,
			UserID:     userID,
			Sha256Hash: hash,
		},
	)
	return firstVersionOrNil(row, err)
}

func (r *postgresRepository) ListApplicationsUsingVersion(ctx context.Context, userID, versionID uuid.UUID) (
	[]ApplicationUsage,
	error,
) {
	rows, err := r.q.ListApplicationsUsingCVVersion(
		ctx, sqlcdb.ListApplicationsUsingCVVersionParams{
			CvVersionID: &versionID,
			UserID:      userID,
		},
	)
	if err != nil {
		return nil, err
	}
	out := make([]ApplicationUsage, 0, len(rows))
	for _, row := range rows {
		out = append(
			out, ApplicationUsage{
				ApplicationID: row.ID,
				CompanyName:   row.CompanyName,
				JobTitle:      row.JobTitle,
				Status:        row.Status,
				AppliedAt:     row.AppliedAt,
			},
		)
	}
	return out, nil
}

func (r *postgresRepository) LastUsageForCV(ctx context.Context, userID, cvID uuid.UUID) (*ApplicationUsage, error) {
	row, err := r.q.GetLastCVUsage(ctx, sqlcdb.GetLastCVUsageParams{CvID: cvID, UserID: userID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &ApplicationUsage{
		CompanyName: row.CompanyName,
		AppliedAt:   row.AppliedAt,
	}, nil
}

// ---------------------------------------------------------------------------
// sqlc row -> domain mapping. Field names below assume sqlc's default naming
// for a table called "cvs" (struct Cv) and "cv_versions" (struct CvVersion).
// If sqlc generated different struct/field names, only this section needs
// adjusting — the interface and its callers stay the same.
// ---------------------------------------------------------------------------

func firstVersionOrNil(row sqlcdb.CvVersion, err error) (*CVVersion, error) {
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	v := toDomainVersion(row)
	return &v, nil
}

func toDomainCV(row sqlcdb.Cv) CV {
	return CV{
		ID:        row.ID,
		Name:      row.Name,
		Tag:       row.Tag,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}
}

func toDomainVersion(row sqlcdb.CvVersion) CVVersion {
	return CVVersion{
		ID:               row.ID,
		CVID:             row.CvID,
		SHA256Hash:       row.Sha256Hash,
		FileSizeBytes:    row.FileSizeBytes,
		OriginalFilename: row.OriginalFilename,
		S3Key:            row.S3Key,
		UploadedAt:       row.UploadedAt,
	}
}

func toDomainVersions(rows []sqlcdb.CvVersion) []CVVersion {
	out := make([]CVVersion, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainVersion(row))
	}
	return out
}
