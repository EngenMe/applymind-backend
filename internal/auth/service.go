package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// Service is the business logic boundary for the auth module.
type Service interface {
	Register(ctx context.Context, in RegisterInput) (*AuthResult, error)
	Login(ctx context.Context, in LoginInput) (*AuthResult, error)
	// Logout revokes the session behind a raw token. Logging out an already
	// dead session succeeds: the caller wanted it unusable, and it is.
	Logout(ctx context.Context, sessionToken string) error

	// AuthenticateSession resolves a raw session token to its user, extending
	// the session's life if it is due. This is the dashboard's hot path.
	//
	// Returns an Identity rather than a User because the caller — the
	// middleware — needs more than who this is. A session is never read-only,
	// but the middleware cannot know that without being told which kind of
	// credential it resolved, and a single return type for both paths keeps
	// that decision in one place.
	AuthenticateSession(ctx context.Context, sessionToken string) (*Identity, error)
	// AuthenticateAPIToken resolves a raw API token to its user. This is the
	// extension's path, and the demo dashboard's.
	AuthenticateAPIToken(ctx context.Context, apiToken string) (*Identity, error)

	GetUser(ctx context.Context, id uuid.UUID) (*User, error)

	ListSessions(ctx context.Context, userID uuid.UUID) ([]Session, error)
	RevokeSession(ctx context.Context, userID, sessionID uuid.UUID) error

	// IssueAPIToken mints a token. readOnly tokens may only be used for safe
	// HTTP methods — the middleware enforces that, not this module.
	IssueAPIToken(ctx context.Context, userID uuid.UUID, name string, readOnly bool) (*IssuedToken, error)
	ListAPITokens(ctx context.Context, userID uuid.UUID) ([]APIToken, error)
	RevokeAPIToken(ctx context.Context, userID, tokenID uuid.UUID) error
}

const (
	// DefaultSessionTTL is how long a session lives without being used.
	DefaultSessionTTL = 30 * 24 * time.Hour

	// DefaultExtendAfter is how much of a session's life must have elapsed
	// before using it is worth a write to extend it.
	//
	// Without this the sliding window means a database write on every single
	// authenticated request, which for an active dashboard is a write per page
	// load to move an expiry that is already a month away. A day's granularity
	// costs nothing: the only visible difference is that a session used once at
	// 09:00 and again at 09:05 expires a few minutes earlier than it strictly
	// could.
	DefaultExtendAfter = 24 * time.Hour

	// BcryptCost of 12 is roughly 250ms on current hardware. High enough to
	// make offline cracking expensive, low enough that a Lambda cold start plus
	// a login does not time out.
	BcryptCost = 12

	// MinPasswordLength is 12 with no composition rules. Length is what
	// actually resists guessing; requiring a symbol and a digit reliably
	// produces "Password1!" and a user who has written it on something.
	MinPasswordLength = 12

	// MaxPasswordLength is bcrypt's limit, not a policy of ours: the algorithm
	// reads at most 72 bytes and the rest of the input has no effect on the
	// hash. Checked here rather than left to bcrypt because the two things it
	// does at that boundary — return ErrPasswordTooLong on current x/crypto,
	// truncate silently on older ones — are both wrong to pass through. The
	// first becomes a 500 for a user who did what the minimum encouraged; the
	// second means "hunter2...<72 bytes>...anything" and the same prefix with a
	// different tail are the same credential.
	//
	// Bytes, not runes, because that is the unit bcrypt counts: a 30-character
	// passphrase in a non-Latin script can exceed this.
	MaxPasswordLength = 72

	// tokenBytes is the entropy behind every session and API token. 32 bytes
	// is 256 bits — not guessable, and not worth making configurable.
	tokenBytes = 32
)

