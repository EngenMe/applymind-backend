package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// ---------------------------------------------------------------------------
// Hand-written fake repository (no third-party mocking dependency)
// ---------------------------------------------------------------------------

// fakeRepo keeps state rather than exposing one hook per call, which is a
// deliberate departure from the per-method mocks in cvs and applications.
// Almost every rule in this module spans two calls — register then log in,
// issue then authenticate, revoke then fail — and a hook-only mock turns those
// into assertions about what the service asked for rather than what actually
// happens. Where a specific failure has to be forced, the hooks below override.
//
// Every Find* mirrors the filtering its SQL does. FindLiveSession in particular
// excludes revoked and expired rows in the query, so a fake that returned them
// would let a test pass against a service that had stopped checking.
type fakeRepo struct {
	now func() time.Time

	users    map[uuid.UUID]User
	byEmail  map[string]uuid.UUID
	sessions map[uuid.UUID]Session
	tokens   map[uuid.UUID]APIToken

	extendCalls int
	touchCalls  int

	// Overrides for the paths that only fail against a real database.
	findUserByEmail func(ctx context.Context, email string) (*User, error)
	createUserErr   error
	extendErr       error
}

func newFakeRepo(now func() time.Time) *fakeRepo {
	return &fakeRepo{
		now:      now,
		users:    map[uuid.UUID]User{},
		byEmail:  map[string]uuid.UUID{},
		sessions: map[uuid.UUID]Session{},
		tokens:   map[uuid.UUID]APIToken{},
	}
}

