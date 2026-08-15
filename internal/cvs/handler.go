package cvs

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/EngenMe/applymind-backend/pkg/middleware"
)

// Handler exposes the cvs module over HTTP via chi, matching the router set up
// in cmd/api/main.go. Register it inside the authenticated group:
//
//	cvs.NewHandler(cvSvc, logger).RegisterRoutes(authed)
type Handler struct {
	svc            Service
	logger         *slog.Logger
	maxUploadBytes int64
}

func NewHandler(svc Service, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{svc: svc, logger: logger, maxUploadBytes: DefaultMaxUploadBytes}
}

func (h *Handler) RegisterRoutes(r chi.Router) {
	r.Post("/cvs", h.Upload)
	r.Get("/cvs", h.List)
	r.Post("/cvs/match", h.Match)
	r.Get("/cvs/{id}/versions", h.ListVersions)
	r.Get("/cvs/{id}/versions/{versionId}/download", h.Download)
}

// pathParam is the single point of coupling to the router.
func (h *Handler) pathParam(r *http.Request, name string) string {
	return chi.URLParam(r, name)
}

// requireUser reads the authenticated caller. Reaching a handler behind
// RequireAuth with no user in context is a wiring bug, not an authentication
// failure — the middleware would already have rejected the request — so this
// is a logged 500, matching the same convention in internal/auth.
func (h *Handler) requireUser(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	userID, ok := middleware.UserID(r.Context())
	if !ok {
		h.logger.ErrorContext(
			r.Context(), "cvs: no user in context on a protected route",
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

type matchRequest struct {
	SHA256Hash    string `json:"sha256_hash"`
	Filename      string `json:"filename"`
	FileSizeBytes *int64 `json:"file_size_bytes"`
}

type matchResponse struct {
	Outcome      string               `json:"outcome"`
	MatchedBy    string               `json:"matched_by,omitempty"`
	CV           *cvResponse          `json:"cv,omitempty"`
	Version      *versionResponse     `json:"version,omitempty"`
	Confirmation *confirmationPayload `json:"confirmation,omitempty"`
}

type confirmationPayload struct {
	CVName      string     `json:"cv_name"`
	LastUsedAt  *time.Time `json:"last_used_at"`
	LastCompany *string    `json:"last_company"`
}

type cvResponse struct {
	ID        uuid.UUID         `json:"id"`
	Name      string            `json:"name"`
	Tag       *string           `json:"tag"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
	Versions  []versionResponse `json:"versions,omitempty"`
}

type versionResponse struct {
	ID               uuid.UUID `json:"id"`
	CVID             uuid.UUID `json:"cv_id"`
	SHA256Hash       string    `json:"sha256_hash"`
	FileSizeBytes    int64     `json:"file_size_bytes"`
	OriginalFilename string    `json:"original_filename"`
	UploadedAt       time.Time `json:"uploaded_at"`
}

type uploadResponse struct {
	CV             cvResponse      `json:"cv"`
	Version        versionResponse `json:"version"`
	AlreadyExisted bool            `json:"already_existed"`
}

type downloadResponse struct {
	URL       string    `json:"url"`
	Filename  string    `json:"filename"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// Match — POST /cvs/match
// Body: {"sha256_hash": "...", "filename": "cv.pdf", "file_size_bytes": 123456}
// sha256_hash is omitted when the browser could not read the file content;
// file_size_bytes is omitted when the size was unavailable.
//
// This endpoint never writes. When the outcome is new_version or unknown the
// extension follows up with POST /cvs to store the file.
func (h *Handler) Match(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireUser(w, r)
	if !ok {
		return
	}

	var req matchRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_body", "request body must be valid JSON")
		return
	}

	result, err := h.svc.Match(r.Context(), userID, MatchQuery{
		SHA256Hash:    req.SHA256Hash,
		Filename:      req.Filename,
		FileSizeBytes: req.FileSizeBytes,
	})
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}

	resp := matchResponse{
		Outcome:   string(result.Outcome),
		MatchedBy: string(result.MatchedBy),
	}
	if result.CV != nil {
		cv := toCVResponse(*result.CV)
		resp.CV = &cv
	}
	if result.Version != nil {
		v := toVersionResponse(*result.Version)
		resp.Version = &v
	}
	if result.Confirmation != nil {
		resp.Confirmation = &confirmationPayload{
			CVName:      result.Confirmation.CVName,
			LastUsedAt:  result.Confirmation.LastUsedAt,
			LastCompany: result.Confirmation.LastCompany,
		}
	}
	h.writeJSON(w, http.StatusOK, resp)
}

// Upload — POST /cvs (multipart/form-data)
// Fields: file (required), cv_id (optional — attach as a new version of an
// existing CV group), tag (optional, only used when creating a new group).
func (h *Handler) Upload(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireUser(w, r)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, h.maxUploadBytes+1024)
	if err := r.ParseMultipartForm(h.maxUploadBytes); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.writeError(w, http.StatusRequestEntityTooLarge, "file_too_large", "the uploaded file is too large")
			return
		}
		h.writeError(w, http.StatusBadRequest, "invalid_multipart", "expected a multipart/form-data body with a 'file' field")
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "file_required", "a multipart field named 'file' is required")
		return
	}
	defer file.Close()

	content, err := io.ReadAll(io.LimitReader(file, h.maxUploadBytes+1))
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "unreadable_file", "could not read the uploaded file")
		return
	}
	if int64(len(content)) > h.maxUploadBytes {
		h.writeError(w, http.StatusRequestEntityTooLarge, "file_too_large", "the uploaded file is too large")
		return
	}

	in := UploadInput{
		Filename:    header.Filename,
		Content:     content,
		ContentType: header.Header.Get("Content-Type"),
	}
	if raw := r.FormValue("cv_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			h.writeError(w, http.StatusBadRequest, "invalid_cv_id", "cv_id must be a UUID")
			return
		}
		in.CVID = &id
	}
	if tag := r.FormValue("tag"); tag != "" {
		in.Tag = &tag
	}

	result, err := h.svc.Upload(r.Context(), userID, in)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}

	status := http.StatusCreated
	if result.AlreadyExisted {
		status = http.StatusOK
	}
	h.writeJSON(w, status, uploadResponse{
		CV:             toCVResponse(*result.CV),
		Version:        toVersionResponse(*result.Version),
		AlreadyExisted: result.AlreadyExisted,
	})
}