// dummyHash is a real bcrypt hash of a value nobody knows, used to burn the
// same ~250ms on a login for an email that does not exist as one that does.
//
// Without it, "no such account" returns in microseconds while "wrong password"
// takes a quarter of a second, and the difference is measurable over the
// network. That turns the login endpoint into a way to test whether an address
// has an account here — which for a job-application tracker is genuinely
// sensitive: it reveals that someone is job hunting.
var dummyHash = []byte("$2a$12$C6UzMDM.H6dfI/f/IKcEe.7YJZjkS0dEEz.OMBtLDMOFcnwYQpF3S")

type service struct {
	repo         Repository
	sessionTTL   time.Duration
	extendAfter  time.Duration
	bcryptCost   int
	now          func() time.Time
	newTokenFunc func() (raw string, hash string, err error)
}

type ServiceOption func(*service)

func WithSessionTTL(d time.Duration) ServiceOption {
	return func(s *service) { s.sessionTTL = d }
}

func WithExtendAfter(d time.Duration) ServiceOption {
	return func(s *service) { s.extendAfter = d }
}

// WithBcryptCost lowers the work factor. Intended for tests, where the default
// cost turns a table of twenty cases into a five-second run.
func WithBcryptCost(cost int) ServiceOption {
	return func(s *service) { s.bcryptCost = cost }
}

func WithClock(f func() time.Time) ServiceOption {
	return func(s *service) { s.now = f }
}

// WithTokenGenerator makes token values deterministic for tests. Production
// never sets this.
func WithTokenGenerator(f func() (string, string, error)) ServiceOption {
	return func(s *service) { s.newTokenFunc = f }
}

func NewService(repo Repository, opts ...ServiceOption) Service {
	s := &service{
		repo:         repo,
		sessionTTL:   DefaultSessionTTL,
		extendAfter:  DefaultExtendAfter,
		bcryptCost:   BcryptCost,
		now:          time.Now,
		newTokenFunc: generateToken,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// ---------------------------------------------------------------------------
// Registration and login
// ---------------------------------------------------------------------------

func (s *service) Register(ctx context.Context, in RegisterInput) (*AuthResult, error) {
	email, err := normaliseEmail(in.Email)
	if err != nil {
		return nil, err
	}
	if len([]rune(in.Password)) < MinPasswordLength {
		return nil, ErrPasswordTooShort
	}
	if len(in.Password) > MaxPasswordLength {
		return nil, ErrPasswordTooLong
	}

	// Checked here for a clear error, and enforced by the unique constraint
	// underneath because this check races: two simultaneous registrations for
	// the same address both see nothing. The constraint is what actually
	// guarantees it; translateWriteError turns that into the same error.
	existing, err := s.repo.FindUserByEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, ErrEmailTaken
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), s.bcryptCost)
	if err != nil {
		// The length check above should have caught this. Mapped anyway rather
		// than wrapped, so that if the two ever disagree — a different bcrypt
		// implementation, a changed limit — the caller still gets a 400 that
		// names the problem instead of a 500 that does not.
		if errors.Is(err, bcrypt.ErrPasswordTooLong) {
			return nil, ErrPasswordTooLong
		}
		return nil, fmt.Errorf("auth: hash password: %w", err)
	}

	// Set at registration for now. Verification is a later phase; recording the
	// timestamp rather than leaving it null means that when verification does
	// arrive, existing accounts do not all become unverified overnight.
	verifiedAt := s.now().UTC()

	user, err := s.repo.CreateUser(
		ctx, NewUser{
			Email:           email,
			PasswordHash:    string(hash),
			DisplayName:     trimmedOrNil(in.DisplayName),
			EmailVerifiedAt: &verifiedAt,
		},
	)
	if err != nil {
		return nil, err
	}

	return s.startSession(ctx, user, in.UserAgent, in.IPAddress)
}

