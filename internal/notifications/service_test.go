package notifications

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

var fixedNow = time.Date(2026, 5, 20, 8, 0, 0, 0, time.UTC)

// testUserID is declared in handler_test.go — Go test files in a package share
// one namespace, so it is declared once there and used here too. ListDue tests
// act as that caller; CheckReminders tests need no user at all, since the sweep
// is global, which is what TestCheckRemindersAsksForPendingOnly asserts by
// checking stubRepo.gotUserIDs is nil.

// ---------------------------------------------------------------------------
// Stubs
// ---------------------------------------------------------------------------

type stubRepo struct {
	due     []FollowUpReminder
	findErr error

	filters    []DueFilter
	gotUserIDs []*uuid.UUID
	marked     []uuid.UUID
	markedAt   []time.Time

	markErr    error
	notPending map[uuid.UUID]bool
}

func (s *stubRepo) FindDue(_ context.Context, userID *uuid.UUID, f DueFilter) ([]FollowUpReminder, error) {
	s.filters = append(s.filters, f)
	s.gotUserIDs = append(s.gotUserIDs, userID)
	if s.findErr != nil {
		return nil, s.findErr
	}
	return s.due, nil
}

func (s *stubRepo) MarkSent(_ context.Context, id uuid.UUID, at time.Time) (bool, error) {
	s.marked = append(s.marked, id)
	s.markedAt = append(s.markedAt, at)
	if s.markErr != nil {
		return false, s.markErr
	}
	if s.notPending[id] {
		return false, nil
	}
	return true, nil
}

type stubNotifier struct {
	got  []Notification
	errs map[uuid.UUID]error
}

