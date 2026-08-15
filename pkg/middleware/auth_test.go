package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/EngenMe/applymind-backend/internal/auth"
)

// ---------------------------------------------------------------------------
// RequireAuth
// ---------------------------------------------------------------------------

// stubAuthService implements auth.Service. Only the two authentication methods
// are ever called by the middleware; the rest exist to satisfy the interface
// and fail loudly if the middleware ever starts reaching for them.
type stubAuthService struct {
	t *testing.T

	session func(ctx context.Context, token string) (*auth.Identity, error)
	token   func(ctx context.Context, token string) (*auth.Identity, error)

	sessionTokens []string
	bearerTokens  []string
}

func (s *stubAuthService) AuthenticateSession(ctx context.Context, token string) (*auth.Identity, error) {
	s.sessionTokens = append(s.sessionTokens, token)
	if s.session != nil {
		return s.session(ctx, token)
	}
	return nil, auth.ErrSessionInvalid
}

func (s *stubAuthService) AuthenticateAPIToken(ctx context.Context, token string) (*auth.Identity, error) {
	s.bearerTokens = append(s.bearerTokens, token)
	if s.token != nil {
		return s.token(ctx, token)
	}
	return nil, auth.ErrTokenInvalid
}

func (s *stubAuthService) unexpected(name string) {
	s.t.Helper()
	s.t.Fatalf("middleware called auth.Service.%s, which it has no business calling", name)
}

func (s *stubAuthService) Register(context.Context, auth.RegisterInput) (*auth.AuthResult, error) {
	s.unexpected("Register")
	return nil, nil
}

func (s *stubAuthService) Login(context.Context, auth.LoginInput) (*auth.AuthResult, error) {
	s.unexpected("Login")
	return nil, nil
}

func (s *stubAuthService) Logout(context.Context, string) error {
	s.unexpected("Logout")
	return nil
}

func (s *stubAuthService) GetUser(context.Context, uuid.UUID) (*auth.User, error) {
	s.unexpected("GetUser")
	return nil, nil
}

func (s *stubAuthService) ListSessions(context.Context, uuid.UUID) ([]auth.Session, error) {
	s.unexpected("ListSessions")
	return nil, nil
}

func (s *stubAuthService) RevokeSession(context.Context, uuid.UUID, uuid.UUID) error {
	s.unexpected("RevokeSession")
	return nil
}

func (s *stubAuthService) IssueAPIToken(context.Context, uuid.UUID, string, bool) (*auth.IssuedToken, error) {
	s.unexpected("IssueAPIToken")
	return nil, nil
}

func (s *stubAuthService) ListAPITokens(context.Context, uuid.UUID) ([]auth.APIToken, error) {
	s.unexpected("ListAPITokens")
	return nil, nil
}

func (s *stubAuthService) RevokeAPIToken(context.Context, uuid.UUID, uuid.UUID) error {
	s.unexpected("RevokeAPIToken")
	return nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// wrap mounts RequireAuth in front of a handler that records the user it saw.
func wrap(svc auth.Service, seen *uuid.UUID) http.Handler {
	next := http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if id, ok := UserID(r.Context()); ok && seen != nil {
				*seen = id
			}
			w.WriteHeader(http.StatusOK)
		},
	)
	return RequireAuth(svc, discardLogger())(next)
}

func sessionIdentity(userID uuid.UUID) *auth.Identity {
	return &auth.Identity{UserID: userID, Kind: auth.CredentialSession}
}

func tokenIdentity(userID uuid.UUID, readOnly bool) *auth.Identity {
	tokenID := uuid.New()
	return &auth.Identity{
		UserID:     userID,
		Kind:       auth.CredentialToken,
		IsReadOnly: readOnly,
		APITokenID: &tokenID,
	}
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding error body %q: %v", rec.Body.String(), err)
	}
	return body["error"]
}

