package auth

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Handler exposes the auth module over HTTP via chi.
//
// Unlike the other modules this one registers in two places: /auth/register and
// /auth/login are public, everything else needs an authenticated user. See
// RegisterPublicRoutes and RegisterProtectedRoutes.
type Handler struct {
	svc    Service
	logger *slog.Logger

	// secureCookies controls the Secure flag. Off for local http development,
	// on everywhere else. A Secure cookie is silently dropped over plain http,
	// which presents as "login succeeds but I am immediately logged out" — so
	// this is worth a flag rather than a constant.
	secureCookies bool
}

// SessionCookieName is the cookie the dashboard authenticates with. Exported
// because the middleware reads it too.
const SessionCookieName = "applymind_session"

// SessionCookieMaxAge is how long the browser is asked to keep the cookie, and
// it is deliberately far longer than DefaultSessionTTL rather than equal to it.
//
// The session's expiry slides forward in the database every time it is used. A
// cookie pinned to the expiry the session had at login does not slide, so after
// thirty days the browser discards a cookie whose row is still perfectly alive:
// somebody who opens the dashboard every single day is logged out on day 30,
// which is the exact outcome the sliding window exists to prevent.
//
// The alternative fix is to re-issue the cookie whenever the session is
// extended, which means the middleware writing Set-Cookie onto arbitrary
// responses, including ones the extension makes and ignores. This is simpler
// and keeps expiry in one place: the database decides when a session ends, and
// the cookie's only job is to outlive that decision. A cookie left over from a
// dead session is harmless — it fails its lookup and gets a 401.
//
// 400 days because that is the ceiling Chrome silently clamps to; asking for
// more just gets less, quietly.
const SessionCookieMaxAge = 400 * 24 * time.Hour

type HandlerOption func(*Handler)

// WithSecureCookies sets the Secure flag on the session cookie.
func WithSecureCookies(secure bool) HandlerOption {
	return func(h *Handler) { h.secureCookies = secure }
}

func NewHandler(svc Service, logger *slog.Logger, opts ...HandlerOption) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	h := &Handler{svc: svc, logger: logger, secureCookies: true}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// RegisterPublicRoutes mounts the two endpoints that cannot require a session,
// because they are how a session is obtained.
func (h *Handler) RegisterPublicRoutes(r chi.Router) {
	r.Post("/auth/register", h.Register)
	r.Post("/auth/login", h.Login)
}

// RegisterProtectedRoutes mounts everything that needs an authenticated user.
//
// Logout is here rather than in the public group deliberately: logging out
// without being logged in is not a meaningful request, and putting it behind
// the middleware means the handler can trust that a session exists.
func (h *Handler) RegisterProtectedRoutes(r chi.Router, userIDFrom UserIDFunc) {
	r.Post("/auth/logout", h.Logout)
	r.Get("/auth/me", h.Me(userIDFrom))
	r.Get("/auth/sessions", h.ListSessions(userIDFrom))
	r.Delete("/auth/sessions/{id}", h.RevokeSession(userIDFrom))
	r.Post("/auth/tokens", h.IssueToken(userIDFrom))
	r.Get("/auth/tokens", h.ListTokens(userIDFrom))
	r.Delete("/auth/tokens/{id}", h.RevokeToken(userIDFrom))
}

// UserIDFunc extracts the authenticated user from a request context.
//
// Passed in rather than imported so this module does not depend on
// pkg/middleware, which depends on this module for the Service it calls. The
// alternative is an import cycle.
type UserIDFunc func(r *http.Request) (uuid.UUID, bool)

// ---------------------------------------------------------------------------
// Request / response DTOs
// ---------------------------------------------------------------------------

type registerRequest struct {
	Email       string  `json:"email"`
	Password    string  `json:"password"`
	DisplayName *string `json:"display_name"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type tokenRequest struct {
	Name string `json:"name"`
	// Defaults to false when omitted, which is the safe direction: a token
	// nobody asked to restrict is a normal one, and a restricted token is
	// always a deliberate choice.
	ReadOnly bool `json:"read_only"`
}

// userResponse is the public shape of a user. PasswordHash is absent, and its
// absence is the point — the domain type carries it and this one must not.
type userResponse struct {
	ID          uuid.UUID  `json:"id"`
	Email       string     `json:"email"`
	DisplayName *string    `json:"display_name"`
	CreatedAt   time.Time  `json:"created_at"`
	VerifiedAt  *time.Time `json:"email_verified_at"`
}

type sessionResponse struct {
	ID        uuid.UUID `json:"id"`
	UserAgent *string   `json:"user_agent"`
	IPAddress *string   `json:"ip_address"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
	Current   bool      `json:"current"`
}