// Login verifies a password and starts a session.
//
// The two failure modes — no such account, wrong password — return the same
// error and take the same time. See dummyHash above for why that matters here
// specifically.
func (s *service) Login(ctx context.Context, in LoginInput) (*AuthResult, error) {
	email, err := normaliseEmail(in.Email)
	if err != nil {
		// Not ErrEmailInvalid: a malformed address is still just a failed
		// login, and saying otherwise tells the caller their input reached
		// further into the system than a wrong password would.
		return nil, ErrInvalidCredentials
	}

	user, err := s.repo.FindUserByEmail(ctx, email)
	if err != nil {
		return nil, err
	}

	if user == nil {
		// Deliberately still hashing. The result is discarded; the elapsed time
		// is the point.
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(in.Password))
		return nil, ErrInvalidCredentials
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(in.Password)); err != nil {
		return nil, ErrInvalidCredentials
	}

	return s.startSession(ctx, user, in.UserAgent, in.IPAddress)
}

func (s *service) Logout(ctx context.Context, sessionToken string) error {
	if strings.TrimSpace(sessionToken) == "" {
		return nil
	}
	return s.repo.RevokeSessionByTokenHash(ctx, hashToken(sessionToken))
}

func (s *service) startSession(
	ctx context.Context,
	user *User,
	userAgent *string,
	ip *netip.Addr,
) (*AuthResult, error) {
	raw, hash, err := s.newTokenFunc()
	if err != nil {
		return nil, err
	}

	session, err := s.repo.CreateSession(
		ctx, NewSession{
			UserID:    user.ID,
			TokenHash: hash,
			ExpiresAt: s.now().UTC().Add(s.sessionTTL),
			UserAgent: trimmedOrNil(userAgent),
			IPAddress: ip,
		},
	)
	if err != nil {
		return nil, err
	}

	return &AuthResult{User: user, Session: session, SessionToken: raw}, nil
}

// ---------------------------------------------------------------------------
// Authentication
// ---------------------------------------------------------------------------

// AuthenticateSession is called on every authenticated dashboard request.
//
// The expiry extension is best-effort on purpose: a failure to slide the window
// forward is not a reason to reject a session that is currently valid. The
// worst case is that it expires on schedule instead of later.
func (s *service) AuthenticateSession(ctx context.Context, sessionToken string) (*Identity, error) {
	if strings.TrimSpace(sessionToken) == "" {
		return nil, ErrSessionInvalid
	}

	session, err := s.repo.FindLiveSession(ctx, hashToken(sessionToken))
	if err != nil {
		return nil, err
	}
	if session == nil {
		return nil, ErrSessionInvalid
	}

	now := s.now().UTC()
	// How much life is left. When less than (TTL - extendAfter) remains, at
	// least extendAfter has passed since the last extension, so it is due.
	if session.ExpiresAt.Sub(now) < s.sessionTTL-s.extendAfter {
		_ = s.repo.ExtendSession(ctx, session.ID, now.Add(s.sessionTTL))
	}

	user, err := s.repo.GetUser(ctx, session.UserID)
	if err != nil {
		// The user was deleted while the session lived. The cascade should have
		// taken the session with it, so this is a surprise — but the caller's
		// answer is the same either way.
		if errors.Is(err, ErrUserNotFound) {
			return nil, ErrSessionInvalid
		}
		return nil, err
	}

	// A session is never read-only. Signing in through the dashboard is the
	// full-access path; the restricted one is a token, deliberately, because a
	// credential a human types should not silently do less than they expect.
	return &Identity{
		UserID:     user.ID,
		Kind:       CredentialSession,
		IsReadOnly: false,
		Session:    session,
	}, nil
}

func (s *service) AuthenticateAPIToken(ctx context.Context, apiToken string) (*Identity, error) {
	if strings.TrimSpace(apiToken) == "" {
		return nil, ErrTokenInvalid
	}

	token, err := s.repo.FindLiveAPIToken(ctx, hashToken(apiToken))
	if err != nil {
		return nil, err
	}
	if token == nil {
		return nil, ErrTokenInvalid
	}

	// Best-effort. A token that authenticated a request but failed to record
	// that it did is still a token that authenticated a request.
	_ = s.repo.TouchAPIToken(ctx, token.ID)

	user, err := s.repo.GetUser(ctx, token.UserID)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			return nil, ErrTokenInvalid
		}
		return nil, err
	}

	tokenID := token.ID
	return &Identity{
		UserID:     user.ID,
		Kind:       CredentialToken,
		IsReadOnly: token.IsReadOnly,
		APITokenID: &tokenID,
	}, nil
}

