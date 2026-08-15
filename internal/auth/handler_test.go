package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Mock service (hand-written, same shape as the cvs handler tests)
// ---------------------------------------------------------------------------

type mockService struct {
	register    func(ctx context.Context, in RegisterInput) (*AuthResult, error)
	login       func(ctx context.Context, in LoginInput) (*AuthResult, error)
	logout      func(ctx context.Context, token string) error
	getUser     func(ctx context.Context, id uuid.UUID) (*User, error)
	listSess    func(ctx context.Context, userID uuid.UUID) ([]Session, error)
	revokeSess  func(ctx context.Context, userID, sessionID uuid.UUID) error
	issueToken  func(ctx context.Context, userID uuid.UUID, name string, readOnly bool) (*IssuedToken, error)
	listTokens  func(ctx context.Context, userID uuid.UUID) ([]APIToken, error)
	revokeToken func(ctx context.Context, userID, tokenID uuid.UUID) error

	lastRegister    RegisterInput
	lastLogin       LoginInput
	lastLogoutToken string
	lastTokenName   string
	lastTokenReadOn bool
	logoutCalls     int
}

func (m *mockService) Register(ctx context.Context, in RegisterInput) (*AuthResult, error) {
	m.lastRegister = in
	if m.register != nil {
		return m.register(ctx, in)
	}
	return newAuthResult(), nil
}

func (m *mockService) Login(ctx context.Context, in LoginInput) (*AuthResult, error) {
	m.lastLogin = in
	if m.login != nil {
		return m.login(ctx, in)
	}
	return newAuthResult(), nil
}

func (m *mockService) Logout(ctx context.Context, token string) error {
	m.logoutCalls++
	m.lastLogoutToken = token
	if m.logout != nil {
		return m.logout(ctx, token)
	}
	return nil
}

func (m *mockService) AuthenticateSession(context.Context, string) (*Identity, error) {
	return nil, ErrSessionInvalid
}

func (m *mockService) AuthenticateAPIToken(context.Context, string) (*Identity, error) {
	return nil, ErrTokenInvalid
}

func (m *mockService) GetUser(ctx context.Context, id uuid.UUID) (*User, error) {
	if m.getUser != nil {
		return m.getUser(ctx, id)
	}
	return &User{ID: id, Email: "someone@example.com", PasswordHash: secretHash}, nil
}

func (m *mockService) ListSessions(ctx context.Context, userID uuid.UUID) ([]Session, error) {
	if m.listSess != nil {
		return m.listSess(ctx, userID)
	}
	return nil, nil
}

func (m *mockService) RevokeSession(ctx context.Context, userID, sessionID uuid.UUID) error {
	if m.revokeSess != nil {
		return m.revokeSess(ctx, userID, sessionID)
	}
	return nil
}

func (m *mockService) IssueAPIToken(
	ctx context.Context,
	userID uuid.UUID,
	name string,
	readOnly bool,
) (*IssuedToken, error) {
	m.lastTokenName = name
	m.lastTokenReadOn = readOnly
	if m.issueToken != nil {
		return m.issueToken(ctx, userID, name, readOnly)
	}
	return &IssuedToken{
		APIToken: &APIToken{ID: uuid.New(), UserID: userID, Name: name, IsReadOnly: readOnly},
		Token:    "raw-token-value",
	}, nil
}

func (m *mockService) ListAPITokens(ctx context.Context, userID uuid.UUID) ([]APIToken, error) {
	if m.listTokens != nil {
		return m.listTokens(ctx, userID)
	}
	return nil, nil
}

func (m *mockService) RevokeAPIToken(ctx context.Context, userID, tokenID uuid.UUID) error {
	if m.revokeToken != nil {
		return m.revokeToken(ctx, userID, tokenID)
	}
	return nil
}

// secretHash is what must never appear in a response body.
const secretHash = "$2a$12$notarealhashbutdistinctive"

