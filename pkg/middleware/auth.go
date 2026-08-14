// Package middleware holds Chi middleware shared across the API Lambda.
//
// Two authentication middlewares live here, and only one of them is meant to
// survive:
//
//   - RequireAuth resolves a real user from a session cookie (dashboard) or a
//     bearer API token (extension, and the dashboard's server-side demo route).
//     Everything written from phase 14 onwards sits behind this.
//
//   - LegacyStaticKeyAuth is the MVP's single shared key. It is kept alive for
//     exactly one phase, because the modules behind it still ignore user_id:
//     moving them onto RequireAuth now would authenticate a user and then hand
//     them everybody's rows, which is worse than the shared key. Phase 15
//     deletes it in the same change that makes those queries user-aware, and
//     the name is deliberately awkward so that every call site reads as
//     temporary until then.
package middleware

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/EngenMe/applymind-backend/internal/auth"
)

const bearerPrefix = "Bearer "

// ---------------------------------------------------------------------------
// Legacy: single static API key
// ---------------------------------------------------------------------------

// LegacyStaticKeyAuth returns Chi-compatible middleware that requires an exact
// "Authorization: Bearer <key>" header matching expectedKey. Comparison uses
// subtle.ConstantTimeCompare so response latency cannot be used to guess the
// key one byte at a time.
//
// expectedKey must be non-empty; an empty key would make every request with
// a missing header pass (empty == empty), which is never correct.
//
// This was APIKeyAuth. It identifies nobody, cannot be revoked, and is shared
// by every caller. Phase 15 removes it along with the last route that uses it;
// nothing new should be put behind it.
func LegacyStaticKeyAuth(expectedKey string) func(http.Handler) http.Handler {
	if expectedKey == "" {
		panic("middleware: LegacyStaticKeyAuth requires a non-empty expectedKey")
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				header := r.Header.Get("Authorization")

				if !strings.HasPrefix(header, bearerPrefix) {
					unauthorized(w)
					return
				}

				presented := strings.TrimPrefix(header, bearerPrefix)

				// ConstantTimeCompare requires equal-length inputs to give its
				// timing guarantee; unequal lengths already fail comparison
				// safely, but we still route both cases through it rather than
				// branching on length first, to avoid a length-based timing
				// side channel of our own making.
				match := subtle.ConstantTimeCompare([]byte(presented), []byte(expectedKey)) == 1
				if !match {
					unauthorized(w)
					return
				}

				next.ServeHTTP(w, r)
			},
		)
	}
}

// ---------------------------------------------------------------------------
// Request context
// ---------------------------------------------------------------------------

// identityContextKey is an unexported type so nothing outside this package can
// write the authenticated identity into a context. A handler reads it through
// UserID and cannot forge it.
type identityContextKey struct{}

// UserID returns the authenticated user from a request context.
//
// The bool is false when RequireAuth did not run. For a handler mounted behind
// RequireAuth that is a wiring bug rather than an authentication failure, and
// the auth handler treats it as one — a quiet 401 there would look like a user
// problem and send somebody debugging their password.
func UserID(ctx context.Context) (uuid.UUID, bool) {
	identity, ok := IdentityFrom(ctx)
	if !ok {
		return uuid.Nil, false
	}
	return identity.UserID, true
}

// IdentityFrom returns the whole resolved identity: which credential proved it,
// and whether that credential may write. Handlers want UserID; this exists for
// the places that need to know how the caller got here.
func IdentityFrom(ctx context.Context) (*auth.Identity, bool) {
	identity, ok := ctx.Value(identityContextKey{}).(*auth.Identity)
	if !ok || identity == nil {
		return nil, false
	}
	return identity, true
}

// UserIDFromRequest adapts UserID to auth.UserIDFunc, which the auth handler
// takes so that the auth module does not have to import this package — this
// package already imports it, and the cycle would be immediate.
func UserIDFromRequest(r *http.Request) (uuid.UUID, bool) {
	return UserID(r.Context())
}

// ---------------------------------------------------------------------------
// Real authentication
// ---------------------------------------------------------------------------

// errNoCredential is "this request presented nothing to authenticate with",
// kept distinct from a credential that was presented and rejected. The
// difference reaches the log and stops there; the response is identical.
var errNoCredential = errors.New("middleware: no credential presented")