func TestRequireAuth_SessionCookie_Passes(t *testing.T) {
	userID := uuid.New()
	svc := &stubAuthService{
		t: t,
		session: func(context.Context, string) (*auth.Identity, error) {
			return sessionIdentity(userID), nil
		},
	}

	var seen uuid.UUID
	req := httptest.NewRequest(http.MethodGet, "/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "raw-session"})
	rec := httptest.NewRecorder()

	wrap(svc, &seen).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if seen != userID {
		t.Errorf("user in context = %s, want %s", seen, userID)
	}
}

func TestRequireAuth_BearerToken_Passes(t *testing.T) {
	userID := uuid.New()
	svc := &stubAuthService{
		t: t,
		token: func(_ context.Context, token string) (*auth.Identity, error) {
			if token != "raw-token" {
				t.Fatalf("service received token %q, want the value after the Bearer prefix", token)
			}
			return tokenIdentity(userID, false), nil
		},
	}

	var seen uuid.UUID
	req := httptest.NewRequest(http.MethodGet, "/applications", nil)
	req.Header.Set("Authorization", "Bearer raw-token")
	rec := httptest.NewRecorder()

	wrap(svc, &seen).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if seen != userID {
		t.Errorf("user in context = %s, want %s", seen, userID)
	}
}

// The cookie wins, and the bearer token is not even consulted. A dashboard
// request that also happens to carry an Authorization header is still a
// dashboard request.
func TestRequireAuth_CookieWinsOverBearer(t *testing.T) {
	cookieUser := uuid.New()
	svc := &stubAuthService{
		t: t,
		session: func(context.Context, string) (*auth.Identity, error) {
			return sessionIdentity(cookieUser), nil
		},
		token: func(context.Context, string) (*auth.Identity, error) {
			t.Fatal("the bearer token must not be consulted when a cookie is present")
			return nil, nil
		},
	}

	var seen uuid.UUID
	req := httptest.NewRequest(http.MethodGet, "/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "raw-session"})
	req.Header.Set("Authorization", "Bearer raw-token")
	rec := httptest.NewRecorder()

	wrap(svc, &seen).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if seen != cookieUser {
		t.Errorf("user in context = %s, want the cookie's user %s", seen, cookieUser)
	}
	if len(svc.bearerTokens) != 0 {
		t.Errorf("bearer path was taken %d times, want 0", len(svc.bearerTokens))
	}
}

// A dead cookie must not silently fall through to the bearer token: that would
// continue the request as a different identity instead of asking the user to
// sign in again.
func TestRequireAuth_DeadCookieDoesNotFallBackToBearer(t *testing.T) {
	svc := &stubAuthService{
		t: t,
		session: func(context.Context, string) (*auth.Identity, error) {
			return nil, auth.ErrSessionInvalid
		},
		token: func(context.Context, string) (*auth.Identity, error) {
			t.Fatal("a failed cookie must not fall back to the bearer token")
			return nil, nil
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "expired"})
	req.Header.Set("Authorization", "Bearer raw-token")
	rec := httptest.NewRecorder()

	wrap(svc, nil).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestRequireAuth_NoCredential_Is401NotPanic(t *testing.T) {
	svc := &stubAuthService{t: t}

	req := httptest.NewRequest(http.MethodGet, "/applications", nil)
	rec := httptest.NewRecorder()

	// A panic here fails the test by escaping; there is no recover in the
	// middleware and chi's Recoverer is not in play in this unit.
	wrap(svc, nil).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := errorCode(t, rec); got != "unauthorized" {
		t.Errorf("error = %q, want %q", got, "unauthorized")
	}
	if len(svc.sessionTokens)+len(svc.bearerTokens) != 0 {
		t.Error("no credential means no lookup: the service must not be called at all")
	}
}

func TestRequireAuth_InvalidToken_Is401(t *testing.T) {
	svc := &stubAuthService{t: t}

	req := httptest.NewRequest(http.MethodGet, "/applications", nil)
	req.Header.Set("Authorization", "Bearer revoked-or-nonsense")
	rec := httptest.NewRecorder()

	wrap(svc, nil).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// A database failure is not a credential failure. Answering 401 would tell a
// user with a perfectly good session that it had expired.
func TestRequireAuth_ServiceFailureIs500NotUnauthorized(t *testing.T) {
	svc := &stubAuthService{
		t: t,
		token: func(context.Context, string) (*auth.Identity, error) {
			return nil, errors.New("neon is down")
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/applications", nil)
	req.Header.Set("Authorization", "Bearer raw-token")
	rec := httptest.NewRecorder()

	wrap(svc, nil).ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if body := rec.Body.String(); body == "" {
		t.Error("expected a JSON error body")
	}
}

// ---------------------------------------------------------------------------
// Read-only tokens
// ---------------------------------------------------------------------------

// Table-driven over the methods so that widening the allow-list later cannot
// pass silently: a method moved into the safe set has to be moved here too.
func TestRequireAuth_ReadOnlyToken_MethodMatrix(t *testing.T) {
	cases := []struct {
		method     string
		wantStatus int
		wantCode   string
	}{
		{http.MethodGet, http.StatusOK, ""},
		{http.MethodHead, http.StatusOK, ""},
		{http.MethodOptions, http.StatusOK, ""},
		{http.MethodPost, http.StatusForbidden, "read_only_token"},
		{http.MethodPut, http.StatusForbidden, "read_only_token"},
		{http.MethodPatch, http.StatusForbidden, "read_only_token"},
		{http.MethodDelete, http.StatusForbidden, "read_only_token"},
	}

	userID := uuid.New()
	for _, tc := range cases {
		t.Run(
			tc.method, func(t *testing.T) {
				svc := &stubAuthService{
					t: t,
					token: func(context.Context, string) (*auth.Identity, error) {
						return tokenIdentity(userID, true), nil
					},
				}

				req := httptest.NewRequest(tc.method, "/applications", nil)
				req.Header.Set("Authorization", "Bearer demo-token")
				rec := httptest.NewRecorder()

				wrap(svc, nil).ServeHTTP(rec, req)

				if rec.Code != tc.wantStatus {
					t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
				}
				if tc.wantCode == "" {
					return
				}
				if got := errorCode(t, rec); got != tc.wantCode {
					t.Errorf("error = %q, want %q", got, tc.wantCode)
				}
			},
		)
	}
}

// 403 and not 401: the credential is valid, so prompting the caller to
// authenticate again would be a lie.
func TestRequireAuth_ReadOnlyWriteIsNot401(t *testing.T) {
	svc := &stubAuthService{
		t: t,
		token: func(context.Context, string) (*auth.Identity, error) {
			return tokenIdentity(uuid.New(), true), nil
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/applications", nil)
	req.Header.Set("Authorization", "Bearer demo-token")
	rec := httptest.NewRecorder()

	wrap(svc, nil).ServeHTTP(rec, req)

	if rec.Code == http.StatusUnauthorized {
		t.Fatal("a valid read-only token attempting a write must not be answered with 401")
	}
}

// Read-only is a property of the token, not of the user: the same account
// holding a normal token can still write.
func TestRequireAuth_ReadOnlyIsPropertyOfTheToken(t *testing.T) {
	userID := uuid.New()

	readOnly := &stubAuthService{
		t: t,
		token: func(context.Context, string) (*auth.Identity, error) {
			return tokenIdentity(userID, true), nil
		},
	}
	full := &stubAuthService{
		t: t,
		token: func(context.Context, string) (*auth.Identity, error) {
			return tokenIdentity(userID, false), nil
		},
	}

	newPost := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/applications", nil)
		req.Header.Set("Authorization", "Bearer whichever")
		return req
	}

	blocked := httptest.NewRecorder()
	wrap(readOnly, nil).ServeHTTP(blocked, newPost())
	if blocked.Code != http.StatusForbidden {
		t.Fatalf("read-only token POST status = %d, want 403", blocked.Code)
	}

	allowed := httptest.NewRecorder()
	wrap(full, nil).ServeHTTP(allowed, newPost())
	if allowed.Code != http.StatusOK {
		t.Fatalf("same user's full token POST status = %d, want 200", allowed.Code)
	}
}

// A session is never read-only, whatever the request method.
func TestRequireAuth_SessionMayAlwaysWrite(t *testing.T) {
	svc := &stubAuthService{
		t: t,
		session: func(context.Context, string) (*auth.Identity, error) {
			return sessionIdentity(uuid.New()), nil
		},
	}

	req := httptest.NewRequest(http.MethodDelete, "/applications/x", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "raw-session"})
	rec := httptest.NewRecorder()

	wrap(svc, nil).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Context accessors
// ---------------------------------------------------------------------------

func TestUserID_AbsentFromBareContext(t *testing.T) {
	if _, ok := UserID(context.Background()); ok {
		t.Error("UserID must report false for a context RequireAuth never touched")
	}
	if _, ok := IdentityFrom(context.Background()); ok {
		t.Error("IdentityFrom must report false for a context RequireAuth never touched")
	}
}

func TestIdentityFrom_CarriesCredentialKind(t *testing.T) {
	svc := &stubAuthService{
		t: t,
		token: func(context.Context, string) (*auth.Identity, error) {
			return tokenIdentity(uuid.New(), true), nil
		},
	}

	var kind auth.CredentialKind
	var readOnly bool
	next := http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if identity, ok := IdentityFrom(r.Context()); ok {
				kind = identity.Kind
				readOnly = identity.IsReadOnly
			}
			w.WriteHeader(http.StatusOK)
		},
	)

	req := httptest.NewRequest(http.MethodGet, "/applications", nil)
	req.Header.Set("Authorization", "Bearer demo-token")
	RequireAuth(svc, discardLogger())(next).ServeHTTP(httptest.NewRecorder(), req)

	if kind != auth.CredentialToken {
		t.Errorf("kind = %q, want %q", kind, auth.CredentialToken)
	}
	if !readOnly {
		t.Error("the identity in context lost the token's read-only flag")
	}
}

func TestRequireAuth_NilService_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected RequireAuth(nil, ...) to panic, it did not")
		}
	}()
	RequireAuth(nil, discardLogger())
}

// ---------------------------------------------------------------------------
// CheckReadOnlyWrite (the router's MethodNotAllowed fallback)
// ---------------------------------------------------------------------------

func TestCheckReadOnlyWrite_ReadOnlyTokenWriting(t *testing.T) {
	svc := &stubAuthService{
		t: t,
		token: func(context.Context, string) (*auth.Identity, error) {
			return tokenIdentity(uuid.New(), true), nil
		},
	}

	req := httptest.NewRequest(http.MethodPut, "/applications/x", nil)
	req.Header.Set("Authorization", "Bearer demo-token")

	if !CheckReadOnlyWrite(req, svc) {
		t.Fatal("a read-only token on an unregistered write method must be reported, or it gets chi's 405 and its Allow header")
	}
}

// A safe method cannot be a read-only violation, so it must not cost a lookup:
// this runs on the 405 path, where the request is already going nowhere.
func TestCheckReadOnlyWrite_SafeMethodResolvesNothing(t *testing.T) {
	svc := &stubAuthService{
		t: t,
		token: func(context.Context, string) (*auth.Identity, error) {
			return tokenIdentity(uuid.New(), true), nil
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/applications", nil)
	req.Header.Set("Authorization", "Bearer demo-token")

	if CheckReadOnlyWrite(req, svc) {
		t.Error("a GET is never a read-only violation")
	}
	if len(svc.bearerTokens) != 0 {
		t.Errorf("resolved the credential %d time(s) for a safe method", len(svc.bearerTokens))
	}
}

// A session is never read-only, so the cookie path cannot change the answer —
// and resolving it would cost a session lookup, a user lookup and possibly a
// sliding-expiry write on a request that is about to 405.
func TestCheckReadOnlyWrite_CookieShortCircuitsWithoutResolving(t *testing.T) {
	svc := &stubAuthService{
		t: t,
		token: func(context.Context, string) (*auth.Identity, error) {
			return tokenIdentity(uuid.New(), true), nil
		},
	}

	req := httptest.NewRequest(http.MethodPut, "/applications/x", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "live-session"})
	req.Header.Set("Authorization", "Bearer demo-token")

	if CheckReadOnlyWrite(req, svc) {
		t.Error("a request carrying a session cookie must not be judged by its bearer token — RequireAuth would resolve it as the session")
	}
	if len(svc.sessionTokens) != 0 || len(svc.bearerTokens) != 0 {
		t.Errorf(
			"resolved credentials on the cookie path: %d session, %d bearer lookups, want 0 and 0",
			len(svc.sessionTokens), len(svc.bearerTokens),
		)
	}
}

func TestCheckReadOnlyWrite_FullAccessTokenAndNoCredential(t *testing.T) {
	full := &stubAuthService{
		t: t,
		token: func(context.Context, string) (*auth.Identity, error) {
			return tokenIdentity(uuid.New(), false), nil
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/applications", nil)
	req.Header.Set("Authorization", "Bearer real-token")
	if CheckReadOnlyWrite(req, full) {
		t.Error("a full-access token is not a read-only violation")
	}

	bare := &stubAuthService{t: t}
	if CheckReadOnlyWrite(httptest.NewRequest(http.MethodPost, "/applications", nil), bare) {
		t.Error("an unauthenticated request must fall through to chi's ordinary 405")
	}
}