func newAuthResult() *AuthResult {
	userID := uuid.New()
	return &AuthResult{
		User: &User{
			ID:           userID,
			Email:        "someone@example.com",
			PasswordHash: secretHash,
			CreatedAt:    time.Date(2026, 5, 16, 9, 0, 0, 0, time.UTC),
		},
		Session: &Session{
			ID:        uuid.New(),
			UserID:    userID,
			TokenHash: hashToken("raw-session-token"),
			ExpiresAt: time.Date(2026, 6, 15, 9, 0, 0, 0, time.UTC),
		},
		SessionToken: "raw-session-token",
	}
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

func staticUser(id uuid.UUID) UserIDFunc {
	return func(*http.Request) (uuid.UUID, bool) { return id, true }
}

// noUser is what a protected route sees when the middleware did not run — a
// wiring bug rather than an authentication failure.
func noUser(*http.Request) (uuid.UUID, bool) { return uuid.Nil, false }

func newTestServer(svc Service, userIDFrom UserIDFunc, opts ...HandlerOption) chi.Router {
	r := chi.NewRouter()
	h := NewHandler(svc, slog.New(slog.NewTextHandler(io.Discard, nil)), opts...)
	h.RegisterPublicRoutes(r)
	h.RegisterProtectedRoutes(r, userIDFrom)
	return r
}

func do(t *testing.T, r chi.Router, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func jsonRequest(method, path, body string) *http.Request {
	return httptest.NewRequest(method, path, bytes.NewBufferString(body))
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding response %q: %v", rec.Body.String(), err)
	}
	return out
}

func cookieNamed(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %q cookie was set; got %v", name, rec.Result().Cookies())
	return nil
}

type userEnvelope struct {
	User userResponse `json:"user"`
}

type sessionsEnvelope struct {
	Sessions []sessionResponse `json:"sessions"`
}

type tokensEnvelope struct {
	APITokens []apiTokenResponse `json:"api_tokens"`
}

// ---------------------------------------------------------------------------
// Register
// ---------------------------------------------------------------------------

func TestHandlerRegister_SetsSessionCookieAndReturnsUser(t *testing.T) {
	svc := &mockService{}
	mux := newTestServer(svc, staticUser(uuid.New()))

	body := `{"email":"someone@example.com","password":"correct-horse-battery","display_name":"Farouk"}`
	rec := do(t, mux, jsonRequest(http.MethodPost, "/auth/register", body))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if got := decode[userEnvelope](t, rec).User.Email; got != "someone@example.com" {
		t.Errorf("email = %q", got)
	}

	cookie := cookieNamed(t, rec, SessionCookieName)
	if cookie.Value != "raw-session-token" {
		t.Errorf("cookie value = %q, want the raw session token", cookie.Value)
	}
	if !cookie.HttpOnly {
		t.Error("the session cookie must be HttpOnly — that is what keeps an XSS bug from becoming a stolen session")
	}
	if !cookie.Secure {
		t.Error("Secure must default to on")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf(
			"SameSite = %v, want Lax so a link from an email or the extension still arrives authenticated",
			cookie.SameSite,
		)
	}
	if cookie.Path != "/" {
		t.Errorf("path = %q, want /", cookie.Path)
	}
}

// The cookie must outlive the session it points at, because the session's
// expiry slides forward in the database and a pinned cookie does not follow it.
// A cookie stamped with the expiry the session had at login logs out a daily
// user on day 30 with a live row still sitting in the table.
func TestHandlerRegister_CookieOutlivesTheSessionExpiry(t *testing.T) {
	mux := newTestServer(&mockService{}, staticUser(uuid.New()))

	rec := do(
		t, mux,
		jsonRequest(http.MethodPost, "/auth/register", `{"email":"a@b.com","password":"correct-horse-battery"}`),
	)

	cookie := cookieNamed(t, rec, SessionCookieName)
	if cookie.MaxAge <= int(DefaultSessionTTL/time.Second) {
		t.Errorf(
			"MaxAge = %d, want more than the %d-second session TTL so the database stays the sole authority on expiry",
			cookie.MaxAge, int(DefaultSessionTTL/time.Second),
		)
	}
	if !cookie.Expires.IsZero() {
		t.Error("Expires must not be set alongside MaxAge — the mock session's expiry is not the cookie's lifetime")
	}
}

// The domain user carries a password hash and the response type must not.
func TestHandlerRegister_ResponseNeverCarriesThePasswordHash(t *testing.T) {
	mux := newTestServer(&mockService{}, staticUser(uuid.New()))

	body := `{"email":"someone@example.com","password":"correct-horse-battery"}`
	rec := do(t, mux, jsonRequest(http.MethodPost, "/auth/register", body))

	if strings.Contains(rec.Body.String(), secretHash) {
		t.Fatal("the password hash reached the response body")
	}
	if strings.Contains(strings.ToLower(rec.Body.String()), "password") {
		t.Errorf("response mentions a password field: %s", rec.Body.String())
	}
}

func TestHandlerRegister_PassesUserAgentAndIPThrough(t *testing.T) {
	svc := &mockService{}
	mux := newTestServer(svc, staticUser(uuid.New()))

	req := jsonRequest(http.MethodPost, "/auth/register", `{"email":"a@b.com","password":"correct-horse-battery"}`)
	req.Header.Set("User-Agent", "Firefox/1.0")
	req.RemoteAddr = "203.0.113.7:54321"
	do(t, mux, req)

	if svc.lastRegister.UserAgent == nil || *svc.lastRegister.UserAgent != "Firefox/1.0" {
		t.Error("user agent did not reach the service")
	}
	if svc.lastRegister.IPAddress == nil || svc.lastRegister.IPAddress.String() != "203.0.113.7" {
		t.Errorf("ip = %v, want 203.0.113.7", svc.lastRegister.IPAddress)
	}
}

func TestHandlerRegister_SecureFlagCanBeTurnedOffForLocalHTTP(t *testing.T) {
	mux := newTestServer(&mockService{}, staticUser(uuid.New()), WithSecureCookies(false))

	rec := do(
		t, mux,
		jsonRequest(http.MethodPost, "/auth/register", `{"email":"a@b.com","password":"correct-horse-battery"}`),
	)

	if cookieNamed(t, rec, SessionCookieName).Secure {
		t.Error("WithSecureCookies(false) must clear the Secure flag, or local http login silently drops the cookie")
	}
}

func TestHandlerRegister_ServiceErrorsMapToStatuses(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		want     int
		wantCode string
	}{
		{"duplicate", ErrEmailTaken, http.StatusConflict, "email_taken"},
		{"missing email", ErrEmailRequired, http.StatusBadRequest, "email_required"},
		{"malformed email", ErrEmailInvalid, http.StatusBadRequest, "email_invalid"},
		{"short password", ErrPasswordTooShort, http.StatusBadRequest, "password_too_short"},
		// 400 rather than 500 specifically: this is the error a user earns by
		// taking "no composition rules, length is what matters" at its word.
		{"long password", ErrPasswordTooLong, http.StatusBadRequest, "password_too_long"},
	}

	for _, tc := range cases {
		t.Run(
			tc.name, func(t *testing.T) {
				svc := &mockService{
					register: func(context.Context, RegisterInput) (*AuthResult, error) { return nil, tc.err },
				}
				mux := newTestServer(svc, staticUser(uuid.New()))

				rec := do(
					t, mux,
					jsonRequest(http.MethodPost, "/auth/register", `{"email":"a@b.com","password":"x"}`),
				)
				if rec.Code != tc.want {
					t.Fatalf("status = %d, want %d", rec.Code, tc.want)
				}
				if got := decode[errorBody](t, rec).Error.Code; got != tc.wantCode {
					t.Errorf("code = %q, want %q", got, tc.wantCode)
				}
			},
		)
	}
}

