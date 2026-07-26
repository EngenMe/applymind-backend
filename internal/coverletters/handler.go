package coverletters

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Handler exposes the coverletters module over HTTP via chi, matching the
// router set up in cmd/api/main.go. Register it inside the API-key-protected
// group:
//
//	coverletters.NewHandler(clSvc, logger).RegisterRoutes(protected)
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
	r.Post("/applications/{id}/coverletter", h.Save)
	r.Get("/applications/{id}/coverletter", h.Get)
	r.Put("/applications/{id}/coverletter", h.EditText)
	r.Get("/applications/{id}/coverletter/download", h.Download)
}

// pathParam is the single point of coupling to the router.
func (h *Handler) pathParam(r *http.Request, name string) string {
	return chi.URLParam(r, name)
}

// ---------------------------------------------------------------------------
// Request / response DTOs
// ---------------------------------------------------------------------------

type textRequest struct {
	BodyText string `json:"body_text"`
}

// coverLetterResponse carries whichever half of the row is populated. For a
// file cover letter DownloadPath tells the client where to get a presigned URL,
// rather than exposing the s3 key.
type coverLetterResponse struct {
	ID               uuid.UUID `json:"id"`
	ApplicationID    uuid.UUID `json:"application_id"`
	Kind             string    `json:"kind"`
	BodyText         *string   `json:"body_text"`
	OriginalFilename *string   `json:"original_filename"`
	DownloadPath     string    `json:"download_path,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type downloadResponse struct {
	URL       string    `json:"url"`
	Filename  string    `json:"filename"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// Save — POST /applications/{id}/coverletter
//
// The request body decides the kind:
//   - multipart/form-data with a 'file' field  → kind = file (PDF, DOCX or DOC)
//   - application/json {"body_text": "..."}    → kind = text
//
// An application has at most one cover letter, so a save replaces whatever was
// there before, including deleting the previous file from S3.
func (h *Handler) Save(w http.ResponseWriter, r *http.Request) {
	applicationID, ok := h.parseUUIDParam(w, r, "id", "invalid_application_id")
	if !ok {
		return
	}

	if isMultipart(r) {
		h.saveFile(w, r, applicationID)
		return
	}
	h.saveText(w, r, applicationID)
}

func (h *Handler) saveText(w http.ResponseWriter, r *http.Request, applicationID uuid.UUID) {
	req, ok := h.decodeTextRequest(w, r)
	if !ok {
		return
	}

	cl, err := h.svc.SaveText(r.Context(), SaveTextInput{
		ApplicationID: applicationID,
		BodyText:      req.BodyText,
	})
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	h.writeJSON(w, http.StatusCreated, toResponse(*cl))
}

func (h *Handler) saveFile(w http.ResponseWriter, r *http.Request, applicationID uuid.UUID) {
	r.Body = http.MaxBytesReader(w, r.Body, h.maxUploadBytes+1024)
	if err := r.ParseMultipartForm(h.maxUploadBytes); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.writeError(w, http.StatusRequestEntityTooLarge, "file_too_large", "the uploaded file is too large")
			return
		}
		h.writeError(
			w, http.StatusBadRequest, "invalid_multipart",
			"expected a multipart/form-data body with a 'file' field",
		)
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

	cl, err := h.svc.SaveFile(r.Context(), SaveFileInput{
		ApplicationID: applicationID,
		Filename:      header.Filename,
		Content:       content,
		ContentType:   header.Header.Get("Content-Type"),
	})
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	h.writeJSON(w, http.StatusCreated, toResponse(*cl))
}

// Get — GET /applications/{id}/coverletter
// Returns the body for a text cover letter, or the filename plus the path to
// call for a download URL for a file one.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	applicationID, ok := h.parseUUIDParam(w, r, "id", "invalid_application_id")
	if !ok {
		return
	}

	cl, err := h.svc.Get(r.Context(), applicationID)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	h.writeJSON(w, http.StatusOK, toResponse(*cl))
}