func (s *stubNotifier) Notify(_ context.Context, n Notification) error {
	s.got = append(s.got, n)
	return s.errs[n.ReminderID]
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

func newTestService(repo Repository, notifier Notifier, opts ...ServiceOption) Service {
	base := []ServiceOption{
		WithNotifier(notifier),
		WithClock(func() time.Time { return fixedNow }),
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	}
	return NewService(repo, append(base, opts...)...)
}

// reminder builds a due reminder for an application sent appliedDaysAgo days
// before fixedNow.
func reminder(appliedDaysAgo int) FollowUpReminder {
	appliedAt := fixedNow.AddDate(0, 0, -appliedDaysAgo)
	return FollowUpReminder{
		ID:            uuid.New(),
		ApplicationID: uuid.New(),
		DueAt:         appliedAt.Add(7 * 24 * time.Hour),
		CreatedAt:     appliedAt,
		UpdatedAt:     appliedAt,
		Application: ApplicationSummary{
			CompanyName: "Stripe",
			JobTitle:    "Backend Engineer",
			JobURL:      "https://www.linkedin.com/jobs/view/123",
			Status:      "Applied",
			AppliedAt:   &appliedAt,
		},
	}
}

// ---------------------------------------------------------------------------
// Finding due reminders
// ---------------------------------------------------------------------------

func TestCheckRemindersAsksForPendingOnly(t *testing.T) {
	repo := &stubRepo{}
	svc := newTestService(repo, &stubNotifier{})

	if _, err := svc.CheckReminders(context.Background()); err != nil {
		t.Fatalf("CheckReminders: %v", err)
	}

	if len(repo.filters) != 1 {
		t.Fatalf("want 1 FindDue call, got %d", len(repo.filters))
	}
	if repo.filters[0].IncludeSent {
		t.Error("the sweep must exclude already-sent reminders, or it re-notifies the same day")
	}
	if !repo.filters[0].AsOf.Equal(fixedNow) {
		t.Errorf("AsOf = %v, want %v", repo.filters[0].AsOf, fixedNow)
	}
	if repo.gotUserIDs[0] != nil {
		t.Errorf("CheckReminders must pass a nil userID — the sweep is for every user, got %v", repo.gotUserIDs[0])
	}
}

func TestListDueIncludesAlreadySentReminders(t *testing.T) {
	sent := fixedNow.Add(-2 * time.Hour)
	r := reminder(9)
	r.SentAt = &sent

	repo := &stubRepo{due: []FollowUpReminder{r}}
	svc := newTestService(repo, &stubNotifier{})

	out, err := svc.ListDue(context.Background(), testUserID)
	if err != nil {
		t.Fatalf("ListDue: %v", err)
	}

	if !repo.filters[0].IncludeSent {
		t.Fatal("the dashboard poll must include sent reminders")
	}
	if repo.gotUserIDs[0] == nil || *repo.gotUserIDs[0] != testUserID {
		t.Errorf("ListDue must scope FindDue to the caller, got %v want %s", repo.gotUserIDs[0], testUserID)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 notification, got %d", len(out))
	}
	if !out[0].AlreadyNotified() {
		t.Error("want already_notified for a reminder the sweep dispatched")
	}
}

func TestCheckRemindersEmptyList(t *testing.T) {
	notifier := &stubNotifier{}
	repo := &stubRepo{}
	svc := newTestService(repo, notifier)

	summary, err := svc.CheckReminders(context.Background())
	if err != nil {
		t.Fatalf("CheckReminders: %v", err)
	}

	if len(notifier.got) != 0 || len(repo.marked) != 0 {
		t.Error("nothing due should mean nothing dispatched and nothing marked")
	}
	if summary.DueCount != 0 || summary.SentCount != 0 || summary.FailedCount != 0 {
		t.Errorf("want an empty summary, got %+v", summary)
	}
}

func TestCheckRemindersPropagatesFindError(t *testing.T) {
	repo := &stubRepo{findErr: errors.New("boom")}
	svc := newTestService(repo, &stubNotifier{})

	if _, err := svc.CheckReminders(context.Background()); err == nil {
		t.Fatal("want an error when the due list cannot be read")
	}
}

// ---------------------------------------------------------------------------
// Marking sent
// ---------------------------------------------------------------------------

func TestCheckRemindersMarksEachDeliveredReminderSent(t *testing.T) {
	a, b := reminder(8), reminder(30)
	repo := &stubRepo{due: []FollowUpReminder{a, b}}
	notifier := &stubNotifier{}
	svc := newTestService(repo, notifier)

	summary, err := svc.CheckReminders(context.Background())
	if err != nil {
		t.Fatalf("CheckReminders: %v", err)
	}

	if len(notifier.got) != 2 {
		t.Fatalf("want 2 dispatched, got %d", len(notifier.got))
	}
	if len(repo.marked) != 2 || repo.marked[0] != a.ID || repo.marked[1] != b.ID {
		t.Fatalf("want both reminders marked sent, got %v", repo.marked)
	}
	if !repo.markedAt[0].Equal(fixedNow) {
		t.Errorf("sent_at = %v, want the sweep's own clock %v", repo.markedAt[0], fixedNow)
	}
	if summary.DueCount != 2 || summary.SentCount != 2 || summary.FailedCount != 0 {
		t.Errorf("summary = %+v, want 2 due / 2 sent / 0 failed", summary)
	}
}

func TestCheckRemindersLeavesFailedDeliveryUnmarked(t *testing.T) {
	r := reminder(8)
	repo := &stubRepo{due: []FollowUpReminder{r}}
	notifier := &stubNotifier{errs: map[uuid.UUID]error{r.ID: errors.New("transport down")}}
	svc := newTestService(repo, notifier)

	summary, err := svc.CheckReminders(context.Background())
	if err != nil {
		t.Fatalf("CheckReminders: %v", err)
	}

	if len(repo.marked) != 0 {
		t.Error("a failed delivery must leave sent_at NULL so tomorrow's run retries it")
	}
	if summary.SentCount != 0 || summary.FailedCount != 1 {
		t.Errorf("summary = %+v, want 0 sent / 1 failed", summary)
	}
}

func TestCheckRemindersContinuesAfterAFailure(t *testing.T) {
	bad, good := reminder(8), reminder(9)
	repo := &stubRepo{due: []FollowUpReminder{bad, good}}
	notifier := &stubNotifier{errs: map[uuid.UUID]error{bad.ID: errors.New("transport down")}}
	svc := newTestService(repo, notifier)

	summary, err := svc.CheckReminders(context.Background())
	if err != nil {
		t.Fatalf("CheckReminders: %v", err)
	}

	if len(repo.marked) != 1 || repo.marked[0] != good.ID {
		t.Fatalf("want only the healthy reminder marked, got %v", repo.marked)
	}
	if summary.SentCount != 1 || summary.FailedCount != 1 {
		t.Errorf("summary = %+v, want 1 sent / 1 failed", summary)
	}
}

func TestCheckRemindersSurvivesMarkSentFailure(t *testing.T) {
	repo := &stubRepo{due: []FollowUpReminder{reminder(8)}, markErr: errors.New("db gone")}
	svc := newTestService(repo, &stubNotifier{})

	summary, err := svc.CheckReminders(context.Background())
	if err != nil {
		t.Fatalf("a failed stamp must not fail the run: %v", err)
	}
	// Delivery happened, so it counts as sent even though it will repeat.
	if summary.SentCount != 1 || summary.FailedCount != 0 {
		t.Errorf("summary = %+v, want 1 sent / 0 failed", summary)
	}
}

func TestCheckRemindersToleratesConcurrentlyDismissedReminder(t *testing.T) {
	r := reminder(8)
	repo := &stubRepo{
		due:        []FollowUpReminder{r},
		notPending: map[uuid.UUID]bool{r.ID: true},
	}
	svc := newTestService(repo, &stubNotifier{})

	summary, err := svc.CheckReminders(context.Background())
	if err != nil {
		t.Fatalf("CheckReminders: %v", err)
	}
	if summary.FailedCount != 0 {
		t.Errorf("a no-op stamp is not a failure, got %+v", summary)
	}
}

// ---------------------------------------------------------------------------
// Suggested actions
// ---------------------------------------------------------------------------

func TestSuggestedActionsBeforeGhostThreshold(t *testing.T) {
	repo := &stubRepo{due: []FollowUpReminder{reminder(8)}}
	notifier := &stubNotifier{}
	svc := newTestService(repo, notifier)

	if _, err := svc.CheckReminders(context.Background()); err != nil {
		t.Fatalf("CheckReminders: %v", err)
	}

	got := labels(notifier.got[0].Actions)
	want := []string{"Send a follow-up email", "Check LinkedIn for updates"}
	assertLabels(t, got, want)
}

func TestSuggestedActionsAfterGhostThreshold(t *testing.T) {
	repo := &stubRepo{due: []FollowUpReminder{reminder(21)}}
	notifier := &stubNotifier{}
	svc := newTestService(repo, notifier)

	if _, err := svc.CheckReminders(context.Background()); err != nil {
		t.Fatalf("CheckReminders: %v", err)
	}

	got := labels(notifier.got[0].Actions)
	want := []string{"Send a follow-up email", "Mark as Ghost", "Check LinkedIn for updates"}
	assertLabels(t, got, want)
}

func TestGhostThresholdIsConfigurable(t *testing.T) {
	repo := &stubRepo{due: []FollowUpReminder{reminder(10)}}
	notifier := &stubNotifier{}
	svc := newTestService(repo, notifier, WithGhostThreshold(10*24*time.Hour))

	if _, err := svc.CheckReminders(context.Background()); err != nil {
		t.Fatalf("CheckReminders: %v", err)
	}

	if !hasKind(notifier.got[0].Actions, ActionMarkGhost) {
		t.Error("want Mark as Ghost once the configured threshold is reached")
	}
}

func TestCheckLinkedInCarriesTheJobURL(t *testing.T) {
	r := reminder(8)
	repo := &stubRepo{due: []FollowUpReminder{r}}
	notifier := &stubNotifier{}
	svc := newTestService(repo, notifier)

	if _, err := svc.CheckReminders(context.Background()); err != nil {
		t.Fatalf("CheckReminders: %v", err)
	}

	for _, a := range notifier.got[0].Actions {
		if a.Kind == ActionCheckLinkedIn && a.URL != r.Application.JobURL {
			t.Errorf("url = %q, want %q", a.URL, r.Application.JobURL)
		}
	}
}

// ---------------------------------------------------------------------------
// Notification content
// ---------------------------------------------------------------------------

func TestNotificationContent(t *testing.T) {
	r := reminder(9)
	repo := &stubRepo{due: []FollowUpReminder{r}}
	notifier := &stubNotifier{}
	svc := newTestService(repo, notifier)

	if _, err := svc.CheckReminders(context.Background()); err != nil {
		t.Fatalf("CheckReminders: %v", err)
	}

	n := notifier.got[0]
	if n.DaysSinceApplied != 9 {
		t.Errorf("days_since_applied = %d, want 9", n.DaysSinceApplied)
	}
	if n.Title != "Follow up with Stripe" {
		t.Errorf("title = %q", n.Title)
	}
	if !strings.Contains(n.Body, "9 days ago") {
		t.Errorf("body = %q, want it to mention how long it has been", n.Body)
	}
	if n.ReminderID != r.ID || n.ApplicationID != r.ApplicationID {
		t.Error("the notification must carry both ids so the dashboard can act on it")
	}
}

func TestDaysSinceFallsBackToReminderCreation(t *testing.T) {
	r := reminder(9)
	r.Application.AppliedAt = nil // never stamped
	r.CreatedAt = fixedNow.AddDate(0, 0, -4)

	repo := &stubRepo{due: []FollowUpReminder{r}}
	notifier := &stubNotifier{}
	svc := newTestService(repo, notifier)

	if _, err := svc.CheckReminders(context.Background()); err != nil {
		t.Fatalf("CheckReminders: %v", err)
	}

	if got := notifier.got[0].DaysSinceApplied; got != 4 {
		t.Errorf("days_since_applied = %d, want 4 from the reminder's creation", got)
	}
}

func TestAppliedPhrase(t *testing.T) {
	cases := map[int]string{-1: "today", 0: "today", 1: "yesterday", 12: "12 days ago"}
	for days, want := range cases {
		if got := appliedPhrase(days); got != want {
			t.Errorf("appliedPhrase(%d) = %q, want %q", days, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func labels(actions []SuggestedAction) []string {
	out := make([]string, 0, len(actions))
	for _, a := range actions {
		out = append(out, a.Label)
	}
	return out
}

func hasKind(actions []SuggestedAction, kind ActionKind) bool {
	for _, a := range actions {
		if a.Kind == kind {
			return true
		}
	}
	return false
}

func assertLabels(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("actions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("actions = %v, want %v", got, want)
		}
	}
}