func TestHandlerRegister_InvalidJSONIs400(t *testing.T) {
	mux := newTestServer(&mockService{}, staticUser(uuid.New()))
	rec := do(t, mux, jsonRequest(http.MethodPost, "/auth/register", `{`))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := decode[errorBody](t, rec).Error.Code; got != "invalid_body" {
		t.Errorf("code = %q, want invalid_body", got)
	}
}

// ---------------------------------------------------------------------------
// Login
// ---------------------------------------------------------------------------

func TestHandlerLogin_SetsCookie(t *testing.T) {
	mux := newTestServer(&mockService{}, staticUser(uuid.New()))

	rec := do(
		t, mux,
		jsonRequest(http.MethodPost, "/auth/login", `{"email":"a@b.com","password":"correct-horse-battery"}`),
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if cookieNamed(t, rec, SessionCookieName).Value != "raw-session-token" {
		t.Error("login must set the session cookie")
	}
}

// The service collapses "no such account" and "wrong password" into one error;
// the handler must not un-collapse them, so both produce a byte-identical
// response.
func TestHandlerLogin_BothFailureModesAreIdentical(t *testing.T) {
	svc := &mockService{
		login: func(context.Context, LoginInput) (*AuthResult, error) { return nil, ErrInvalidCredentials },
	}
	mux := newTestServer(svc, staticUser(uuid.New()))

	wrongPassword := do(
		t, mux,
		jsonRequest(http.MethodPost, "/auth/login", `{"email":"known@example.com","password":"wrong"}`),
	)
	unknownEmail := do(
		t, mux,
		jsonRequest(
			http.MethodPost,
			"/auth/login",
			`{"email":"nobody@example.com","password":"correct-horse-battery"}`,
		),
	)

	if wrongPassword.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", wrongPassword.Code)
	}
	if wrongPassword.Code != unknownEmail.Code {
		t.Errorf("statuses differ: %d vs %d", wrongPassword.Code, unknownEmail.Code)
	}
	if wrongPassword.Body.String() != unknownEmail.Body.String() {
		t.Errorf("bodies differ:\n%s\n%s", wrongPassword.Body.String(), unknownEmail.Body.String())
	}
	if got := decode[errorBody](t, wrongPassword).Error.Code; got != "invalid_credentials" {
		t.Errorf("code = %q, want invalid_credentials", got)
	}
}

