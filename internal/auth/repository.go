package auth

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	sqlcdb "github.com/EngenMe/applymind-backend/internal/db/sqlc"
)

// Repository is the persistence boundary for the auth module.
//
// Same convention as cvs and applications: Get* returns a not-found error
// because the caller asked for something specific; Find* returns (nil, nil)
// because "no match" is a normal answer. Every Find* here is on the
// authentication path, where a missing row is the expected outcome for anyone
// presenting a bad credential, not an error worth propagating.
type Repository interface {
	CreateUser(ctx context.Context, in NewUser) (*User, error)
	GetUser(ctx context.Context, id uuid.UUID) (*User, error)
	// FindUserByEmail returns (nil, nil) when no account exists. Login depends
	// on this specifically: it must be able to run its dummy bcrypt comparison
	// without an error path that behaves differently.
	FindUserByEmail(ctx context.Context, email string) (*User, error)
	UpdateUserPassword(ctx context.Context, id uuid.UUID, hash string) (*User, error)

	CreateSession(ctx context.Context, in NewSession) (*Session, error)
	// FindLiveSession filters expired and revoked rows in SQL, so a stale
	// session cannot be authenticated by a caller that forgot to check.
	FindLiveSession(ctx context.Context, tokenHash string) (*Session, error)
	ExtendSession(ctx context.Context, id uuid.UUID, expiresAt time.Time) error
	RevokeSession(ctx context.Context, id, userID uuid.UUID) error
	RevokeSessionByTokenHash(ctx context.Context, tokenHash string) error
	RevokeAllUserSessions(ctx context.Context, userID uuid.UUID) error
	ListActiveSessions(ctx context.Context, userID uuid.UUID) ([]Session, error)

	CreateAPIToken(ctx context.Context, in NewAPIToken) (*APIToken, error)
	FindLiveAPIToken(ctx context.Context, tokenHash string) (*APIToken, error)
	// TouchAPIToken records last_used_at. Errors are the caller's to ignore.
	TouchAPIToken(ctx context.Context, id uuid.UUID) error
	RevokeAPIToken(ctx context.Context, id, userID uuid.UUID) error
	ListActiveAPITokens(ctx context.Context, userID uuid.UUID) ([]APIToken, error)
}

// NewUser is the repository-level input for inserting a user row. Email arrives
// already normalised and the password already hashed — this layer does neither.
type NewUser struct {
	Email           string
	PasswordHash    string
	DisplayName     *string
	EmailVerifiedAt *time.Time
}

type NewSession struct {
	UserID    uuid.UUID
	TokenHash string
	ExpiresAt time.Time
	UserAgent *string
	IPAddress *netip.Addr
}

type NewAPIToken struct {
	UserID     uuid.UUID
	TokenHash  string
	Name       string
	IsReadOnly bool
}

type postgresRepository struct {
	q *sqlcdb.Queries
}

func NewRepository(q *sqlcdb.Queries) Repository {
	return &postgresRepository{q: q}
}

// ---------------------------------------------------------------------------
// Users
// ---------------------------------------------------------------------------

func (r *postgresRepository) CreateUser(ctx context.Context, in NewUser) (*User, error) {
	row, err := r.q.CreateUser(
		ctx, sqlcdb.CreateUserParams{
			Email:           in.Email,
			PasswordHash:    in.PasswordHash,
			DisplayName:     in.DisplayName,
			EmailVerifiedAt: in.EmailVerifiedAt,
		},
	)
	if err != nil {
		return nil, translateWriteError(err)
	}
	user := toDomainUser(row)
	return &user, nil
}