func (f *fakeRepo) CreateUser(_ context.Context, in NewUser) (*User, error) {
	if f.createUserErr != nil {
		return nil, f.createUserErr
	}
	// The unique constraint, which is what actually guarantees this — the
	// service's own check races and the repository translates the violation.
	if _, exists := f.byEmail[in.Email]; exists {
		return nil, ErrEmailTaken
	}
	now := f.now()
	user := User{
		ID:              uuid.New(),
		Email:           in.Email,
		PasswordHash:    in.PasswordHash,
		DisplayName:     in.DisplayName,
		EmailVerifiedAt: in.EmailVerifiedAt,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	f.users[user.ID] = user
	f.byEmail[in.Email] = user.ID
	return &user, nil
}

func (f *fakeRepo) GetUser(_ context.Context, id uuid.UUID) (*User, error) {
	user, ok := f.users[id]
	if !ok {
		return nil, ErrUserNotFound
	}
	return &user, nil
}

func (f *fakeRepo) FindUserByEmail(ctx context.Context, email string) (*User, error) {
	if f.findUserByEmail != nil {
		return f.findUserByEmail(ctx, email)
	}
	id, ok := f.byEmail[email]
	if !ok {
		return nil, nil
	}
	user := f.users[id]
	return &user, nil
}

func (f *fakeRepo) UpdateUserPassword(_ context.Context, id uuid.UUID, hash string) (*User, error) {
	user, ok := f.users[id]
	if !ok {
		return nil, ErrUserNotFound
	}
	user.PasswordHash = hash
	user.UpdatedAt = f.now()
	f.users[id] = user
	return &user, nil
}

func (f *fakeRepo) CreateSession(_ context.Context, in NewSession) (*Session, error) {
	session := Session{
		ID:        uuid.New(),
		UserID:    in.UserID,
		TokenHash: in.TokenHash,
		ExpiresAt: in.ExpiresAt,
		UserAgent: in.UserAgent,
		IPAddress: in.IPAddress,
		CreatedAt: f.now(),
	}
	f.sessions[session.ID] = session
	return &session, nil
}

// FindLiveSession mirrors FindLiveSessionByTokenHash: revoked and expired rows
// are filtered in SQL and never reach Go.
func (f *fakeRepo) FindLiveSession(_ context.Context, tokenHash string) (*Session, error) {
	for _, session := range f.sessions {
		if session.TokenHash != tokenHash {
			continue
		}
		if !session.Active(f.now()) {
			return nil, nil
		}
		found := session
		return &found, nil
	}
	return nil, nil
}

func (f *fakeRepo) ExtendSession(_ context.Context, id uuid.UUID, expiresAt time.Time) error {
	f.extendCalls++
	if f.extendErr != nil {
		return f.extendErr
	}
	session, ok := f.sessions[id]
	if !ok {
		return nil
	}
	session.ExpiresAt = expiresAt
	f.sessions[id] = session
	return nil
}

func (f *fakeRepo) RevokeSession(_ context.Context, id, userID uuid.UUID) error {
	session, ok := f.sessions[id]
	if !ok || session.UserID != userID || session.RevokedAt != nil {
		return ErrSessionNotFound
	}
	now := f.now()
	session.RevokedAt = &now
	f.sessions[id] = session
	return nil
}

func (f *fakeRepo) RevokeSessionByTokenHash(_ context.Context, tokenHash string) error {
	for id, session := range f.sessions {
		if session.TokenHash != tokenHash {
			continue
		}
		if session.RevokedAt == nil {
			now := f.now()
			session.RevokedAt = &now
			f.sessions[id] = session
		}
	}
	return nil
}

func (f *fakeRepo) RevokeAllUserSessions(_ context.Context, userID uuid.UUID) error {
	for id, session := range f.sessions {
		if session.UserID == userID && session.RevokedAt == nil {
			now := f.now()
			session.RevokedAt = &now
			f.sessions[id] = session
		}
	}
	return nil
}

func (f *fakeRepo) ListActiveSessions(_ context.Context, userID uuid.UUID) ([]Session, error) {
	out := make([]Session, 0, len(f.sessions))
	for _, session := range f.sessions {
		if session.UserID == userID && session.Active(f.now()) {
			out = append(out, session)
		}
	}
	return out, nil
}

func (f *fakeRepo) CreateAPIToken(_ context.Context, in NewAPIToken) (*APIToken, error) {
	token := APIToken{
		ID:         uuid.New(),
		UserID:     in.UserID,
		IsReadOnly: in.IsReadOnly,
		TokenHash:  in.TokenHash,
		Name:       in.Name,
		CreatedAt:  f.now(),
	}
	f.tokens[token.ID] = token
	return &token, nil
}

func (f *fakeRepo) FindLiveAPIToken(_ context.Context, tokenHash string) (*APIToken, error) {
	for _, token := range f.tokens {
		if token.TokenHash != tokenHash {
			continue
		}
		if !token.Active() {
			return nil, nil
		}
		found := token
		return &found, nil
	}
	return nil, nil
}

func (f *fakeRepo) TouchAPIToken(_ context.Context, id uuid.UUID) error {
	f.touchCalls++
	token, ok := f.tokens[id]
	if !ok {
		return nil
	}
	now := f.now()
	token.LastUsedAt = &now
	f.tokens[id] = token
	return nil
}

func (f *fakeRepo) RevokeAPIToken(_ context.Context, id, userID uuid.UUID) error {
	token, ok := f.tokens[id]
	if !ok || token.UserID != userID || token.RevokedAt != nil {
		return ErrTokenNotFound
	}
	now := f.now()
	token.RevokedAt = &now
	f.tokens[id] = token
	return nil
}

func (f *fakeRepo) ListActiveAPITokens(_ context.Context, userID uuid.UUID) ([]APIToken, error) {
	out := make([]APIToken, 0, len(f.tokens))
	for _, token := range f.tokens {
		if token.UserID == userID && token.Active() {
			out = append(out, token)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Test harness
// ---------------------------------------------------------------------------

type testClock struct{ t time.Time }

func (c *testClock) Now() time.Time          { return c.t }
func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }
func newTestClock() *testClock               { return &testClock{t: time.Date(2026, 5, 16, 9, 0, 0, 0, time.UTC)} }

// newTestService builds a service on the fake repo at the lowest bcrypt cost.
// The default cost 12 is ~250ms per hash, which turns a table of a dozen cases
// into a wall-clock problem; the cost being configurable is what makes that
// avoidable, and nothing under test depends on its value.
func newTestService(t *testing.T, opts ...ServiceOption) (Service, *fakeRepo, *testClock) {
	t.Helper()
	clock := newTestClock()
	repo := newFakeRepo(clock.Now)
	base := []ServiceOption{WithClock(clock.Now), WithBcryptCost(bcrypt.MinCost)}
	return NewService(repo, append(base, opts...)...), repo, clock
}

const testPassword = "correct-horse-battery"

func strPtr(s string) *string { return &s }

func mustRegister(t *testing.T, svc Service, email string) *AuthResult {
	t.Helper()
	result, err := svc.Register(context.Background(), RegisterInput{Email: email, Password: testPassword})
	if err != nil {
		t.Fatalf("register %q: %v", email, err)
	}
	return result
}

// ---------------------------------------------------------------------------
// Registration and login
// ---------------------------------------------------------------------------

func TestRegisterThenLogin_Succeeds(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()

	registered := mustRegister(t, svc, "someone@example.com")
	if registered.SessionToken == "" {
		t.Fatal("register must return a raw session token")
	}

	loggedIn, err := svc.Login(ctx, LoginInput{Email: "someone@example.com", Password: testPassword})
	if err != nil {
		t.Fatalf("login after register: %v", err)
	}
	if loggedIn.User.ID != registered.User.ID {
		t.Errorf("login returned user %s, want %s", loggedIn.User.ID, registered.User.ID)
	}
	if loggedIn.SessionToken == registered.SessionToken {
		t.Error("each login must mint its own session token")
	}
}

func TestRegister_DuplicateEmailIsCaseInsensitive(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()

	mustRegister(t, svc, "Someone@Example.com")

	_, err := svc.Register(ctx, RegisterInput{Email: "  someone@example.COM  ", Password: testPassword})
	if !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("second register error = %v, want ErrEmailTaken", err)
	}
}

// The Go-side existence check races: two simultaneous registrations for the same
// address both see nothing. The unique constraint is what actually guarantees
// uniqueness, and its violation has to surface as the same error.
func TestRegister_ConstraintViolationSurfacesAsEmailTaken(t *testing.T) {
	svc, repo, _ := newTestService(t)
	mustRegister(t, svc, "someone@example.com")

	// Simulate losing the race: the lookup finds nothing, the insert does not.
	repo.findUserByEmail = func(context.Context, string) (*User, error) { return nil, nil }

	_, err := svc.Register(
		context.Background(),
		RegisterInput{Email: "someone@example.com", Password: testPassword},
	)
	if !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("error = %v, want ErrEmailTaken from the constraint", err)
	}
}

func TestRegister_StoresNormalisedEmail(t *testing.T) {
	svc, repo, _ := newTestService(t)

	result := mustRegister(t, svc, "  MiXeD@Example.COM ")
	stored := repo.users[result.User.ID]

	if stored.Email != "mixed@example.com" {
		t.Errorf("stored email = %q, want it trimmed and lowercased", stored.Email)
	}
}

func TestRegister_PasswordLengthBoundary(t *testing.T) {
	cases := []struct {
		name     string
		password string
		wantErr  error
	}{
		{"one short", strings.Repeat("a", MinPasswordLength-1), ErrPasswordTooShort},
		{"exactly the minimum", strings.Repeat("a", MinPasswordLength), nil},
		{"empty", "", ErrPasswordTooShort},
	}

	for _, tc := range cases {
		t.Run(
			tc.name, func(t *testing.T) {
				svc, _, _ := newTestService(t)
				_, err := svc.Register(
					context.Background(),
					RegisterInput{Email: "someone@example.com", Password: tc.password},
				)
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want %v", err, tc.wantErr)
				}
			},
		)
	}
}