func TestHandlerLogin_FailureSetsNoCookie(t *testing.T) {
	svc := &mockService{
		login: func(context.Context, LoginInput) (*AuthResult, error) { return nil, ErrInvalidCredentials },
	}
	mux := newTestServer(svc, staticUser(uuid.New()))

	rec := do(t, mux, jsonRequest(http.MethodPost, "/auth/login", `{"email":"a@b.com","password":"nope"}`))
	if len(rec.Result().Cookies()) != 0 {
		t.Error("a failed login must not set a session cookie")
	}
}

// ---------------------------------------------------------------------------
// Logout
// ---------------------------------------------------------------------------

func TestHandlerLogout_RevokesAndClearsTheCookie(t *testing.T) {
	svc := &mockService{}
	mux := newTestServer(svc, staticUser(uuid.New()))

	req := jsonRequest(http.MethodPost, "/auth/logout", "")
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "raw-session-token"})
	rec := do(t, mux, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if svc.lastLogoutToken != "raw-session-token" {
		t.Errorf("service received %q", svc.lastLogoutToken)
	}
	if cleared := cookieNamed(t, rec, SessionCookieName); cleared.MaxAge >= 0 {
		t.Errorf("MaxAge = %d, want a negative value to clear the cookie", cleared.MaxAge)
	}
}

// Always 204: the caller asked for the session to stop working, and afterwards
// it does not.
func TestHandlerLogout_WithoutACookieIsStill204(t *testing.T) {
	svc := &mockService{}
	mux := newTestServer(svc, staticUser(uuid.New()))

	rec := do(t, mux, jsonRequest(http.MethodPost, "/auth/logout", ""))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if svc.logoutCalls != 0 {
		t.Error("with no cookie there is nothing to revoke; the service should not be called")
	}
}

// ---------------------------------------------------------------------------
// Me and sessions
// ---------------------------------------------------------------------------

