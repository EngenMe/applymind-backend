package sites

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Handler exposes the sites module over HTTP via chi, matching the router set up
// in cmd/api/main.go. Register it inside the API-key-protected group:
//
//	sites.NewHandler(siteSvc, logger).RegisterRoutes(protected)
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
	r.Get("/sites", h.List)
	r.Post("/sites", h.Add)
	r.Patch("/sites/{id}/toggle", h.ToggleActive)
	r.Delete("/sites/{id}", h.Delete)
}

// pathParam is the single point of coupling to the router.
func (h *Handler) pathParam(r *http.Request, name string) string {
	return chi.URLParam(r, name)
}

// ---------------------------------------------------------------------------
// Request / response DTOs
// ---------------------------------------------------------------------------

type addRequest struct {
	Name   string `json:"name"`
	Domain string `json:"domain"`
}

// siteResponse exposes selectors read-only. Nothing in this phase writes it, and
// POST /sites ignores it if a client sends one.
type siteResponse struct {
	ID              uuid.UUID       `json:"id"`
	Name            string          `json:"name"`
	Domain          string          `json:"domain"`
	IsPreconfigured bool            `json:"is_preconfigured"`
	IsActive        bool            `json:"is_active"`
	Selectors       json.RawMessage `json:"selectors"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// List — GET /sites
//
// Returns every site, pre-configured and custom, which is what the dashboard
// settings page needs. ?active=true narrows it to what the extension may
// currently capture from.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	var filter ListFilter
	if raw := r.URL.Query().Get("active"); raw != "" {
		active, err := strconv.ParseBool(raw)
		if err != nil {
			h.writeError(w, http.StatusBadRequest, "invalid_active", "active must be true or false")
			return
		}
		filter.ActiveOnly = active
	}

	sites, err := h.svc.List(r.Context(), filter)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"sites": toResponses(sites)})
}

// Add — POST /sites
//
// Adds a user's own site. domain may be a bare host or a full URL; it is stored
// normalised. The new site is custom (deletable) and active.
func (h *Handler) Add(w http.ResponseWriter, r *http.Request) {
	var req addRequest
	if !h.decode(w, r, &req) {
		return
	}

	site, err := h.svc.Add(r.Context(), AddInput{Name: req.Name, Domain: req.Domain})
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	h.writeJSON(w, http.StatusCreated, toResponse(*site))
}

// ToggleActive — PATCH /sites/{id}/toggle
//
// Body-less: it flips whatever the current value is. Allowed on pre-configured
// sites, which is how they are switched off without being removed.
func (h *Handler) ToggleActive(w http.ResponseWriter, r *http.Request) {
	id, ok := h.parseUUIDParam(w, r, "id")
	if !ok {
		return
	}

	site, err := h.svc.ToggleActive(r.Context(), id)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	h.writeJSON(w, http.StatusOK, toResponse(*site))
}

// Delete — DELETE /sites/{id}
//
// Custom sites only. Pre-configured sites and sites with applications attached
// are both refused with a 409.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	id, ok := h.parseUUIDParam(w, r, "id")
	if !ok {
		return
	}

	if err := h.svc.Delete(r.Context(), id); err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (h *Handler) decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	// A site is a name and a domain; 64 KiB is already generous.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(dst); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_body", "request body must be valid JSON")
		return false
	}
	return true
}

func (h *Handler) parseUUIDParam(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(h.pathParam(r, name))
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_site_id", "path parameter must be a UUID")
		return uuid.Nil, false
	}
	return id, true
}

func toResponse(site Site) siteResponse {
	return siteResponse{
		ID:              site.ID,
		Name:            site.Name,
		Domain:          site.Domain,
		IsPreconfigured: site.IsPreconfigured,
		IsActive:        site.IsActive,
		Selectors:       site.Selectors,
		CreatedAt:       site.CreatedAt,
		UpdatedAt:       site.UpdatedAt,
	}
}

func toResponses(sites []Site) []siteResponse {
	out := make([]siteResponse, 0, len(sites))
	for _, site := range sites {
		out = append(out, toResponse(site))
	}
	return out
}

// errorBody mirrors the envelope used by the applications, cvs and coverletters
// modules.
type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (h *Handler) writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		h.writeError(w, http.StatusNotFound, "site_not_found", "no site with that id")
	case errors.Is(err, ErrNameRequired):
		h.writeError(w, http.StatusBadRequest, "name_required", "name is required")
	case errors.Is(err, ErrDomainRequired):
		h.writeError(w, http.StatusBadRequest, "domain_required", "domain is required")
	case errors.Is(err, ErrInvalidDomain):
		h.writeError(
			w, http.StatusBadRequest, "invalid_domain",
			"domain must be a host such as example.com, or a url to take one from",
		)
	case errors.Is(err, ErrDuplicate):
		h.writeError(w, http.StatusConflict, "site_already_exists", "a site with that name or domain already exists")
	case errors.Is(err, ErrPreconfigured):
		h.writeError(
			w, http.StatusConflict, "site_is_preconfigured",
			"pre-configured sites cannot be deleted — deactivate it instead",
		)
	case errors.Is(err, ErrInUse):
		h.writeError(
			w, http.StatusConflict, "site_in_use",
			"applications still reference this site — deactivate it instead",
		)
	default:
		h.logger.ErrorContext(
			r.Context(), "sites handler failure",
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
		h.logger.Error("sites: encode response", slog.String("error", err.Error()))
	}
}