// List — GET /cvs (each CV with its full version history)
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireUser(w, r)
	if !ok {
		return
	}

	cvs, err := h.svc.ListWithVersions(r.Context(), userID)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	out := make([]cvResponse, 0, len(cvs))
	for _, cv := range cvs {
		out = append(out, toCVResponse(cv))
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"cvs": out})
}

// ListVersions — GET /cvs/{id}/versions
func (h *Handler) ListVersions(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	cvID, ok := h.parseUUIDParam(w, r, "id", "invalid_cv_id")
	if !ok {
		return
	}
	versions, err := h.svc.ListVersions(r.Context(), userID, cvID)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	out := make([]versionResponse, 0, len(versions))
	for _, v := range versions {
		out = append(out, toVersionResponse(v))
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"versions": out})
}

// Download — GET /cvs/{id}/versions/{versionId}/download
// Returns a short-lived presigned S3 URL as JSON rather than redirecting, so the
// extension and dashboard can both decide what to do with it.
func (h *Handler) Download(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	cvID, ok := h.parseUUIDParam(w, r, "id", "invalid_cv_id")
	if !ok {
		return
	}
	versionID, ok := h.parseUUIDParam(w, r, "versionId", "invalid_version_id")
	if !ok {
		return
	}

	link, err := h.svc.DownloadURL(r.Context(), userID, cvID, versionID)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	h.writeJSON(w, http.StatusOK, downloadResponse{
		URL:       link.URL,
		Filename:  link.Filename,
		ExpiresAt: link.ExpiresAt,
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (h *Handler) parseUUIDParam(w http.ResponseWriter, r *http.Request, name, code string) (uuid.UUID, bool) {
	id, err := uuid.Parse(h.pathParam(r, name))
	if err != nil {
		h.writeError(w, http.StatusBadRequest, code, "path parameter must be a UUID")
		return uuid.Nil, false
	}
	return id, true
}

func toCVResponse(cv CV) cvResponse {
	out := cvResponse{
		ID:        cv.ID,
		Name:      cv.Name,
		Tag:       cv.Tag,
		CreatedAt: cv.CreatedAt,
		UpdatedAt: cv.UpdatedAt,
	}
	if cv.Versions != nil {
		out.Versions = make([]versionResponse, 0, len(cv.Versions))
		for _, v := range cv.Versions {
			out.Versions = append(out.Versions, toVersionResponse(v))
		}
	}
	return out
}

func toVersionResponse(v CVVersion) versionResponse {
	return versionResponse{
		ID:               v.ID,
		CVID:             v.CVID,
		SHA256Hash:       v.SHA256Hash,
		FileSizeBytes:    v.FileSizeBytes,
		OriginalFilename: v.OriginalFilename,
		UploadedAt:       v.UploadedAt,
	}
}

// errorBody is this module's error envelope. Swap for the shared one if the
// sites/applications modules already define a project-wide format.
type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (h *Handler) writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrCVNotFound):
		h.writeError(w, http.StatusNotFound, "cv_not_found", "no CV with that id")
	case errors.Is(err, ErrVersionNotFound):
		h.writeError(w, http.StatusNotFound, "version_not_found", "no version with that id for this CV")
	case errors.Is(err, ErrFilenameEmpty):
		h.writeError(w, http.StatusBadRequest, "filename_required", "filename is required")
	case errors.Is(err, ErrEmptyFile):
		h.writeError(w, http.StatusBadRequest, "empty_file", "the uploaded file is empty")
	case errors.Is(err, ErrFileTooLarge):
		h.writeError(w, http.StatusRequestEntityTooLarge, "file_too_large", "the uploaded file is too large")
	default:
		h.logger.ErrorContext(r.Context(), "cvs handler failure",
			slog.String("path", r.URL.Path), slog.String("error", err.Error()))
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
		h.logger.Error("cvs: encode response", slog.String("error", err.Error()))
	}
}