// bcrypt reads 72 bytes and ignores the rest. Rejecting rather than truncating
// means two passphrases sharing a 72-byte prefix are never the same credential.
func TestRegister_PasswordAboveBcryptsCeilingIsRejected(t *testing.T) {
	cases := []struct {
		name     string
		password string
		wantErr  error
	}{
		{"exactly the maximum", strings.Repeat("a", MaxPasswordLength), nil},
		{"one over", strings.Repeat("a", MaxPasswordLength+1), ErrPasswordTooLong},
		// Bytes, not runes: 40 three-byte characters is 120 bytes, which bcrypt
		// will not read past even though it is well under 72 characters.
		{"multibyte under 72 runes but over 72 bytes", strings.Repeat("パ", 40), ErrPasswordTooLong},
	}

	for _, tc := range cases {
		t.Run(
			tc.name, func(t *testing.T) {
				svc, _, _ := newTestService(t)
				_, err := svc.Register(
					context.Background(),
					RegisterInput{Email: "someone@example.com", Password: tc.password},
				)
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want %v", err, tc.wantErr)
				}
			},
		)
	}
}

// mail.ParseAddress accepts a full mailbox, so the display name has to be
// dropped rather than stored as part of the address.
func TestRegister_MailboxFormStoresOnlyTheAddress(t *testing.T) {
	svc, repo, _ := newTestService(t)

	result, err := svc.Register(
		context.Background(),
		RegisterInput{Email: "Farouk <Someone@Example.com>", Password: testPassword},
	)
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	if stored := repo.users[result.User.ID].Email; stored != "someone@example.com" {
		t.Errorf("stored email = %q, want the parsed address with the display name dropped", stored)
	}
}

