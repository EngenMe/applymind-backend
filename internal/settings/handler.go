package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// Handler exposes the settings module over HTTP via chi, matching the router set
// up in cmd/api/main.go. Register it inside the API-key-protected group:
//
//	settings.NewHandler(settingsSvc, logger).RegisterRoutes(protected)
type Handler struct {
	svc    Service
	logger *slog.Logger
}

func NewHandler(svc Service, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{svc: svc, logger: logger}
}

func (h *Handler) RegisterRoutes(r chi.Router) {
	r.Get("/settings/profile-summary", h.GetProfileSummary)
	r.Put("/settings/profile-summary", h.SetProfileSummary)
}

// ---------------------------------------------------------------------------
// Request / response DTOs
// ---------------------------------------------------------------------------

type profileSummaryRequest struct {
	ProfileSummary string `json:"profile_summary"`
}

// profileSummaryResponse keeps both fields nullable so an unset summary is a
// 200 with nulls rather than a 404. The settings resource always exists; the
// value inside it is what may be missing, and the dashboard can render that
// without treating it as an error.
type profileSummaryResponse struct {
	ProfileSummary *string    `json:"profile_summary"`
	UpdatedAt      *time.Time `json:"updated_at"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// GetProfileSummary — GET /settings/profile-summary
func (h *Handler) GetProfileSummary(w http.ResponseWriter, r *http.Request) {
	current, err := h.svc.Get(r.Context())
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	h.writeJSON(w, http.StatusOK, toResponse(*current))
}

// SetProfileSummary — PUT /settings/profile-summary
// Body: {"profile_summary": "Backend engineer with six years of Go..."}
//
// The summary is what the AI job score is calculated against; until one is set,
// applications save without a score.
func (h *Handler) SetProfileSummary(w http.ResponseWriter, r *http.Request) {
	var req profileSummaryRequest
	if !h.decode(w, r, &req) {
		return
	}

	updated, err := h.svc.SetProfileSummary(r.Context(), req.ProfileSummary)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	h.writeJSON(w, http.StatusOK, toResponse(*updated))
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func toResponse(s Settings) profileSummaryResponse {
	out := profileSummaryResponse{ProfileSummary: s.ProfileSummary}
	if !s.UpdatedAt.IsZero() {
		at := s.UpdatedAt
		out.UpdatedAt = &at
	}
	return out
}

func (h *Handler) decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	// A few sentences, so the cap is small — the validation error for a long
	// summary is friendlier than a truncated read.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(dst); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_body", "request body must be valid JSON")
		return false
	}
	return true
}

// errorBody mirrors the envelope used by the other modules.
type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (h *Handler) writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrSummaryRequired):
		h.writeError(w, http.StatusBadRequest, "profile_summary_required", "profile_summary is required")
	case errors.Is(err, ErrSummaryTooLong):
		h.writeError(
			w, http.StatusBadRequest, "profile_summary_too_long",
			fmt.Sprintf("profile_summary must be at most %d characters", MaxProfileSummaryLength),
		)
	default:
		h.logger.ErrorContext(
			r.Context(), "settings handler failure",
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
		h.logger.Error("settings: encode response", slog.String("error", err.Error()))
	}
}