// RequireAuth resolves the caller and rejects anyone it cannot.
//
// Cookie first, then bearer. A request carrying both is a dashboard request:
// the cookie wins, and — importantly — a cookie that fails to resolve is not
// retried against the bearer token. Falling through would mean an expired
// dashboard session silently continuing as whoever the bearer token belongs to,
// which is a quiet identity switch rather than the re-login the user needs.
//
// There is no constant-time comparison in here, and its absence is not an
// oversight. Nothing in this path compares a presented secret against a stored
// one in Go: the raw credential is SHA-256'd and the digest is used as a lookup
// key, so the only equality test is the database's, against 256 bits of
// crypto/rand entropy. A timing signal on that leaks nothing guessable. The
// discipline still applies in LegacyStaticKeyAuth above, which does hold a
// secret in memory and does compare it.
func RequireAuth(svc auth.Service, logger *slog.Logger) func(http.Handler) http.Handler {
	if svc == nil {
		panic("middleware: RequireAuth requires an auth.Service")
	}
	if logger == nil {
		logger = slog.Default()
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				identity, err := resolveIdentity(r, svc)
				if err != nil {
					// A credential failure is a 401. Anything else — the
					// database being unreachable, most likely — is not: telling
					// a user their session is invalid because Neon blinked
					// sends them to re-authenticate against a system that
					// cannot authenticate them, and logs them out of a session
					// that was fine.
					switch {
					case errors.Is(err, errNoCredential),
						errors.Is(err, auth.ErrSessionInvalid),
						errors.Is(err, auth.ErrTokenInvalid):
						logger.InfoContext(
							r.Context(), "auth: request rejected",
							slog.String("path", r.URL.Path),
							slog.String("reason", rejectionReason(err)),
						)
						unauthorized(w)
					default:
						logger.ErrorContext(
							r.Context(), "auth: could not resolve credential",
							slog.String("path", r.URL.Path),
							slog.String("error", err.Error()),
						)
						internalError(w)
					}
					return
				}

				// A read-only token may read and nothing else. Enforced here,
				// once, rather than in each handler: an endpoint added next
				// year is covered without anybody remembering to cover it,
				// which is the opposite of how per-handler checks age.
				//
				// 403 rather than 401 because the credential is valid. A 401
				// invites the client to authenticate again, and doing so would
				// change nothing.
				if identity.IsReadOnly && !isSafeMethod(r.Method) {
					logger.InfoContext(
						r.Context(), "auth: read-only token attempted a write",
						slog.String("path", r.URL.Path),
						slog.String("method", r.Method),
						slog.String("user_id", identity.UserID.String()),
					)
					readOnlyForbidden(w)
					return
				}

				ctx := context.WithValue(r.Context(), identityContextKey{}, identity)
				next.ServeHTTP(w, r.WithContext(ctx))
			},
		)
	}
}

// resolveIdentity picks the credential and hands it to the service.
func resolveIdentity(r *http.Request, svc auth.Service) (*auth.Identity, error) {
	if cookie, err := r.Cookie(auth.SessionCookieName); err == nil && cookie.Value != "" {
		return svc.AuthenticateSession(r.Context(), cookie.Value)
	}

	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, bearerPrefix) {
		return nil, errNoCredential
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, bearerPrefix))
	if token == "" {
		return nil, errNoCredential
	}
	return svc.AuthenticateAPIToken(r.Context(), token)
}

// isSafeMethod reports whether a read-only credential may make this request.
//
// An explicit allow-list, not a deny-list of the write verbs: a method nobody
// thought of is refused rather than permitted.
func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

// CheckReadOnlyWrite reports whether this request is a write attempt by a
// read-only credential, resolving the caller exactly as RequireAuth does.
//
// It exists for exactly one caller: the router's MethodNotAllowedHandler. chi
// bakes RequireAuth onto each handler individually when it is added inside an
// inline r.Group — a method with no registered handler anywhere (this API has
// no PUT route at all, for instance) never reaches that middleware, and chi
// answers 405 straight out of its routing tree first. Without this, a
// read-only token probing unsupported methods gets a bare 405 whose Allow
// header discloses which methods do exist at that path — more than a
// restricted credential should learn — instead of the same 403
// read_only_token a supported write gets.
//
// A request that fails to authenticate at all is not this function's concern:
// false is returned and the caller falls back to chi's ordinary 405, which is
// exactly what an unauthenticated or fully-authenticated caller hitting the
// same unsupported method already gets. Only a request that resolves to a
// genuine read-only credential changes behaviour here.
func CheckReadOnlyWrite(r *http.Request, svc auth.Service) bool {
	identity, err := resolveIdentity(r, svc)
	if err != nil {
		return false
	}
	return identity.IsReadOnly && !isSafeMethod(r.Method)
}

// rejectionReason is for the log only. Expired and revoked are deliberately not
// separable here — FindLiveSession filters both in SQL, so by the time the
// middleware sees a failure there is one answer. See the phase notes.
func rejectionReason(err error) string {
	switch {
	case errors.Is(err, errNoCredential):
		return "no_credential"
	case errors.Is(err, auth.ErrSessionInvalid):
		return "session_invalid"
	case errors.Is(err, auth.ErrTokenInvalid):
		return "token_invalid"
	default:
		return "unknown"
	}
}

// ---------------------------------------------------------------------------
// Responses
// ---------------------------------------------------------------------------

// The envelope here is flat — {"error":"unauthorized"} — and stays that way
// because clients already parse it. Note that it is not the modules' nested
// {"error":{"code","message"}} shape; reconciling the two is a job for the
// phase that touches the dashboard's error handling, not this one.
func unauthorized(w http.ResponseWriter) {
	writeAuthError(w, http.StatusUnauthorized, "unauthorized")
}

func readOnlyForbidden(w http.ResponseWriter) {
	writeAuthError(w, http.StatusForbidden, "read_only_token")
}

func internalError(w http.ResponseWriter) {
	writeAuthError(w, http.StatusInternalServerError, "internal_error")
}

func writeAuthError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(
		map[string]string{
			"error": code,
		},
	)
}