// The address is what identifies an account, so the mailbox form of an existing
// address must collide with it rather than opening a second account.
func TestRegister_MailboxFormCollidesWithThePlainAddress(t *testing.T) {
	svc, _, _ := newTestService(t)
	mustRegister(t, svc, "someone@example.com")

	_, err := svc.Register(
		context.Background(),
		RegisterInput{Email: "Someone Else <someone@example.com>", Password: testPassword},
	)
	if !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("error = %v, want ErrEmailTaken", err)
	}
}

// No composition rules: length is the whole requirement.
func TestRegister_LongAllLowercasePasswordIsAccepted(t *testing.T) {
	svc, _, _ := newTestService(t)
	if _, err := svc.Register(
		context.Background(),
		RegisterInput{Email: "someone@example.com", Password: "sixteencharacters"},
	); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRegister_EmailValidation(t *testing.T) {
	cases := []struct {
		name    string
		email   string
		wantErr error
	}{
		{"empty", "   ", ErrEmailRequired},
		{"no at sign", "not-an-address", ErrEmailInvalid},
	}

	for _, tc := range cases {
		t.Run(
			tc.name, func(t *testing.T) {
				svc, _, _ := newTestService(t)
				_, err := svc.Register(
					context.Background(),
					RegisterInput{Email: tc.email, Password: testPassword},
				)
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want %v", err, tc.wantErr)
				}
			},
		)
	}
}