type apiTokenResponse struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	// Reported so the settings page can label a token honestly. A user looking
	// at two tokens needs to know which one can write.
	ReadOnly   bool       `json:"read_only"`
	LastUsedAt *time.Time `json:"last_used_at"`
	CreatedAt  time.Time  `json:"created_at"`
	// There is no revoked_at here on purpose. ListActiveAPITokens filters
	// revoked rows out in SQL, so the field could only ever serialise as null —
	// a client would reasonably read that as "no token has ever been revoked".
	// Showing revoked tokens as an audit trail is a real feature and a better
	// one than a permanently-null field, but it needs the query to return them;
	// until it does, the honest response is to omit it.
}

// issuedTokenResponse is the only response that ever carries a raw token. The
// warning field is not decoration: this value cannot be recovered, and a client
// that does not surface that will produce users who lose it.
type issuedTokenResponse struct {
	Token   string           `json:"token"`
	Warning string           `json:"warning"`
	Details apiTokenResponse `json:"api_token"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// Register — POST /auth/register
func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if !h.decode(w, r, &req) {
		return
	}

	result, err := h.svc.Register(
		r.Context(), RegisterInput{
			Email:       req.Email,
			Password:    req.Password,
			DisplayName: req.DisplayName,
			UserAgent:   userAgentOf(r),
			IPAddress:   ipOf(r),
		},
	)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}

	h.setSessionCookie(w, result.SessionToken)
	h.writeJSON(w, http.StatusCreated, map[string]any{"user": toUserResponse(*result.User)})
}

// Login — POST /auth/login
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !h.decode(w, r, &req) {
		return
	}

	result, err := h.svc.Login(
		r.Context(), LoginInput{
			Email:     req.Email,
			Password:  req.Password,
			UserAgent: userAgentOf(r),
			IPAddress: ipOf(r),
		},
	)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}

	h.setSessionCookie(w, result.SessionToken)
	h.writeJSON(w, http.StatusOK, map[string]any{"user": toUserResponse(*result.User)})
}

// Logout — POST /auth/logout
//
// Always 204, even if there was nothing to revoke. The caller asked for the
// session to stop working; afterwards it does not.
func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(SessionCookieName); err == nil {
		if err := h.svc.Logout(r.Context(), cookie.Value); err != nil {
			h.logger.ErrorContext(r.Context(), "auth: logout failed", slog.String("error", err.Error()))
		}
	}
	h.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// Me — GET /auth/me
func (h *Handler) Me(userIDFrom UserIDFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := h.requireUser(w, r, userIDFrom)
		if !ok {
			return
		}
		user, err := h.svc.GetUser(r.Context(), userID)
		if err != nil {
			h.writeServiceError(w, r, err)
			return
		}
		h.writeJSON(w, http.StatusOK, map[string]any{"user": toUserResponse(*user)})
	}
}

// ListSessions — GET /auth/sessions
func (h *Handler) ListSessions(userIDFrom UserIDFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := h.requireUser(w, r, userIDFrom)
		if !ok {
			return
		}
		sessions, err := h.svc.ListSessions(r.Context(), userID)
		if err != nil {
			h.writeServiceError(w, r, err)
			return
		}

		// Marking the caller's own session lets a client warn before someone
		// revokes the session they are sitting in.
		var currentHash string
		if cookie, err := r.Cookie(SessionCookieName); err == nil {
			currentHash = hashToken(cookie.Value)
		}

		out := make([]sessionResponse, 0, len(sessions))
		for _, session := range sessions {
			out = append(out, toSessionResponse(session, currentHash))
		}
		h.writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
	}
}

// RevokeSession — DELETE /auth/sessions/{id}
func (h *Handler) RevokeSession(userIDFrom UserIDFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := h.requireUser(w, r, userIDFrom)
		if !ok {
			return
		}
		sessionID, ok := h.parseUUIDParam(w, r, "id", "invalid_session_id")
		if !ok {
			return
		}
		if err := h.svc.RevokeSession(r.Context(), userID, sessionID); err != nil {
			h.writeServiceError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// IssueToken — POST /auth/tokens
func (h *Handler) IssueToken(userIDFrom UserIDFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := h.requireUser(w, r, userIDFrom)
		if !ok {
			return
		}
		var req tokenRequest
		if !h.decode(w, r, &req) {
			return
		}

		issued, err := h.svc.IssueAPIToken(r.Context(), userID, req.Name, req.ReadOnly)
		if err != nil {
			h.writeServiceError(w, r, err)
			return
		}

		h.writeJSON(
			w, http.StatusCreated, issuedTokenResponse{
				Token:   issued.Token,
				Warning: "This token is shown once and cannot be retrieved again. Store it now.",
				Details: toAPITokenResponse(*issued.APIToken),
			},
		)
	}
}

// ListTokens — GET /auth/tokens
func (h *Handler) ListTokens(userIDFrom UserIDFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := h.requireUser(w, r, userIDFrom)
		if !ok {
			return
		}
		tokens, err := h.svc.ListAPITokens(r.Context(), userID)
		if err != nil {
			h.writeServiceError(w, r, err)
			return
		}
		out := make([]apiTokenResponse, 0, len(tokens))
		for _, token := range tokens {
			out = append(out, toAPITokenResponse(token))
		}
		h.writeJSON(w, http.StatusOK, map[string]any{"api_tokens": out})
	}
}

// RevokeToken — DELETE /auth/tokens/{id}
func (h *Handler) RevokeToken(userIDFrom UserIDFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := h.requireUser(w, r, userIDFrom)
		if !ok {
			return
		}
		tokenID, ok := h.parseUUIDParam(w, r, "id", "invalid_token_id")
		if !ok {
			return
		}
		if err := h.svc.RevokeAPIToken(r.Context(), userID, tokenID); err != nil {
			h.writeServiceError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// ---------------------------------------------------------------------------
// Cookies
// ---------------------------------------------------------------------------

// setSessionCookie writes the session cookie.
//
// HttpOnly so page JavaScript cannot read it, which is what makes an XSS bug a
// smaller problem than it would otherwise be. SameSite=Lax rather than Strict
// because Strict withholds the cookie on any cross-site navigation — including
// following a link into the dashboard from the extension — and arriving
// logged-out from your own link is a worse failure than the narrow CSRF surface
// Lax leaves open on top-level GETs.
//
// The lifetime is deliberately not the session's expiry. See
// SessionCookieMaxAge.
func (h *Handler) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(
		w, &http.Cookie{
			Name:     SessionCookieName,
			Value:    token,
			Path:     "/",
			MaxAge:   int(SessionCookieMaxAge / time.Second),
			HttpOnly: true,
			Secure:   h.secureCookies,
			SameSite: http.SameSiteLaxMode,
		},
	)
}

func (h *Handler) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(
		w, &http.Cookie{
			Name:     SessionCookieName,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			HttpOnly: true,
			Secure:   h.secureCookies,
			SameSite: http.SameSiteLaxMode,
		},
	)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// requireUser reads the authenticated user, or fails the request.
//
// Reaching this with no user is a wiring bug, not an authentication failure:
// the middleware has already run and would have rejected the request itself.
// So it is a 500 and it is logged, rather than a quiet 401 that would look like
// a user problem.
func (h *Handler) requireUser(w http.ResponseWriter, r *http.Request, from UserIDFunc) (uuid.UUID, bool) {
	userID, ok := from(r)
	if !ok {
		h.logger.ErrorContext(
			r.Context(), "auth: no user in context on a protected route",
			slog.String("path", r.URL.Path),
		)
		h.writeError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return uuid.Nil, false
	}
	return userID, true
}

func (h *Handler) decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(dst); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_body", "request body must be valid JSON")
		return false
	}
	return true
}

func (h *Handler) parseUUIDParam(w http.ResponseWriter, r *http.Request, name, code string) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		h.writeError(w, http.StatusBadRequest, code, "path parameter must be a UUID")
		return uuid.Nil, false
	}
	return id, true
}

func userAgentOf(r *http.Request) *string {
	ua := strings.TrimSpace(r.UserAgent())
	if ua == "" {
		return nil
	}
	// Long enough to identify a browser, short enough that a hostile client
	// cannot use the field as storage.
	if len(ua) > 400 {
		ua = ua[:400]
	}
	return &ua
}

// ipOf reads the client address. chi's RealIP middleware has already resolved
// X-Forwarded-For by the time this runs.
func ipOf(r *http.Request) *netip.Addr {
	host := r.RemoteAddr
	if idx := strings.LastIndex(host, ":"); idx != -1 && strings.Count(host, ":") == 1 {
		host = host[:idx]
	}
	if addrPort, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		addr := addrPort.Addr()
		return &addr
	}
	addr, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return nil
	}
	return &addr
}

func toUserResponse(user User) userResponse {
	return userResponse{
		ID:          user.ID,
		Email:       user.Email,
		DisplayName: user.DisplayName,
		CreatedAt:   user.CreatedAt,
		VerifiedAt:  user.EmailVerifiedAt,
	}
}

func toSessionResponse(session Session, currentHash string) sessionResponse {
	out := sessionResponse{
		ID:        session.ID,
		UserAgent: session.UserAgent,
		ExpiresAt: session.ExpiresAt,
		CreatedAt: session.CreatedAt,
		Current:   currentHash != "" && session.TokenHash == currentHash,
	}
	if session.IPAddress != nil {
		ip := session.IPAddress.String()
		out.IPAddress = &ip
	}
	return out
}

func toAPITokenResponse(token APIToken) apiTokenResponse {
	return apiTokenResponse{
		ID:         token.ID,
		Name:       token.Name,
		ReadOnly:   token.IsReadOnly,
		LastUsedAt: token.LastUsedAt,
		CreatedAt:  token.CreatedAt,
	}
}

// errorBody mirrors the envelope used by every other module.
type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (h *Handler) writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrInvalidCredentials):
		// 401 with one message for both causes. The service already collapses
		// them; this must not un-collapse them by accident.
		h.writeError(w, http.StatusUnauthorized, "invalid_credentials", "email or password is incorrect")
	case errors.Is(err, ErrEmailTaken):
		h.writeError(w, http.StatusConflict, "email_taken", "an account with that email already exists")
	case errors.Is(err, ErrEmailRequired):
		h.writeError(w, http.StatusBadRequest, "email_required", "email is required")
	case errors.Is(err, ErrEmailInvalid):
		h.writeError(w, http.StatusBadRequest, "email_invalid", "that is not a valid email address")
	case errors.Is(err, ErrPasswordTooShort):
		h.writeError(
			w, http.StatusBadRequest, "password_too_short",
			"password must be at least 12 characters",
		)
	case errors.Is(err, ErrPasswordTooLong):
		// 400, not 500. A user who read "longer is better" and pasted a
		// passphrase deserves to be told the limit, not handed an internal
		// error for doing the thing the minimum encouraged.
		h.writeError(
			w, http.StatusBadRequest, "password_too_long",
			"password must be at most 72 bytes",
		)
	case errors.Is(err, ErrTokenNameEmpty):
		h.writeError(w, http.StatusBadRequest, "name_required", "name is required")
	case errors.Is(err, ErrUserNotFound):
		h.writeError(w, http.StatusNotFound, "user_not_found", "no user with that id")
	case errors.Is(err, ErrSessionNotFound):
		h.writeError(w, http.StatusNotFound, "session_not_found", "no session with that id")
	case errors.Is(err, ErrTokenNotFound):
		h.writeError(w, http.StatusNotFound, "token_not_found", "no api token with that id")
	case errors.Is(err, ErrSessionInvalid), errors.Is(err, ErrTokenInvalid):
		h.writeError(w, http.StatusUnauthorized, "unauthorized", "not authenticated")
	default:
		h.logger.ErrorContext(
			r.Context(), "auth handler failure",
			slog.String("path", r.URL.Path), slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
	}
}

func (h *Handler) writeError(w http.ResponseWriter, status int, code, message string) {
	var body errorBody
	body.Error.Code = code
	body.Error.Message = message
	h.writeJSON(w, status, body)
}

func (h *Handler) writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		h.logger.Error("auth: encode response", slog.String("error", err.Error()))
	}
}