// EditText — PUT /applications/{id}/coverletter
// Body: {"body_text": "..."}
//
// Text cover letters only. A file cover letter is a record of what was actually
// sent, so it cannot be edited in place — replacing it is a POST.
func (h *Handler) EditText(w http.ResponseWriter, r *http.Request) {
	applicationID, ok := h.parseUUIDParam(w, r, "id", "invalid_application_id")
	if !ok {
		return
	}
	req, ok := h.decodeTextRequest(w, r)
	if !ok {
		return
	}

	cl, err := h.svc.EditText(r.Context(), applicationID, req.BodyText)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	h.writeJSON(w, http.StatusOK, toResponse(*cl))
}

// Download — GET /applications/{id}/coverletter/download
// Returns a short-lived presigned S3 URL as JSON rather than redirecting, so
// the extension and dashboard can both decide what to do with it. File kind only.
func (h *Handler) Download(w http.ResponseWriter, r *http.Request) {
	applicationID, ok := h.parseUUIDParam(w, r, "id", "invalid_application_id")
	if !ok {
		return
	}

	link, err := h.svc.DownloadURL(r.Context(), applicationID)
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

// isMultipart decides which half of Save runs. A body with no Content-Type at
// all is treated as JSON so the error the client gets is about the JSON, which
// is the more common case.
func isMultipart(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	return strings.HasPrefix(mediaType, "multipart/")
}

func (h *Handler) decodeTextRequest(w http.ResponseWriter, r *http.Request) (textRequest, bool) {
	var req textRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_body", "request body must be valid JSON")
		return textRequest{}, false
	}
	return req, true
}

func (h *Handler) parseUUIDParam(w http.ResponseWriter, r *http.Request, name, code string) (uuid.UUID, bool) {
	id, err := uuid.Parse(h.pathParam(r, name))
	if err != nil {
		h.writeError(w, http.StatusBadRequest, code, "path parameter must be a UUID")
		return uuid.Nil, false
	}
	return id, true
}

func toResponse(cl CoverLetter) coverLetterResponse {
	out := coverLetterResponse{
		ID:               cl.ID,
		ApplicationID:    cl.ApplicationID,
		Kind:             string(cl.Kind),
		BodyText:         cl.BodyText,
		OriginalFilename: cl.OriginalFilename,
		CreatedAt:        cl.CreatedAt,
		UpdatedAt:        cl.UpdatedAt,
	}
	if cl.Kind == KindFile {
		out.DownloadPath = "/applications/" + cl.ApplicationID.String() + "/coverletter/download"
	}
	return out
}

// errorBody mirrors the envelope used by the cvs module. Swap both for the
// shared one if a project-wide format is introduced later.
type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (h *Handler) writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		h.writeError(w, http.StatusNotFound, "cover_letter_not_found", "this application has no cover letter")
	case errors.Is(err, ErrApplicationNotFound):
		h.writeError(w, http.StatusNotFound, "application_not_found", "no application with that id")
	case errors.Is(err, ErrAlreadyExists):
		h.writeError(
			w, http.StatusConflict, "cover_letter_exists",
			"this application already has a cover letter",
		)
	case errors.Is(err, ErrEmptyBody):
		h.writeError(w, http.StatusBadRequest, "body_text_required", "body_text is required and cannot be blank")
	case errors.Is(err, ErrFilenameEmpty):
		h.writeError(w, http.StatusBadRequest, "filename_required", "the uploaded file must have a filename")
	case errors.Is(err, ErrEmptyFile):
		h.writeError(w, http.StatusBadRequest, "empty_file", "the uploaded file is empty")
	case errors.Is(err, ErrFileTooLarge):
		h.writeError(w, http.StatusRequestEntityTooLarge, "file_too_large", "the uploaded file is too large")
	case errors.Is(err, ErrUnsupportedFileType):
		h.writeError(
			w, http.StatusUnsupportedMediaType, "unsupported_file_type",
			"only PDF, DOCX and DOC files are accepted",
		)
	case errors.Is(err, ErrNotTextKind):
		h.writeError(
			w, http.StatusConflict, "not_editable",
			"this cover letter is a file; upload a replacement instead of editing it",
		)
	case errors.Is(err, ErrNotFileKind):
		h.writeError(
			w, http.StatusConflict, "no_file_to_download",
			"this cover letter is text; read body_text instead",
		)
	default:
		h.logger.ErrorContext(
			r.Context(), "coverletters handler failure",
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
		h.logger.Error("coverletters: encode response", slog.String("error", err.Error()))
	}
}