func TestRegister_BlankDisplayNameBecomesNil(t *testing.T) {
	svc, repo, _ := newTestService(t)

	result, err := svc.Register(
		context.Background(), RegisterInput{
			Email:       "someone@example.com",
			Password:    testPassword,
			DisplayName: strPtr("   "),
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stored := repo.users[result.User.ID]; stored.DisplayName != nil {
		t.Errorf("display name = %q, want nil for a whitespace-only value", *stored.DisplayName)
	}
}

func TestRegister_SetsEmailVerifiedAt(t *testing.T) {
	svc, _, _ := newTestService(t)
	result := mustRegister(t, svc, "someone@example.com")

	if result.User.EmailVerifiedAt == nil {
		t.Error("email_verified_at is set at registration for now; nothing gates on it, but null would make every existing account unverified the day verification arrives")
	}
}

// Wrong password and unknown email must be one answer. Two errors here would
// become an account enumeration oracle at the handler however carefully the
// handler tried to collapse them.
func TestLogin_WrongPasswordAndUnknownEmailAreIndistinguishable(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	mustRegister(t, svc, "someone@example.com")

	_, wrongPassword := svc.Login(ctx, LoginInput{Email: "someone@example.com", Password: "not-the-password"})
	_, unknownEmail := svc.Login(ctx, LoginInput{Email: "nobody@example.com", Password: testPassword})
	_, malformedEmail := svc.Login(ctx, LoginInput{Email: "not-an-address", Password: testPassword})

	for name, err := range map[string]error{
		"wrong password":  wrongPassword,
		"unknown email":   unknownEmail,
		"malformed email": malformedEmail,
	} {
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Errorf("%s: error = %v, want ErrInvalidCredentials", name, err)
		}
	}
	if wrongPassword.Error() != unknownEmail.Error() {
		t.Errorf("messages differ: %q vs %q", wrongPassword, unknownEmail)
	}
}

// dummyHash has to be a well-formed bcrypt hash at the production cost, or the
// comparison against it returns instantly, and the timing equalisation it exists
// for silently stops working.
func TestDummyHashIsUsableAtProductionCost(t *testing.T) {
	cost, err := bcrypt.Cost(dummyHash)
	if err != nil {
		t.Fatalf("dummyHash is not a valid bcrypt hash: %v", err)
	}
	if cost != BcryptCost {
		t.Errorf("dummyHash cost = %d, want %d", cost, BcryptCost)
	}
}

// The complement to the test above: an unknown email must still spend the time.
// Measured with a wide margin — bcrypt at cost 12 is hundreds of milliseconds,
// so a millisecond floor separates "hashed something" from "returned early"
// without being sensitive to how fast the machine is.
func TestLogin_UnknownEmailStillHashes(t *testing.T) {
	svc, _, _ := newTestService(t)

	start := time.Now()
	if _, err := svc.Login(
		context.Background(),
		LoginInput{Email: "nobody@example.com", Password: testPassword},
	); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("error = %v, want ErrInvalidCredentials", err)
	}
	elapsed := time.Since(start)

	if elapsed < 5*time.Millisecond {
		t.Errorf(
			"login for an unknown email returned in %v — the dummy bcrypt comparison is not running, "+
				"which makes this endpoint an account enumeration oracle", elapsed,
		)
	}
}

// ---------------------------------------------------------------------------
// Session authentication
// ---------------------------------------------------------------------------

func TestAuthenticateSession_HappyPath(t *testing.T) {
	svc, _, _ := newTestService(t)
	registered := mustRegister(t, svc, "someone@example.com")

	identity, err := svc.AuthenticateSession(context.Background(), registered.SessionToken)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if identity.UserID != registered.User.ID {
		t.Errorf("user = %s, want %s", identity.UserID, registered.User.ID)
	}
	if identity.Kind != CredentialSession {
		t.Errorf("kind = %q, want %q", identity.Kind, CredentialSession)
	}
	if identity.IsReadOnly {
		t.Error("a session is never read-only")
	}
}

func TestAuthenticateSession_ExpiredIsRejected(t *testing.T) {
	svc, _, clock := newTestService(t)
	registered := mustRegister(t, svc, "someone@example.com")

	clock.advance(DefaultSessionTTL + time.Minute)

	if _, err := svc.AuthenticateSession(context.Background(), registered.SessionToken); !errors.Is(
		err, ErrSessionInvalid,
	) {
		t.Fatalf("error = %v, want ErrSessionInvalid", err)
	}
}

func TestAuthenticateSession_RevokedIsRejected(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	registered := mustRegister(t, svc, "someone@example.com")

	if err := svc.RevokeSession(ctx, registered.User.ID, registered.Session.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	if _, err := svc.AuthenticateSession(ctx, registered.SessionToken); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("error = %v, want ErrSessionInvalid", err)
	}
}

func TestAuthenticateSession_UnknownAndEmptyTokensRejected(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()

	for name, token := range map[string]string{"unknown": "nonsense", "empty": "", "blank": "   "} {
		if _, err := svc.AuthenticateSession(ctx, token); !errors.Is(err, ErrSessionInvalid) {
			t.Errorf("%s token: error = %v, want ErrSessionInvalid", name, err)
		}
	}
}

// A session whose user has gone is not a session. The cascade should have taken
// it, so this is a surprise — but not one the caller hears about differently.
func TestAuthenticateSession_DeletedUserIsRejected(t *testing.T) {
	svc, repo, _ := newTestService(t)
	registered := mustRegister(t, svc, "someone@example.com")

	delete(repo.users, registered.User.ID)

	if _, err := svc.AuthenticateSession(context.Background(), registered.SessionToken); !errors.Is(
		err, ErrSessionInvalid,
	) {
		t.Fatalf("error = %v, want ErrSessionInvalid", err)
	}
}

