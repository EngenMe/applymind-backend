package applications

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/EngenMe/applymind-backend/pkg/middleware"
)

// Handler exposes the applications module over HTTP via chi, matching the router
// set up in cmd/api/main.go. Register it inside the authenticated group:
//
//	applications.NewHandler(appSvc, logger).RegisterRoutes(authed)
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
	r.Post("/applications", h.Create)
	r.Get("/applications", h.List)
	// Static segment before the {id} routes: chi matches static first, but the
	// ordering keeps the intent obvious to a reader.
	r.Get("/applications/check-duplicate", h.CheckDuplicate)
	r.Get("/applications/{id}", h.Get)
	r.Put("/applications/{id}", h.Update)
	r.Patch("/applications/{id}/status", h.UpdateStatus)
	r.Patch("/applications/{id}/complete", h.Complete)
	r.Delete("/applications/{id}", h.Delete)
}

// pathParam is the single point of coupling to the router.
func (h *Handler) pathParam(r *http.Request, name string) string {
	return chi.URLParam(r, name)
}

// requireUser reads the authenticated caller. See the identical helper in
// internal/auth and internal/cvs for why a missing user here is a 500, not a 401.
func (h *Handler) requireUser(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	userID, ok := middleware.UserID(r.Context())
	if !ok {
		h.logger.ErrorContext(
			r.Context(), "applications: no user in context on a protected route",
			slog.String("path", r.URL.Path),
		)
		h.writeError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return uuid.Nil, false
	}
	return userID, true
}

// ---------------------------------------------------------------------------
// Request / response DTOs
// ---------------------------------------------------------------------------

type createRequest struct {
	CompanyName     string     `json:"company_name"`
	JobTitle        string     `json:"job_title"`
	JobDescription  string     `json:"job_description"`
	JobURL          string     `json:"job_url"`
	SiteID          *uuid.UUID `json:"site_id"`
	CVVersionID     *uuid.UUID `json:"cv_version_id"`
	Status          string     `json:"status"`
	AppliedAt       *time.Time `json:"applied_at"`
	CoverLetterText *string    `json:"cover_letter_text"`
	Note            *string    `json:"note"`
}

type updateRequest struct {
	CompanyName    string     `json:"company_name"`
	JobTitle       string     `json:"job_title"`
	JobDescription string     `json:"job_description"`
	JobURL         string     `json:"job_url"`
	SiteID         *uuid.UUID `json:"site_id"`
	CVVersionID    *uuid.UUID `json:"cv_version_id"`
}

type statusRequest struct {
	Status    string  `json:"status"`
	Note      *string `json:"note"`
	ChangedBy string  `json:"changed_by"`
}

// completeRequest is what the extension sends from the external site. Every
// field is optional: pressing "Mark as Complete" with nothing attached still
// means the application was sent.
type completeRequest struct {
	CVVersionID     *uuid.UUID `json:"cv_version_id"`
	CoverLetterText *string    `json:"cover_letter_text"`
	Note            *string    `json:"note"`
	CompletedAt     *time.Time `json:"completed_at"`
}

type applicationResponse struct {
	ID                 uuid.UUID         `json:"id"`
	CompanyName        string            `json:"company_name"`
	JobTitle           string            `json:"job_title"`
	JobDescription     string            `json:"job_description"`
	JobURL             string            `json:"job_url"`
	SiteID             uuid.UUID         `json:"site_id"`
	CVVersionID        *uuid.UUID        `json:"cv_version_id"`
	Status             string            `json:"status"`
	AIScore            *float64          `json:"ai_score"`
	AIScoreExplanation *string           `json:"ai_score_explanation"`
	AppliedAt          *time.Time        `json:"applied_at"`
	CreatedAt          time.Time         `json:"created_at"`
	UpdatedAt          time.Time         `json:"updated_at"`
	History            []historyResponse `json:"status_history,omitempty"`
}

type historyResponse struct {
	ID         uuid.UUID `json:"id"`
	FromStatus *string   `json:"from_status"`
	ToStatus   string    `json:"to_status"`
	ChangedBy  string    `json:"changed_by"`
	Note       *string   `json:"note"`
	ChangedAt  time.Time `json:"changed_at"`
}

// createResponse carries the saved application plus the two things the sidebar
// needs to show immediately: whether this looks like a repeat application, and
// when the follow-up nudge is due.
type createResponse struct {
	Application   applicationResponse `json:"application"`
	Duplicate     *duplicatePayload   `json:"duplicate_warning,omitempty"`
	FollowUpDueAt *time.Time          `json:"follow_up_due_at,omitempty"`
}