func (s *service) GetUser(ctx context.Context, id uuid.UUID) (*User, error) {
	return s.repo.GetUser(ctx, id)
}

// ---------------------------------------------------------------------------
// Session and token management
// ---------------------------------------------------------------------------

func (s *service) ListSessions(ctx context.Context, userID uuid.UUID) ([]Session, error) {
	return s.repo.ListActiveSessions(ctx, userID)
}

func (s *service) RevokeSession(ctx context.Context, userID, sessionID uuid.UUID) error {
	return s.repo.RevokeSession(ctx, sessionID, userID)
}

func (s *service) IssueAPIToken(
	ctx context.Context,
	userID uuid.UUID,
	name string,
	readOnly bool,
) (*IssuedToken, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return nil, ErrTokenNameEmpty
	}

	raw, hash, err := s.newTokenFunc()
	if err != nil {
		return nil, err
	}

	token, err := s.repo.CreateAPIToken(
		ctx, NewAPIToken{
			UserID:     userID,
			TokenHash:  hash,
			Name:       trimmed,
			IsReadOnly: readOnly,
		},
	)
	if err != nil {
		return nil, err
	}

	// The only moment raw is ever available. After this it exists solely in
	// whatever the caller does with it.
	return &IssuedToken{APIToken: token, Token: raw}, nil
}

func (s *service) ListAPITokens(ctx context.Context, userID uuid.UUID) ([]APIToken, error) {
	return s.repo.ListActiveAPITokens(ctx, userID)
}

func (s *service) RevokeAPIToken(ctx context.Context, userID, tokenID uuid.UUID) error {
	return s.repo.RevokeAPIToken(ctx, tokenID, userID)
}

// ---------------------------------------------------------------------------
// Tokens
// ---------------------------------------------------------------------------

// generateToken returns a random token and its storage hash.
//
// base64url for transport because the raw value goes into a cookie and an
// Authorization header, neither of which tolerates arbitrary bytes. Hex for the
// hash because that is what the column holds and it compares as plain text.
func generateToken() (raw string, hash string, err error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("auth: generate token: %w", err)
	}
	raw = base64.RawURLEncoding.EncodeToString(buf)
	return raw, hashToken(raw), nil
}

// hashToken is what turns a presented credential into something to look up.
//
// Plain SHA-256, not bcrypt, and that is correct here rather than lazy: bcrypt
// is slow by design to make guessing a low-entropy human password expensive.
// These tokens carry 256 bits of entropy from crypto/rand — they cannot be
// guessed at any speed — and a per-request bcrypt would add a quarter of a
// second to every authenticated call for nothing.
func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// normaliseEmail trims, lowercases and checks the shape.
//
// The column is citext, so the database already treats case as insignificant.
// Normalising here as well means the Go-side "does this exist" check and the
// constraint can never disagree about what counts as the same address.
// The address returned is the one mail.ParseAddress extracted, not the input.
// ParseAddress accepts a full RFC 5322 mailbox — `Farouk <a@b.com>` parses
// happily — and returning the input would store that entire string, display
// name and angle brackets included, as the account's email. It would be unique,
// it would be citext, and it would be wrong: nothing else in the system expects
// an address it cannot send to.
func normaliseEmail(raw string) (string, error) {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	if trimmed == "" {
		return "", ErrEmailRequired
	}
	parsed, err := mail.ParseAddress(trimmed)
	if err != nil {
		return "", ErrEmailInvalid
	}
	// ParseAddress does not lowercase, and the input may have been a mailbox
	// whose address part escaped the earlier pass.
	address := strings.ToLower(strings.TrimSpace(parsed.Address))
	if address == "" {
		return "", ErrEmailRequired
	}
	return address, nil
}

func trimmedOrNil(s *string) *string {
	if s == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*s)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}
