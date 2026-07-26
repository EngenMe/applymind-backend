package notifications

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Handler exposes the notifications module over HTTP via chi, matching the
// router set up in cmd/api/main.go. Register it inside the API-key-protected
// group:
//
//	notifications.NewHandler(notifSvc, logger).RegisterRoutes(protected)
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
	r.Get("/notifications/due", h.ListDue)
}

// ---------------------------------------------------------------------------
// Response DTOs
// ---------------------------------------------------------------------------

type notificationResponse struct {
	ReminderID       uuid.UUID        `json:"reminder_id"`
	ApplicationID    uuid.UUID        `json:"application_id"`
	Title            string           `json:"title"`
	Body             string           `json:"body"`
	CompanyName      string           `json:"company_name"`
	JobTitle         string           `json:"job_title"`
	JobURL           string           `json:"job_url"`
	Status           string           `json:"status"`
	DueAt            time.Time        `json:"due_at"`
	DaysSinceApplied int              `json:"days_since_applied"`
	AlreadyNotified  bool             `json:"already_notified"`
	SentAt           *time.Time       `json:"sent_at"`
	Actions          []actionResponse `json:"suggested_actions"`
}

type actionResponse struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
	URL   string `json:"url,omitempty"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// ListDue — GET /notifications/due
//
// What the dashboard should be showing right now. Reminders the daily sweep has
// already dispatched are included, flagged with already_notified, because a
// reminder raised at 08:00 is still the answer when the tab is opened at noon.
// A reminder leaves this list when its application stops being Applied, which
// dismisses it.
//
// The browser notification itself is raised by the client from this payload —
// nothing here pushes.
func (h *Handler) ListDue(w http.ResponseWriter, r *http.Request) {
	notifications, err := h.svc.ListDue(r.Context())
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}

	h.writeJSON(
		w, http.StatusOK, map[string]any{
			"notifications": toResponses(notifications),
			"count":         len(notifications),
		},
	)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func toResponse(n Notification) notificationResponse {
	actions := make([]actionResponse, 0, len(n.Actions))
	for _, a := range n.Actions {
		actions = append(
			actions, actionResponse{
				Kind:  string(a.Kind),
				Label: a.Label,
				URL:   a.URL,
			},
		)
	}

	return notificationResponse{
		ReminderID:       n.ReminderID,
		ApplicationID:    n.ApplicationID,
		Title:            n.Title,
		Body:             n.Body,
		CompanyName:      n.CompanyName,
		JobTitle:         n.JobTitle,
		JobURL:           n.JobURL,
		Status:           n.Status,
		DueAt:            n.DueAt,
		DaysSinceApplied: n.DaysSinceApplied,
		AlreadyNotified:  n.AlreadyNotified(),
		SentAt:           n.SentAt,
		Actions:          actions,
	}
}

func toResponses(ns []Notification) []notificationResponse {
	out := make([]notificationResponse, 0, len(ns))
	for _, n := range ns {
		out = append(out, toResponse(n))
	}
	return out
}

// errorBody mirrors the envelope used by the applications, sites, cvs and
// coverletters modules.
type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// writeServiceError has only a default arm for now: the one endpoint takes no
// input, so nothing it can be handed is invalid. Domain errors get their own
// cases as endpoints are added.
func (h *Handler) writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	h.logger.ErrorContext(
		r.Context(), "notifications handler failure",
		slog.String("path", r.URL.Path), slog.String("error", err.Error()),
	)
	h.writeError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
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
		h.logger.Error("notifications: encode response", slog.String("error", err.Error()))
	}
}
