package auth

import (
	"errors"
	"net/netip"
	"time"

	"github.com/google/uuid"
)

// Domain models. No database or HTTP tags — mapping lives in repository.go and
// handler.go respectively, matching the cvs and applications modules.

// User is an account.
//
// PasswordHash is present on the domain type because the service needs it to
// verify a login, and deliberately never appears in handler.go's response DTOs.
// The compiler cannot enforce that; the separation between this and
// userResponse is what does.
type User struct {
	ID              uuid.UUID
	Email           string
	PasswordHash    string
	DisplayName     *string
	EmailVerifiedAt *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Session is one signed-in browser.
//
// There is no Token field, and there never will be. The raw token exists in
// exactly two places — the client's cookie and the return value of Register or
// Login — and is a SHA-256 digest everywhere else.
type Session struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	TokenHash string
	ExpiresAt time.Time
	RevokedAt *time.Time
	UserAgent *string
	IPAddress *netip.Addr
	CreatedAt time.Time
}

// Active reports whether this session can still authenticate a request.
//
// The database filters on the same two conditions in FindLiveSessionByTokenHash,
// so in the normal path this is redundant. It exists for the paths that load a
// session by some other route, where forgetting the check would be silent.
func (s Session) Active(now time.Time) bool {
	return s.RevokedAt == nil && s.ExpiresAt.After(now)
}

// APIToken is a long-lived credential held by the browser extension.
//
// Separate from Session because the lifetimes have nothing in common: a session
// dies in thirty days, a token lives until somebody revokes it. The extension
// has no way to prompt for a re-login, so an expiring token would present as
// the extension mysteriously breaking.
type APIToken struct {
	ID     uuid.UUID
	UserID uuid.UUID
	// IsReadOnly restricts this credential to safe HTTP methods. Enforced in
	// the middleware rather than per-handler: an endpoint written next year is
	// then covered by default, whereas a per-handler check is one somebody
	// eventually forgets on exactly the endpoint where it mattered.
	//
	// It is a property of the token, not of the user. The same account may hold
	// a read-only token for a public demo and a full one for its own extension.
	IsReadOnly bool
	TokenHash  string
	Name       string
	LastUsedAt *time.Time
	RevokedAt  *time.Time
	CreatedAt  time.Time
}

func (t APIToken) Active() bool {
	return t.RevokedAt == nil
}

// Identity is who a request belongs to, as resolved by the middleware.
//
// It carries more than a user id because the middleware has a decision to make
// that a user id cannot answer: whether this credential is allowed to write.
// Kind records which path proved the identity, and IsReadOnly is the answer —
// always false for a session, and whatever the token says otherwise.
type Identity struct {
	UserID     uuid.UUID
	Kind       CredentialKind
	IsReadOnly bool

	// Set only when Kind is CredentialSession.
	Session *Session
	// Set only when Kind is CredentialToken.
	APITokenID *uuid.UUID
}

// CredentialKind is how a request authenticated.
type CredentialKind string

const (
	// CredentialSession is the dashboard: an httpOnly cookie.
	CredentialSession CredentialKind = "session"
	// CredentialToken is the extension, and the dashboard's server-side demo
	// route: a bearer token.
	CredentialToken CredentialKind = "api_token"
)

// ---------------------------------------------------------------------------
// Inputs
// ---------------------------------------------------------------------------

// RegisterInput creates an account. DisplayName is optional; the address is
// what identifies the user.
type RegisterInput struct {
	Email       string
	Password    string
	DisplayName *string

	// Recorded against the session this creates, so the settings page can
	// describe it. Both optional — a missing user agent is a session that is
	// harder to recognise, not a failure.
	UserAgent *string
	IPAddress *netip.Addr
}

// LoginInput authenticates an existing account.
type LoginInput struct {
	Email     string
	Password  string
	UserAgent *string
	IPAddress *netip.Addr
}

// AuthResult is what a successful register or login produced.
//
// SessionToken is the only time the raw token is ever returned. The caller
// writes it into a cookie and forgets it; nothing can recover it afterwards.
type AuthResult struct {
	User         *User
	Session      *Session
	SessionToken string
}

// IssuedToken is a newly created API token.
//
// Token carries the same one-shot rule as AuthResult.SessionToken: shown once,
// unrecoverable after. The handler is responsible for saying so to the user.
type IssuedToken struct {
	APIToken *APIToken
	Token    string
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

var (
	ErrUserNotFound = errors.New("auth: user not found")

	// ErrInvalidCredentials is returned for a wrong password and for an email
	// that does not exist, deliberately without distinction. Two errors here
	// would become an account enumeration oracle at the handler, however
	// carefully the handler tried to collapse them.
	ErrInvalidCredentials = errors.New("auth: invalid email or password")

	ErrEmailTaken       = errors.New("auth: an account with that email already exists")
	ErrEmailRequired    = errors.New("auth: email is required")
	ErrEmailInvalid     = errors.New("auth: email is not a valid address")
	ErrPasswordTooShort = errors.New("auth: password is too short")

	// ErrPasswordTooLong is bcrypt's own 72-byte ceiling surfaced as a domain
	// error. It exists because this module has no composition rules and tells
	// users that length is the whole requirement — which reliably produces
	// someone pasting a passphrase over the limit. Without this they would get
	// a 500 for following the advice, or, on an older x/crypto that truncates
	// silently instead of erroring, an account whose password is its own first
	// 72 bytes and whose remainder never mattered.
	ErrPasswordTooLong = errors.New("auth: password is too long")

	// ErrSessionInvalid covers expired, revoked and never-existed alike. The
	// caller gets one answer because the difference is not theirs to know.
	ErrSessionInvalid = errors.New("auth: session is not valid")
	ErrTokenInvalid   = errors.New("auth: api token is not valid")
	// ErrReadOnly is a perfectly valid credential attempting a write. Kept
	// separate from the invalid-credential errors because the response differs:
	// 403, not 401. The caller has authenticated correctly and signing in again
	// would change nothing, so prompting them to would be a lie.
	ErrReadOnly = errors.New("auth: this token is read-only")

	ErrSessionNotFound = errors.New("auth: session not found")
	ErrTokenNotFound   = errors.New("auth: api token not found")
	ErrTokenNameEmpty  = errors.New("auth: token name is required")
)