// ---------------------------------------------------------------------------
// Sliding expiry
// ---------------------------------------------------------------------------

// Two uses an hour apart must produce at most one write. Without the
// extend-after rule the sliding window means a database write on every single
// authenticated request, to move an expiry that is already a month away.
func TestAuthenticateSession_UsedTwiceWithinAnHour_ExtendsOnce(t *testing.T) {
	svc, repo, clock := newTestService(t)
	ctx := context.Background()
	registered := mustRegister(t, svc, "someone@example.com")

	// Far enough in that an extension is due.
	clock.advance(DefaultExtendAfter + time.Hour)
	if _, err := svc.AuthenticateSession(ctx, registered.SessionToken); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if repo.extendCalls != 1 {
		t.Fatalf("extend calls after the first due use = %d, want 1", repo.extendCalls)
	}

	clock.advance(30 * time.Minute)
	if _, err := svc.AuthenticateSession(ctx, registered.SessionToken); err != nil {
		t.Fatalf("second use: %v", err)
	}
	if repo.extendCalls != 1 {
		t.Errorf("extend calls after a second use half an hour later = %d, want 1", repo.extendCalls)
	}
}

func TestAuthenticateSession_FreshSessionIsNotExtended(t *testing.T) {
	svc, repo, clock := newTestService(t)
	ctx := context.Background()
	registered := mustRegister(t, svc, "someone@example.com")

	clock.advance(time.Minute)
	if _, err := svc.AuthenticateSession(ctx, registered.SessionToken); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if repo.extendCalls != 0 {
		t.Errorf("extend calls = %d, want 0 for a session minted a minute ago", repo.extendCalls)
	}
}

func TestAuthenticateSession_ExtensionMovesExpiryAFullTTLForward(t *testing.T) {
	svc, repo, clock := newTestService(t)
	registered := mustRegister(t, svc, "someone@example.com")

	clock.advance(DefaultExtendAfter + time.Hour)
	if _, err := svc.AuthenticateSession(context.Background(), registered.SessionToken); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := clock.Now().UTC().Add(DefaultSessionTTL)
	if got := repo.sessions[registered.Session.ID].ExpiresAt; !got.Equal(want) {
		t.Errorf("expires_at = %v, want %v", got, want)
	}
}

// A failure to slide the window forward is not a reason to reject a session
// that is currently valid; the worst case is that it expires on schedule.
func TestAuthenticateSession_ExtensionFailureDoesNotFailTheRequest(t *testing.T) {
	svc, repo, clock := newTestService(t)
	registered := mustRegister(t, svc, "someone@example.com")

	clock.advance(DefaultExtendAfter + time.Hour)
	repo.extendErr = errors.New("neon is down")

	identity, err := svc.AuthenticateSession(context.Background(), registered.SessionToken)
	if err != nil {
		t.Fatalf("a failed extension must not fail an otherwise valid session: %v", err)
	}
	if identity.UserID != registered.User.ID {
		t.Errorf("user = %s, want %s", identity.UserID, registered.User.ID)
	}
	if repo.extendCalls != 1 {
		t.Errorf("extend calls = %d, want 1", repo.extendCalls)
	}
}

// ---------------------------------------------------------------------------
// Logout
// ---------------------------------------------------------------------------

func TestLogout_RevokesTheSession(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	registered := mustRegister(t, svc, "someone@example.com")

	if err := svc.Logout(ctx, registered.SessionToken); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if _, err := svc.AuthenticateSession(ctx, registered.SessionToken); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("error after logout = %v, want ErrSessionInvalid", err)
	}
}