func (r *postgresRepository) GetUser(ctx context.Context, id uuid.UUID) (*User, error) {
	row, err := r.q.GetUser(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	user := toDomainUser(row)
	return &user, nil
}

func (r *postgresRepository) FindUserByEmail(ctx context.Context, email string) (*User, error) {
	row, err := r.q.GetUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	user := toDomainUser(row)
	return &user, nil
}

func (r *postgresRepository) UpdateUserPassword(ctx context.Context, id uuid.UUID, hash string) (*User, error) {
	row, err := r.q.UpdateUserPassword(
		ctx, sqlcdb.UpdateUserPasswordParams{
			ID:           id,
			PasswordHash: hash,
		},
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	user := toDomainUser(row)
	return &user, nil
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

func (r *postgresRepository) CreateSession(ctx context.Context, in NewSession) (*Session, error) {
	row, err := r.q.CreateSession(
		ctx, sqlcdb.CreateSessionParams{
			UserID:    in.UserID,
			TokenHash: in.TokenHash,
			ExpiresAt: in.ExpiresAt,
			UserAgent: in.UserAgent,
			IpAddress: in.IPAddress,
		},
	)
	if err != nil {
		return nil, translateWriteError(err)
	}
	session := toDomainSession(row)
	return &session, nil
}

func (r *postgresRepository) FindLiveSession(ctx context.Context, tokenHash string) (*Session, error) {
	row, err := r.q.FindLiveSessionByTokenHash(ctx, tokenHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	session := toDomainSession(row)
	return &session, nil
}

func (r *postgresRepository) ExtendSession(ctx context.Context, id uuid.UUID, expiresAt time.Time) error {
	_, err := r.q.ExtendSession(
		ctx, sqlcdb.ExtendSessionParams{
			ID:        id,
			ExpiresAt: expiresAt,
		},
	)
	// The session was revoked between the lookup and this write. Nothing to
	// extend, and nothing worth reporting: the next request will fail its
	// lookup, which is the correct outcome.
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

func (r *postgresRepository) RevokeSession(ctx context.Context, id, userID uuid.UUID) error {
	affected, err := r.q.RevokeSession(
		ctx, sqlcdb.RevokeSessionParams{
			ID:     id,
			UserID: userID,
		},
	)
	if err != nil {
		return err
	}
	// Zero rows means the session does not exist, is already revoked, or
	// belongs to somebody else. All three are reported the same way, because
	// distinguishing them would confirm the existence of another user's session.
	if affected == 0 {
		return ErrSessionNotFound
	}
	return nil
}

func (r *postgresRepository) RevokeSessionByTokenHash(ctx context.Context, tokenHash string) error {
	// Logging out a session that is already gone is a success, not an error —
	// the caller wanted it not to be usable, and it is not.
	_, err := r.q.RevokeSessionByTokenHash(ctx, tokenHash)
	return err
}

func (r *postgresRepository) RevokeAllUserSessions(ctx context.Context, userID uuid.UUID) error {
	_, err := r.q.RevokeAllUserSessions(ctx, userID)
	return err
}

func (r *postgresRepository) ListActiveSessions(ctx context.Context, userID uuid.UUID) ([]Session, error) {
	rows, err := r.q.ListActiveSessions(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]Session, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainSession(row))
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// API tokens
// ---------------------------------------------------------------------------

func (r *postgresRepository) CreateAPIToken(ctx context.Context, in NewAPIToken) (*APIToken, error) {
	row, err := r.q.CreateAPIToken(
		ctx, sqlcdb.CreateAPITokenParams{
			UserID:     in.UserID,
			TokenHash:  in.TokenHash,
			Name:       in.Name,
			IsReadOnly: in.IsReadOnly,
		},
	)
	if err != nil {
		return nil, translateWriteError(err)
	}
	token := toDomainAPIToken(row)
	return &token, nil
}

func (r *postgresRepository) FindLiveAPIToken(ctx context.Context, tokenHash string) (*APIToken, error) {
	row, err := r.q.FindLiveAPITokenByTokenHash(ctx, tokenHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	token := toDomainAPIToken(row)
	return &token, nil
}

func (r *postgresRepository) TouchAPIToken(ctx context.Context, id uuid.UUID) error {
	return r.q.TouchAPIToken(ctx, id)
}

func (r *postgresRepository) RevokeAPIToken(ctx context.Context, id, userID uuid.UUID) error {
	affected, err := r.q.RevokeAPIToken(
		ctx, sqlcdb.RevokeAPITokenParams{
			ID:     id,
			UserID: userID,
		},
	)
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrTokenNotFound
	}
	return nil
}

func (r *postgresRepository) ListActiveAPITokens(ctx context.Context, userID uuid.UUID) ([]APIToken, error) {
	rows, err := r.q.ListActiveAPITokens(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]APIToken, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainAPIToken(row))
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Error and row mapping
// ---------------------------------------------------------------------------

const pgUniqueViolation = "23505"

// translateWriteError turns the one constraint violation this module can
// legitimately hit into a domain error.
//
// users_email_key is the only unique constraint a caller can trip: the token
// hashes are 32 random bytes, and a collision there would be a far more
// interesting problem than a duplicate account.
func translateWriteError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	if pgErr.Code == pgUniqueViolation && strings.Contains(strings.ToLower(pgErr.ConstraintName), "email") {
		return ErrEmailTaken
	}
	return err
}

func toDomainUser(row sqlcdb.User) User {
	return User{
		ID:              row.ID,
		Email:           row.Email,
		PasswordHash:    row.PasswordHash,
		DisplayName:     row.DisplayName,
		EmailVerifiedAt: row.EmailVerifiedAt,
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
	}
}

func toDomainSession(row sqlcdb.Session) Session {
	return Session{
		ID:        row.ID,
		UserID:    row.UserID,
		TokenHash: row.TokenHash,
		ExpiresAt: row.ExpiresAt,
		RevokedAt: row.RevokedAt,
		UserAgent: row.UserAgent,
		IPAddress: row.IpAddress,
		CreatedAt: row.CreatedAt,
	}
}

func toDomainAPIToken(row sqlcdb.ApiToken) APIToken {
	return APIToken{
		ID:         row.ID,
		UserID:     row.UserID,
		IsReadOnly: row.IsReadOnly,
		TokenHash:  row.TokenHash,
		Name:       row.Name,
		LastUsedAt: row.LastUsedAt,
		RevokedAt:  row.RevokedAt,
		CreatedAt:  row.CreatedAt,
	}
}