func TestHandlerMe_ReturnsTheUser(t *testing.T) {
	userID := uuid.New()
	mux := newTestServer(&mockService{}, staticUser(userID))

	rec := do(t, mux, httptest.NewRequest(http.MethodGet, "/auth/me", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := decode[userEnvelope](t, rec).User.ID; got != userID {
		t.Errorf("id = %s, want %s", got, userID)
	}
}

// No user on a protected route means the middleware did not run. That is a
// wiring bug, and answering 401 would send the user to debug their password.
func TestHandlerMe_MissingUserInContextIs500(t *testing.T) {
	mux := newTestServer(&mockService{}, noUser)

	rec := do(t, mux, httptest.NewRequest(http.MethodGet, "/auth/me", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestHandlerListSessions_MarksTheCallersOwnSession(t *testing.T) {
	userID := uuid.New()
	currentID, otherID := uuid.New(), uuid.New()
	svc := &mockService{
		listSess: func(context.Context, uuid.UUID) ([]Session, error) {
			return []Session{
				{ID: currentID, UserID: userID, TokenHash: hashToken("mine")},
				{ID: otherID, UserID: userID, TokenHash: hashToken("theirs")},
			}, nil
		},
	}
	mux := newTestServer(svc, staticUser(userID))

	req := httptest.NewRequest(http.MethodGet, "/auth/sessions", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "mine"})
	rec := do(t, mux, req)

	sessions := decode[sessionsEnvelope](t, rec).Sessions
	if len(sessions) != 2 {
		t.Fatalf("returned %d sessions, want 2", len(sessions))
	}
	for _, s := range sessions {
		wantCurrent := s.ID == currentID
		if s.Current != wantCurrent {
			t.Errorf("session %s current = %v, want %v", s.ID, s.Current, wantCurrent)
		}
	}
}

func TestHandlerListSessions_EmptySerialisesAsArray(t *testing.T) {
	mux := newTestServer(&mockService{}, staticUser(uuid.New()))

	rec := do(t, mux, httptest.NewRequest(http.MethodGet, "/auth/sessions", nil))
	if got := rec.Body.String(); got != "{\"sessions\":[]}\n" {
		t.Errorf("body = %q, want an empty array rather than null", got)
	}
}

func TestHandlerRevokeSession_BadUUIDIs400(t *testing.T) {
	mux := newTestServer(&mockService{}, staticUser(uuid.New()))

	rec := do(t, mux, httptest.NewRequest(http.MethodDelete, "/auth/sessions/not-a-uuid", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := decode[errorBody](t, rec).Error.Code; got != "invalid_session_id" {
		t.Errorf("code = %q", got)
	}
}

func TestHandlerRevokeSession_UnknownIs404(t *testing.T) {
	svc := &mockService{
		revokeSess: func(context.Context, uuid.UUID, uuid.UUID) error { return ErrSessionNotFound },
	}
	mux := newTestServer(svc, staticUser(uuid.New()))

	rec := do(t, mux, httptest.NewRequest(http.MethodDelete, "/auth/sessions/"+uuid.New().String(), nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// API tokens
// ---------------------------------------------------------------------------

func TestHandlerIssueToken_ReturnsTheRawValueOnceWithAWarning(t *testing.T) {
	mux := newTestServer(&mockService{}, staticUser(uuid.New()))

	rec := do(t, mux, jsonRequest(http.MethodPost, "/auth/tokens", `{"name":"extension"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	got := decode[issuedTokenResponse](t, rec)
	if got.Token != "raw-token-value" {
		t.Errorf("token = %q", got.Token)
	}
	if got.Warning == "" {
		t.Error("a value that cannot be recovered has to say so, or clients will not surface it")
	}
	if got.Details.Name != "extension" {
		t.Errorf("name = %q", got.Details.Name)
	}
}

func TestHandlerIssueToken_ReadOnlyDefaultsToFalseAndIsForwarded(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"omitted", `{"name":"extension"}`, false},
		{"explicit false", `{"name":"extension","read_only":false}`, false},
		{"explicit true", `{"name":"public demo","read_only":true}`, true},
	}

	for _, tc := range cases {
		t.Run(
			tc.name, func(t *testing.T) {
				svc := &mockService{}
				mux := newTestServer(svc, staticUser(uuid.New()))

				rec := do(t, mux, jsonRequest(http.MethodPost, "/auth/tokens", tc.body))
				if rec.Code != http.StatusCreated {
					t.Fatalf("status = %d, want 201", rec.Code)
				}
				if svc.lastTokenReadOn != tc.want {
					t.Errorf("read_only reached the service as %v, want %v", svc.lastTokenReadOn, tc.want)
				}
				if got := decode[issuedTokenResponse](t, rec).Details.ReadOnly; got != tc.want {
					t.Errorf("read_only in the response = %v, want %v", got, tc.want)
				}
			},
		)
	}
}

func TestHandlerIssueToken_EmptyNameIs400(t *testing.T) {
	svc := &mockService{
		issueToken: func(context.Context, uuid.UUID, string, bool) (*IssuedToken, error) {
			return nil, ErrTokenNameEmpty
		},
	}
	mux := newTestServer(svc, staticUser(uuid.New()))

	rec := do(t, mux, jsonRequest(http.MethodPost, "/auth/tokens", `{"name":"  "}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := decode[errorBody](t, rec).Error.Code; got != "name_required" {
		t.Errorf("code = %q", got)
	}
}

// A user looking at two tokens needs to know which one can write.
func TestHandlerListTokens_ReportsReadOnly(t *testing.T) {
	userID := uuid.New()
	svc := &mockService{
		listTokens: func(context.Context, uuid.UUID) ([]APIToken, error) {
			return []APIToken{
				{ID: uuid.New(), UserID: userID, Name: "extension", IsReadOnly: false},
				{ID: uuid.New(), UserID: userID, Name: "public demo", IsReadOnly: true},
			}, nil
		},
	}
	mux := newTestServer(svc, staticUser(userID))

	rec := do(t, mux, httptest.NewRequest(http.MethodGet, "/auth/tokens", nil))
	tokens := decode[tokensEnvelope](t, rec).APITokens
	if len(tokens) != 2 {
		t.Fatalf("returned %d tokens, want 2", len(tokens))
	}
	if tokens[0].ReadOnly || !tokens[1].ReadOnly {
		t.Errorf("read_only flags = %v, %v", tokens[0].ReadOnly, tokens[1].ReadOnly)
	}
}

// No listing may carry a token hash: it is not secret enough to be interesting
// and not useful enough to publish.
func TestHandlerListTokens_NeverExposesTheHash(t *testing.T) {
	hash := hashToken("something")
	svc := &mockService{
		listTokens: func(context.Context, uuid.UUID) ([]APIToken, error) {
			return []APIToken{{ID: uuid.New(), Name: "extension", TokenHash: hash}}, nil
		},
	}
	mux := newTestServer(svc, staticUser(uuid.New()))

	rec := do(t, mux, httptest.NewRequest(http.MethodGet, "/auth/tokens", nil))
	if strings.Contains(rec.Body.String(), hash) {
		t.Error("the token hash reached the response body")
	}
}

func TestHandlerRevokeToken_HappyPathAndNotFound(t *testing.T) {
	mux := newTestServer(&mockService{}, staticUser(uuid.New()))
	rec := do(t, mux, httptest.NewRequest(http.MethodDelete, "/auth/tokens/"+uuid.New().String(), nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}

	missing := &mockService{
		revokeToken: func(context.Context, uuid.UUID, uuid.UUID) error { return ErrTokenNotFound },
	}
	rec = do(
		t, newTestServer(missing, staticUser(uuid.New())),
		httptest.NewRequest(http.MethodDelete, "/auth/tokens/"+uuid.New().String(), nil),
	)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestHandlerRevokeToken_BadUUIDIs400(t *testing.T) {
	mux := newTestServer(&mockService{}, staticUser(uuid.New()))

	rec := do(t, mux, httptest.NewRequest(http.MethodDelete, "/auth/tokens/nope", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := decode[errorBody](t, rec).Error.Code; got != "invalid_token_id" {
		t.Errorf("code = %q", got)
	}
}