// Logging out something already dead is a success: the caller wanted it
// unusable, and it is.
func TestLogout_IsIdempotentAndTolerantOfNothing(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	registered := mustRegister(t, svc, "someone@example.com")

	for _, token := range []string{registered.SessionToken, registered.SessionToken, "", "never-existed"} {
		if err := svc.Logout(ctx, token); err != nil {
			t.Errorf("logout(%q) = %v, want nil", token, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Sessions listing and revocation
// ---------------------------------------------------------------------------

func TestListSessions_ExcludesRevoked(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	registered := mustRegister(t, svc, "someone@example.com")

	second, err := svc.Login(ctx, LoginInput{Email: "someone@example.com", Password: testPassword})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if err := svc.RevokeSession(ctx, registered.User.ID, second.Session.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	sessions, err := svc.ListSessions(ctx, registered.User.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(sessions) != 1 || sessions[0].ID != registered.Session.ID {
		t.Fatalf("listed %d sessions, want only the surviving one", len(sessions))
	}
}

// Revoking somebody else's session reports not-found rather than forbidden.
// Distinguishing the two would confirm that the session exists.
func TestRevokeSession_AnotherUsersSessionIsNotFound(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()

	mine := mustRegister(t, svc, "me@example.com")
	theirs := mustRegister(t, svc, "them@example.com")

	if err := svc.RevokeSession(ctx, mine.User.ID, theirs.Session.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("error = %v, want ErrSessionNotFound", err)
	}
	if _, err := svc.AuthenticateSession(ctx, theirs.SessionToken); err != nil {
		t.Error("the other user's session must still work")
	}
}

// ---------------------------------------------------------------------------
// API tokens
// ---------------------------------------------------------------------------

func TestIssueAPIToken_RawValueIsReturnedOnceAndStoredHashed(t *testing.T) {
	svc, repo, _ := newTestService(t)
	ctx := context.Background()
	registered := mustRegister(t, svc, "someone@example.com")

	issued, err := svc.IssueAPIToken(ctx, registered.User.ID, "  extension  ", false)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if issued.Token == "" {
		t.Fatal("the raw token must be returned to the caller once")
	}
	if issued.APIToken.Name != "extension" {
		t.Errorf("name = %q, want it trimmed", issued.APIToken.Name)
	}

	stored := repo.tokens[issued.APIToken.ID]
	if stored.TokenHash == issued.Token {
		t.Fatal("the raw token was persisted; only its digest may be stored")
	}
	if stored.TokenHash != hashToken(issued.Token) {
		t.Error("stored hash does not match the digest of the issued token")
	}

	// And nothing anywhere can hand it back.
	listed, err := svc.ListAPITokens(ctx, registered.User.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("listed %d tokens, want 1", len(listed))
	}
	if strings.Contains(listed[0].TokenHash, issued.Token) {
		t.Error("the raw token is recoverable from the listing")
	}
}

func TestIssueAPIToken_EmptyNameRejected(t *testing.T) {
	svc, _, _ := newTestService(t)
	registered := mustRegister(t, svc, "someone@example.com")

	if _, err := svc.IssueAPIToken(
		context.Background(), registered.User.ID, "   ", false,
	); !errors.Is(err, ErrTokenNameEmpty) {
		t.Fatalf("error = %v, want ErrTokenNameEmpty", err)
	}
}

func TestAuthenticateAPIToken_HappyPathTouchesLastUsed(t *testing.T) {
	svc, repo, _ := newTestService(t)
	ctx := context.Background()
	registered := mustRegister(t, svc, "someone@example.com")

	issued, err := svc.IssueAPIToken(ctx, registered.User.ID, "extension", false)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	identity, err := svc.AuthenticateAPIToken(ctx, issued.Token)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if identity.UserID != registered.User.ID {
		t.Errorf("user = %s, want %s", identity.UserID, registered.User.ID)
	}
	if identity.Kind != CredentialToken {
		t.Errorf("kind = %q, want %q", identity.Kind, CredentialToken)
	}
	if identity.APITokenID == nil || *identity.APITokenID != issued.APIToken.ID {
		t.Error("the identity must record which token proved it")
	}
	if repo.touchCalls != 1 {
		t.Errorf("touch calls = %d, want 1", repo.touchCalls)
	}
	if repo.tokens[issued.APIToken.ID].LastUsedAt == nil {
		t.Error("last_used_at was not recorded")
	}
}

func TestAuthenticateAPIToken_RevokedIsRejected(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	registered := mustRegister(t, svc, "someone@example.com")

	issued, err := svc.IssueAPIToken(ctx, registered.User.ID, "extension", false)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if err := svc.RevokeAPIToken(ctx, registered.User.ID, issued.APIToken.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	if _, err := svc.AuthenticateAPIToken(ctx, issued.Token); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("error = %v, want ErrTokenInvalid", err)
	}
}

func TestAuthenticateAPIToken_UnknownAndEmptyRejected(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()

	for name, token := range map[string]string{"unknown": "nonsense", "empty": "", "blank": "  "} {
		if _, err := svc.AuthenticateAPIToken(ctx, token); !errors.Is(err, ErrTokenInvalid) {
			t.Errorf("%s token: error = %v, want ErrTokenInvalid", name, err)
		}
	}
}

func TestRevokeAPIToken_AnotherUsersTokenIsNotFound(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()

	mine := mustRegister(t, svc, "me@example.com")
	theirs := mustRegister(t, svc, "them@example.com")

	issued, err := svc.IssueAPIToken(ctx, theirs.User.ID, "theirs", false)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	if err := svc.RevokeAPIToken(ctx, mine.User.ID, issued.APIToken.ID); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("error = %v, want ErrTokenNotFound", err)
	}
}

// Read-only is a property of the token, not of the user: one account can hold
// a restricted token for a public demo and a full one for its own extension.
func TestAuthenticateAPIToken_ReadOnlyIsCarriedPerToken(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	registered := mustRegister(t, svc, "demo@example.com")

	restricted, err := svc.IssueAPIToken(ctx, registered.User.ID, "public demo", true)
	if err != nil {
		t.Fatalf("issue restricted: %v", err)
	}
	full, err := svc.IssueAPIToken(ctx, registered.User.ID, "my extension", false)
	if err != nil {
		t.Fatalf("issue full: %v", err)
	}

	restrictedIdentity, err := svc.AuthenticateAPIToken(ctx, restricted.Token)
	if err != nil {
		t.Fatalf("authenticate restricted: %v", err)
	}
	fullIdentity, err := svc.AuthenticateAPIToken(ctx, full.Token)
	if err != nil {
		t.Fatalf("authenticate full: %v", err)
	}

	if !restrictedIdentity.IsReadOnly {
		t.Error("the read-only token did not resolve to a read-only identity")
	}
	if fullIdentity.IsReadOnly {
		t.Error("the same user's full token must not be restricted")
	}
	if restrictedIdentity.UserID != fullIdentity.UserID {
		t.Error("both tokens belong to the same account")
	}
}

// ---------------------------------------------------------------------------
// Token generation
// ---------------------------------------------------------------------------

func TestGenerateToken_IsUniqueAndHashesConsistently(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		raw, hash, err := generateToken()
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if seen[raw] {
			t.Fatal("generateToken returned a duplicate")
		}
		seen[raw] = true

		if hash != hashToken(raw) {
			t.Fatal("the returned hash is not the digest of the returned token")
		}
		if strings.ContainsAny(raw, "+/=") {
			t.Errorf("token %q is not base64url — it has to survive a cookie and a header", raw)
		}
	}
}

func TestWithTokenGenerator_IsUsedForSessionsAndTokens(t *testing.T) {
	clock := newTestClock()
	repo := newFakeRepo(clock.Now)
	svc := NewService(
		repo,
		WithClock(clock.Now),
		WithBcryptCost(bcrypt.MinCost),
		WithTokenGenerator(func() (string, string, error) { return "fixed-raw", hashToken("fixed-raw"), nil }),
	)

	registered := mustRegister(t, svc, "someone@example.com")
	if registered.SessionToken != "fixed-raw" {
		t.Errorf("session token = %q, want the generator's value", registered.SessionToken)
	}
	if repo.sessions[registered.Session.ID].TokenHash != hashToken("fixed-raw") {
		t.Error("the session was stored with something other than the generator's hash")
	}
}