// completeResponse mirrors createResponse. No duplicate warning: the duplicate
// question was answered when the application was first saved, before the browser
// ever left LinkedIn.
type completeResponse struct {
	Application   applicationResponse `json:"application"`
	FollowUpDueAt *time.Time          `json:"follow_up_due_at,omitempty"`
}

// duplicatePayload sorts the matches so the client can phrase the warning
// honestly: LikelySame is "you may have already applied to this job",
// CrossSite is the same job found under a different site, and Matches is
// everything on record for this company.
type duplicatePayload struct {
	CompanyName string                `json:"company_name"`
	JobTitle    string                `json:"job_title,omitempty"`
	Matches     []applicationResponse `json:"matches"`
	LikelySame  []applicationResponse `json:"likely_same_job,omitempty"`
	CrossSite   []applicationResponse `json:"cross_site,omitempty"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// Create — POST /applications
//
// Flow 1, step 17. site_id is optional: when it is absent the site is derived
// from the job url's domain. An existing application for the same company comes
// back as duplicate_warning — the save still happens, and the caller decides
// what to tell the user.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireUser(w, r)
	if !ok {
		return
	}

	var req createRequest
	if !h.decode(w, r, &req) {
		return
	}

	result, err := h.svc.Create(
		r.Context(), userID, CreateInput{
			CompanyName:     req.CompanyName,
			JobTitle:        req.JobTitle,
			JobDescription:  req.JobDescription,
			JobURL:          req.JobURL,
			SiteID:          req.SiteID,
			CVVersionID:     req.CVVersionID,
			Status:          Status(req.Status),
			AppliedAt:       req.AppliedAt,
			CoverLetterText: req.CoverLetterText,
			Note:            req.Note,
		},
	)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}

	resp := createResponse{
		Application:   toResponse(*result.Application),
		FollowUpDueAt: result.FollowUpDueAt,
	}
	if result.Duplicate != nil {
		resp.Duplicate = toDuplicatePayload(result.Duplicate)
	}
	h.writeJSON(w, http.StatusCreated, resp)
}

// List — GET /applications
//
// Filters, all optional and combinable:
//
//	status, site_id, cv_version_id, company, q (company or title search),
//	from, to (RFC3339 or YYYY-MM-DD), limit, offset
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireUser(w, r)
	if !ok {
		return
	}

	q := r.URL.Query()
	filter := ListFilter{
		Company: optionalString(q.Get("company")),
		Search:  optionalString(q.Get("q")),
	}

	if raw := q.Get("status"); raw != "" {
		status := Status(raw)
		filter.Status = &status
	}
	var uOK bool
	if filter.SiteID, uOK = h.optionalUUID(w, q.Get("site_id"), "invalid_site_id"); !uOK {
		return
	}
	if filter.CVVersionID, uOK = h.optionalUUID(w, q.Get("cv_version_id"), "invalid_cv_version_id"); !uOK {
		return
	}
	if filter.From, uOK = h.optionalTime(w, q.Get("from"), "invalid_from"); !uOK {
		return
	}
	if filter.To, uOK = h.optionalTime(w, q.Get("to"), "invalid_to"); !uOK {
		return
	}
	filter.Limit = atoiOr(q.Get("limit"), 0)
	filter.Offset = atoiOr(q.Get("offset"), 0)

	apps, err := h.svc.List(r.Context(), userID, filter)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"applications": toResponses(apps)})
}

// Get — GET /applications/{id}, with the full status history.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id, ok := h.parseUUIDParam(w, r, "id")
	if !ok {
		return
	}

	app, err := h.svc.Get(r.Context(), userID, id)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	h.writeJSON(w, http.StatusOK, toResponse(*app))
}

// Update — PUT /applications/{id}
// Captured job data only; use PATCH .../status to move the status.
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id, ok := h.parseUUIDParam(w, r, "id")
	if !ok {
		return
	}
	var req updateRequest
	if !h.decode(w, r, &req) {
		return
	}

	app, err := h.svc.Update(
		r.Context(), userID, id, UpdateInput{
			CompanyName:    req.CompanyName,
			JobTitle:       req.JobTitle,
			JobDescription: req.JobDescription,
			JobURL:         req.JobURL,
			SiteID:         req.SiteID,
			CVVersionID:    req.CVVersionID,
		},
	)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	h.writeJSON(w, http.StatusOK, toResponse(*app))
}

// UpdateStatus — PATCH /applications/{id}/status
// Body: {"status": "Interviewing", "note": "phone screen booked"}
// changed_by defaults to "user".
func (h *Handler) UpdateStatus(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id, ok := h.parseUUIDParam(w, r, "id")
	if !ok {
		return
	}
	var req statusRequest
	if !h.decode(w, r, &req) {
		return
	}

	app, err := h.svc.UpdateStatus(
		r.Context(), userID, id, StatusUpdateInput{
			Status:    Status(req.Status),
			Note:      req.Note,
			ChangedBy: ChangeSource(req.ChangedBy),
		},
	)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	h.writeJSON(w, http.StatusOK, toResponse(*app))
}

// Complete — PATCH /applications/{id}/complete
//
// Flow 2, step 29. Finishes an application submitted on a company's own site:
// attaches the CV version and cover letter that went with it, moves the
// application to Applied and schedules the follow-up, in one call.
//
// Body, all fields optional:
//
//	{"cv_version_id": "...", "cover_letter_text": "...", "note": "...",
//	 "completed_at": "2026-08-05T09:12:00Z"}
//
// An application that is already Applied comes back 409 — it has been sent.
func (h *Handler) Complete(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id, ok := h.parseUUIDParam(w, r, "id")
	if !ok {
		return
	}
	var req completeRequest
	if !h.decode(w, r, &req) {
		return
	}

	result, err := h.svc.Complete(
		r.Context(), userID, id, CompleteInput{
			CVVersionID:     req.CVVersionID,
			CoverLetterText: req.CoverLetterText,
			Note:            req.Note,
			CompletedAt:     req.CompletedAt,
		},
	)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}

	h.writeJSON(
		w, http.StatusOK, completeResponse{
			Application:   toResponse(*result.Application),
			FollowUpDueAt: result.FollowUpDueAt,
		},
	)
}

// Delete — DELETE /applications/{id}
// Cover letter, status history, reminders and recruiter contact go with it.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id, ok := h.parseUUIDParam(w, r, "id")
	if !ok {
		return
	}

	if err := h.svc.Delete(r.Context(), userID, id); err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// CheckDuplicate — GET /applications/check-duplicate?company=X&title=Y&job_url=Z
//
// A pre-flight check for the sidebar and the dashboard's create form. company is
// required; title turns "applied here before" into "may have applied to this
// job"; job_url (or site_id) is what lets a match be reported as cross-site.
//
// It never blocks anything, and neither does POST /applications, which reports
// the same warning if the user saves anyway.
func (h *Handler) CheckDuplicate(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireUser(w, r)
	if !ok {
		return
	}

	q := r.URL.Query()
	siteID, uOK := h.optionalUUID(w, q.Get("site_id"), "invalid_site_id")
	if !uOK {
		return
	}

	warning, err := h.svc.CheckDuplicate(
		r.Context(), userID, DuplicateQuery{
			Company:  q.Get("company"),
			JobTitle: q.Get("title"),
			SiteID:   siteID,
			JobURL:   q.Get("job_url"),
		},
	)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}

	if warning == nil {
		h.writeJSON(w, http.StatusOK, map[string]any{"duplicate": false, "matches": []applicationResponse{}})
		return
	}

	payload := toDuplicatePayload(warning)
	h.writeJSON(
		w, http.StatusOK, map[string]any{
			"duplicate":       true,
			"company_name":    payload.CompanyName,
			"job_title":       payload.JobTitle,
			"matches":         payload.Matches,
			"likely_same_job": payload.LikelySame,
			"cross_site":      payload.CrossSite,
		},
	)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (h *Handler) decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	// Job descriptions are long; 1 MiB leaves plenty of room without letting a
	// body run away.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(dst); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_body", "request body must be valid JSON")
		return false
	}
	return true
}

func (h *Handler) parseUUIDParam(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(h.pathParam(r, name))
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_application_id", "path parameter must be a UUID")
		return uuid.Nil, false
	}
	return id, true
}

func (h *Handler) optionalUUID(w http.ResponseWriter, raw, code string) (*uuid.UUID, bool) {
	if raw == "" {
		return nil, true
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, code, "must be a UUID")
		return nil, false
	}
	return &id, true
}

// optionalTime accepts a full RFC 3339 timestamp or a bare date, which is what a
// date picker sends.
func (h *Handler) optionalTime(w http.ResponseWriter, raw, code string) (*time.Time, bool) {
	if raw == "" {
		return nil, true
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, raw); err == nil {
			t = t.UTC()
			return &t, true
		}
	}
	h.writeError(w, http.StatusBadRequest, code, "must be an RFC3339 timestamp or YYYY-MM-DD date")
	return nil, false
}

func optionalString(raw string) *string {
	if raw == "" {
		return nil
	}
	return &raw
}

func atoiOr(raw string, fallback int) int {
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return n
}

func toResponse(app Application) applicationResponse {
	out := applicationResponse{
		ID:                 app.ID,
		CompanyName:        app.CompanyName,
		JobTitle:           app.JobTitle,
		JobDescription:     app.JobDescription,
		JobURL:             app.JobURL,
		SiteID:             app.SiteID,
		CVVersionID:        app.CVVersionID,
		Status:             string(app.Status),
		AIScore:            app.AIScore,
		AIScoreExplanation: app.AIScoreExplanation,
		AppliedAt:          app.AppliedAt,
		CreatedAt:          app.CreatedAt,
		UpdatedAt:          app.UpdatedAt,
	}
	if app.History != nil {
		out.History = make([]historyResponse, 0, len(app.History))
		for _, h := range app.History {
			entry := historyResponse{
				ID:        h.ID,
				ToStatus:  string(h.ToStatus),
				ChangedBy: string(h.ChangedBy),
				Note:      h.Note,
				ChangedAt: h.ChangedAt,
			}
			if h.FromStatus != nil {
				from := string(*h.FromStatus)
				entry.FromStatus = &from
			}
			out.History = append(out.History, entry)
		}
	}
	return out
}

func toResponses(apps []Application) []applicationResponse {
	out := make([]applicationResponse, 0, len(apps))
	for _, app := range apps {
		out = append(out, toResponse(app))
	}
	return out
}

func toDuplicatePayload(warning *DuplicateWarning) *duplicatePayload {
	return &duplicatePayload{
		CompanyName: warning.CompanyName,
		JobTitle:    warning.JobTitle,
		Matches:     toResponses(warning.Matches),
		LikelySame:  toResponses(warning.LikelySame),
		CrossSite:   toResponses(warning.CrossSite),
	}
}

// errorBody mirrors the envelope used by the cvs and coverletters modules.
type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (h *Handler) writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		h.writeError(w, http.StatusNotFound, "application_not_found", "no application with that id")
	case errors.Is(err, ErrCompanyRequired):
		h.writeError(w, http.StatusBadRequest, "company_name_required", "company_name is required")
	case errors.Is(err, ErrJobTitleRequired):
		h.writeError(w, http.StatusBadRequest, "job_title_required", "job_title is required")
	case errors.Is(err, ErrJobURLRequired):
		h.writeError(w, http.StatusBadRequest, "job_url_required", "job_url is required")
	case errors.Is(err, ErrInvalidStatus):
		h.writeError(w, http.StatusBadRequest, "invalid_status", "status is not a known application status")
	case errors.Is(err, ErrInvalidChangeSource):
		h.writeError(w, http.StatusBadRequest, "invalid_changed_by", "changed_by must be 'user' or 'system'")
	case errors.Is(err, ErrAlreadyCompleted):
		h.writeError(
			w, http.StatusConflict, "already_completed",
			"this application has already been marked complete",
		)
	case errors.Is(err, ErrSameStatus):
		h.writeError(w, http.StatusConflict, "status_unchanged", "the application is already in that status")
	case errors.Is(err, ErrSiteUnresolvable):
		h.writeError(
			w, http.StatusBadRequest, "site_unresolvable",
			"job_url has no host to derive a site from — send site_id instead",
		)
	case errors.Is(err, ErrSiteNotFound):
		h.writeError(w, http.StatusNotFound, "site_not_found", "no active site matches that id or domain")
	case errors.Is(err, ErrCVVersionNotFound):
		h.writeError(w, http.StatusNotFound, "cv_version_not_found", "no cv version with that id")
	case errors.Is(err, ErrDuplicateJobURL):
		h.writeError(
			w, http.StatusConflict, "job_url_already_saved",
			"this job url has already been saved for this site",
		)
	default:
		h.logger.ErrorContext(
			r.Context(), "applications handler failure",
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
		h.logger.Error("applications: encode response", slog.String("error", err.Error()))
	}
}
