package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type stubService struct {
	notifications []Notification
	err           error

	checked bool
}

func (s *stubService) CheckReminders(context.Context) (*RunSummary, error) {
	s.checked = true
	return &RunSummary{}, s.err
}

func (s *stubService) ListDue(context.Context) ([]Notification, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.notifications, nil
}

func newTestHandler(svc Service) http.Handler {
	r := chi.NewRouter()
	NewHandler(svc, slog.New(slog.NewTextHandler(io.Discard, nil))).RegisterRoutes(r)
	return r
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestListDueReturnsNotifications(t *testing.T) {
	sent := time.Date(2026, 5, 20, 8, 0, 0, 0, time.UTC)
	svc := &stubService{
		notifications: []Notification{
			{
				ReminderID:       uuid.New(),
				ApplicationID:    uuid.New(),
				Title:            "Follow up with Stripe",
				Body:             "You applied for Backend Engineer at Stripe 9 days ago and there has been no reply.",
				CompanyName:      "Stripe",
				JobTitle:         "Backend Engineer",
				JobURL:           "https://www.linkedin.com/jobs/view/123",
				Status:           "Applied",
				DueAt:            sent.Add(-24 * time.Hour),
				DaysSinceApplied: 9,
				SentAt:           &sent,
				Actions: []SuggestedAction{
					{Kind: ActionSendFollowUp, Label: "Send a follow-up email"},
					{Kind: ActionCheckLinkedIn, Label: "Check LinkedIn for updates", URL: "https://www.linkedin.com/jobs/view/123"},
				},
			},
		},
	}

	rec := get(t, newTestHandler(svc), "/notifications/due")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body struct {
		Count         int `json:"count"`
		Notifications []struct {
			Title           string `json:"title"`
			AlreadyNotified bool   `json:"already_notified"`
			Actions         []struct {
				Kind  string `json:"kind"`
				Label string `json:"label"`
				URL   string `json:"url"`
			} `json:"suggested_actions"`
		} `json:"notifications"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if body.Count != 1 || len(body.Notifications) != 1 {
		t.Fatalf("want 1 notification, got count=%d len=%d", body.Count, len(body.Notifications))
	}
	n := body.Notifications[0]
	if n.Title != "Follow up with Stripe" {
		t.Errorf("title = %q", n.Title)
	}
	if !n.AlreadyNotified {
		t.Error("want already_notified = true when sent_at is set")
	}
	if len(n.Actions) != 2 || n.Actions[0].Kind != string(ActionSendFollowUp) {
		t.Errorf("suggested_actions = %+v", n.Actions)
	}
	if n.Actions[1].URL == "" {
		t.Error("want the LinkedIn action to carry its url")
	}
}

func TestListDueEmptyIsAnEmptyArray(t *testing.T) {
	rec := get(t, newTestHandler(&stubService{}), "/notifications/due")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body struct {
		Count         int               `json:"count"`
		Notifications []json.RawMessage `json:"notifications"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Count != 0 {
		t.Errorf("count = %d, want 0", body.Count)
	}
	if body.Notifications == nil {
		t.Error("want [] rather than null, so the dashboard can iterate without a nil check")
	}
}

func TestListDueServiceFailureIs500(t *testing.T) {
	rec := get(t, newTestHandler(&stubService{err: errors.New("db gone")}), "/notifications/due")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}

	var body errorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != "internal_error" {
		t.Errorf("error code = %q, want internal_error", body.Error.Code)
	}
}
