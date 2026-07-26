package applications

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/EngenMe/applymind-backend/internal/coverletters"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type reminder struct {
	dueAt     time.Time
	dismissed bool
}

// fakeRepo is an in-memory Repository. Tx runs fn against the same instance,
// which is enough to assert what a successful transaction wrote; rollback
// behaviour belongs to an integration test against a real database.
type fakeRepo struct {
	apps      map[uuid.UUID]*Application
	history   map[uuid.UUID][]StatusHistory
	reminders map[uuid.UUID][]*reminder
	sites     map[string]Site

	lastFilter ListFilter
	createErr  error
	txCalls    int
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		apps:      map[uuid.UUID]*Application{},
		history:   map[uuid.UUID][]StatusHistory{},
		reminders: map[uuid.UUID][]*reminder{},
		sites:     map[string]Site{},
	}
}

func (f *fakeRepo) Tx(ctx context.Context, fn func(Repository) error) error {
	f.txCalls++
	return fn(f)
}

func (f *fakeRepo) Create(_ context.Context, in NewApplication) (*Application, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	now := time.Date(2026, 5, 16, 9, 0, 0, 0, time.UTC)
	app := &Application{
		ID:             in.ID,
		CompanyName:    in.CompanyName,
		JobTitle:       in.JobTitle,
		JobDescription: in.JobDescription,
		JobURL:         in.JobURL,
		SiteID:         in.SiteID,
		CVVersionID:    in.CVVersionID,
		Status:         in.Status,
		AppliedAt:      in.AppliedAt,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	f.apps[in.ID] = app
	return copyApp(app), nil
}

func (f *fakeRepo) Get(_ context.Context, id uuid.UUID) (*Application, error) {
	app, ok := f.apps[id]
	if !ok {
		return nil, ErrNotFound
	}
	return copyApp(app), nil
}

func (f *fakeRepo) List(_ context.Context, filter ListFilter) ([]Application, error) {
	f.lastFilter = filter

	out := []Application{}
	for _, app := range f.apps {
		if filter.Status != nil && app.Status != *filter.Status {
			continue
		}
		if filter.SiteID != nil && app.SiteID != *filter.SiteID {
			continue
		}
		if filter.CVVersionID != nil &&
			(app.CVVersionID == nil || *app.CVVersionID != *filter.CVVersionID) {
			continue
		}
		if filter.Company != nil && !containsFold(app.CompanyName, *filter.Company) {
			continue
		}
		if filter.Search != nil &&
			!containsFold(app.CompanyName, *filter.Search) &&
			!containsFold(app.JobTitle, *filter.Search) {
			continue
		}
		effective := app.CreatedAt
		if app.AppliedAt != nil {
			effective = *app.AppliedAt
		}
		if filter.From != nil && effective.Before(*filter.From) {
			continue
		}
		if filter.To != nil && effective.After(*filter.To) {
			continue
		}
		out = append(out, *copyApp(app))
	}
	return out, nil
}

func (f *fakeRepo) Update(_ context.Context, id uuid.UUID, in UpdateFields) (*Application, error) {
	app, ok := f.apps[id]
	if !ok {
		return nil, ErrNotFound
	}
	app.CompanyName = in.CompanyName
	app.JobTitle = in.JobTitle
	app.JobDescription = in.JobDescription
	app.JobURL = in.JobURL
	app.SiteID = in.SiteID
	app.CVVersionID = in.CVVersionID
	return copyApp(app), nil
}

func (f *fakeRepo) UpdateStatus(
	_ context.Context,
	id uuid.UUID,
	status Status,
	appliedAt *time.Time,
) (*Application, error) {
	app, ok := f.apps[id]
	if !ok {
		return nil, ErrNotFound
	}
	app.Status = status
	if app.AppliedAt == nil && appliedAt != nil {
		app.AppliedAt = appliedAt
	}
	return copyApp(app), nil
}

func (f *fakeRepo) Delete(_ context.Context, id uuid.UUID) error {
	if _, ok := f.apps[id]; !ok {
		return ErrNotFound
	}
	delete(f.apps, id)
	delete(f.history, id)
	delete(f.reminders, id)
	return nil
}

func (f *fakeRepo) FindByCompanyName(_ context.Context, company string) ([]Application, error) {
	out := []Application{}
	want := strings.ToLower(strings.TrimSpace(company))
	for _, app := range f.apps {
		if strings.ToLower(strings.TrimSpace(app.CompanyName)) == want {
			out = append(out, *copyApp(app))
		}
	}
	return out, nil
}

func (f *fakeRepo) CreateStatusHistory(_ context.Context, in NewStatusHistory) (*StatusHistory, error) {
	entry := StatusHistory{
		ID:            uuid.New(),
		ApplicationID: in.ApplicationID,
		FromStatus:    in.FromStatus,
		ToStatus:      in.ToStatus,
		ChangedBy:     in.ChangedBy,
		Note:          in.Note,
		ChangedAt:     time.Now().UTC(),
	}
	f.history[in.ApplicationID] = append(f.history[in.ApplicationID], entry)
	return &entry, nil
}

func (f *fakeRepo) ListStatusHistory(_ context.Context, applicationID uuid.UUID) ([]StatusHistory, error) {
	return append([]StatusHistory{}, f.history[applicationID]...), nil
}

func (f *fakeRepo) EnsurePendingReminder(_ context.Context, applicationID uuid.UUID, dueAt time.Time) error {
	for _, r := range f.reminders[applicationID] {
		if !r.dismissed {
			return nil // one pending at a time, per the partial unique index
		}
	}
	f.reminders[applicationID] = append(f.reminders[applicationID], &reminder{dueAt: dueAt})
	return nil
}

func (f *fakeRepo) DismissPendingReminders(_ context.Context, applicationID uuid.UUID) error {
	for _, r := range f.reminders[applicationID] {
		r.dismissed = true
	}
	return nil
}

func (f *fakeRepo) FindSiteByDomain(_ context.Context, domain string) (*Site, error) {
	site, ok := f.sites[domain]
	if !ok {
		return nil, nil
	}
	return &site, nil
}

func (f *fakeRepo) pending(applicationID uuid.UUID) *reminder {
	for _, r := range f.reminders[applicationID] {
		if !r.dismissed {
			return r
		}
	}
	return nil
}

type fakeCoverLetters struct {
	saved map[uuid.UUID]string
	err   error
}

func (f *fakeCoverLetters) SaveText(_ context.Context, in coverletters.SaveTextInput) (
	*coverletters.CoverLetter,
	error,
) {
	if f.err != nil {
		return nil, f.err
	}
	if f.saved == nil {
		f.saved = map[uuid.UUID]string{}
	}
	f.saved[in.ApplicationID] = in.BodyText
	return &coverletters.CoverLetter{ApplicationID: in.ApplicationID, Kind: coverletters.KindText}, nil
}

func copyApp(app *Application) *Application {
	clone := *app
	return &clone
}

func containsFold(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

var (
	fixedNow  = time.Date(2026, 5, 16, 9, 0, 0, 0, time.UTC)
	linkedIn  = Site{ID: uuid.MustParse("11111111-1111-1111-1111-111111111111"), Name: "LinkedIn", Domain: "linkedin.com"}
	newAppID  = uuid.MustParse("22222222-2222-2222-2222-222222222222")
	cvVersion = uuid.MustParse("33333333-3333-3333-3333-333333333333")
)

func newTestService(t *testing.T) (*fakeRepo, *fakeCoverLetters, Service) {
	t.Helper()

	repo := newFakeRepo()
	repo.sites[linkedIn.Domain] = linkedIn
	cl := &fakeCoverLetters{}

	svc := NewService(
		repo, cl,
		WithClock(func() time.Time { return fixedNow }),
		WithIDGenerator(func() uuid.UUID { return newAppID }),
	)
	return repo, cl, svc
}

func validInput() CreateInput {
	return CreateInput{
		CompanyName:    "  Stripe ",
		JobTitle:       "Senior Backend Engineer",
		JobDescription: "Go, Postgres, AWS.",
		JobURL:         "https://www.linkedin.com/jobs/view/4012345678",
		CVVersionID:    &cvVersion,
	}
}

// ---------------------------------------------------------------------------
// Create
// ---------------------------------------------------------------------------

func TestCreateSuccess(t *testing.T) {
	repo, cl, svc := newTestService(t)

	in := validInput()
	body := "Dear hiring manager..."
	in.CoverLetterText = &body

	result, err := svc.Create(context.Background(), in)
	if err != nil {
		t.Fatalf("Create: unexpected error: %v", err)
	}

	app := result.Application
	if app.CompanyName != "Stripe" {
		t.Errorf("company name = %q, want it trimmed to %q", app.CompanyName, "Stripe")
	}
	if app.Status != StatusApplied {
		t.Errorf("status = %q, want the default %q", app.Status, StatusApplied)
	}
	// site_id is not in the Flow 1 payload, so it comes from the job url host.
	if app.SiteID != linkedIn.ID {
		t.Errorf("site id = %v, want %v resolved from the url", app.SiteID, linkedIn.ID)
	}
	if app.AppliedAt == nil || !app.AppliedAt.Equal(fixedNow) {
		t.Errorf("applied at = %v, want %v", app.AppliedAt, fixedNow)
	}
	if result.Duplicate != nil {
		t.Errorf("duplicate warning = %+v, want none for a first application", result.Duplicate)
	}
	if cl.saved[app.ID] != body {
		t.Errorf("cover letter = %q, want %q", cl.saved[app.ID], body)
	}
	if repo.txCalls != 1 {
		t.Errorf("transactions = %d, want the write to happen in exactly 1", repo.txCalls)
	}
}

func TestCreateWritesFirstHistoryRow(t *testing.T) {
	repo, _, svc := newTestService(t)

	result, err := svc.Create(context.Background(), validInput())
	if err != nil {
		t.Fatalf("Create: unexpected error: %v", err)
	}

	history := repo.history[result.Application.ID]
	if len(history) != 1 {
		t.Fatalf("history rows = %d, want 1", len(history))
	}
	if history[0].FromStatus != nil {
		t.Errorf("from status = %v, want nil on the first transition", *history[0].FromStatus)
	}
	if history[0].ToStatus != StatusApplied {
		t.Errorf("to status = %q, want %q", history[0].ToStatus, StatusApplied)
	}
	if history[0].ChangedBy != ChangeSourceUser {
		t.Errorf("changed by = %q, want %q", history[0].ChangedBy, ChangeSourceUser)
	}
}

func TestCreateReturnsDuplicateWarningWithoutBlocking(t *testing.T) {
	repo, _, svc := newTestService(t)

	existing := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	repo.apps[existing] = &Application{
		ID:          existing,
		CompanyName: "stripe", // different case on purpose
		JobTitle:    "Backend Engineer",
		SiteID:      linkedIn.ID,
		Status:      StatusApplied,
		CreatedAt:   fixedNow,
	}

	result, err := svc.Create(context.Background(), validInput())
	if err != nil {
		t.Fatalf("Create: a duplicate must not block the save, got error: %v", err)
	}
	if result.Duplicate == nil {
		t.Fatal("duplicate warning = nil, want the existing Stripe application reported")
	}
	if len(result.Duplicate.Matches) != 1 || result.Duplicate.Matches[0].ID != existing {
		t.Errorf("matches = %+v, want the existing application", result.Duplicate.Matches)
	}
	if _, ok := repo.apps[result.Application.ID]; !ok {
		t.Error("the new application was not saved; the warning must not block")
	}
}

func TestCreateSchedulesFollowUp(t *testing.T) {
	repo, _, svc := newTestService(t)

	result, err := svc.Create(context.Background(), validInput())
	if err != nil {
		t.Fatalf("Create: unexpected error: %v", err)
	}

	want := fixedNow.Add(DefaultFollowUpDelay)
	if result.FollowUpDueAt == nil || !result.FollowUpDueAt.Equal(want) {
		t.Errorf("follow up due at = %v, want %v (7 days after applying)", result.FollowUpDueAt, want)
	}
	pending := repo.pending(result.Application.ID)
	if pending == nil {
		t.Fatal("no pending reminder was written")
	}
	if !pending.dueAt.Equal(want) {
		t.Errorf("reminder due at = %v, want %v", pending.dueAt, want)
	}
}

func TestCreateSavedStatusSchedulesNothing(t *testing.T) {
	repo, _, svc := newTestService(t)

	in := validInput()
	in.Status = StatusSaved

	result, err := svc.Create(context.Background(), in)
	if err != nil {
		t.Fatalf("Create: unexpected error: %v", err)
	}
	if result.Application.AppliedAt != nil {
		t.Errorf("applied at = %v, want nil for a Saved application", result.Application.AppliedAt)
	}
	if result.FollowUpDueAt != nil {
		t.Errorf("follow up due at = %v, want nil — nothing has been sent yet", result.FollowUpDueAt)
	}
	if repo.pending(result.Application.ID) != nil {
		t.Error("a reminder was scheduled for an application that was never sent")
	}
}

func TestCreateValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*CreateInput)
		want   error
	}{
		{"no company", func(in *CreateInput) { in.CompanyName = "   " }, ErrCompanyRequired},
		{"no title", func(in *CreateInput) { in.JobTitle = "" }, ErrJobTitleRequired},
		{"no url", func(in *CreateInput) { in.JobURL = "" }, ErrJobURLRequired},
		{"unknown status", func(in *CreateInput) { in.Status = "Pending" }, ErrInvalidStatus},
		{"unregistered site", func(in *CreateInput) { in.JobURL = "https://jobs.example.com/1" }, ErrSiteNotFound},
		{"url with no host", func(in *CreateInput) { in.JobURL = "not a url" }, ErrSiteUnresolvable},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, svc := newTestService(t)
			in := validInput()
			tc.mutate(&in)

			if _, err := svc.Create(context.Background(), in); !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestCreateExplicitSiteIDWins(t *testing.T) {
	_, _, svc := newTestService(t)

	other := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	in := validInput()
	in.SiteID = &other

	result, err := svc.Create(context.Background(), in)
	if err != nil {
		t.Fatalf("Create: unexpected error: %v", err)
	}
	if result.Application.SiteID != other {
		t.Errorf("site id = %v, want the explicitly supplied %v", result.Application.SiteID, other)
	}
}

func TestCreateRollsBackWhenCoverLetterFails(t *testing.T) {
	repo, cl, svc := newTestService(t)
	cl.err = errors.New("s3 unavailable")

	in := validInput()
	body := "Dear hiring manager..."
	in.CoverLetterText = &body

	if _, err := svc.Create(context.Background(), in); err == nil {
		t.Fatal("Create: want an error when the cover letter cannot be saved")
	}
	if len(repo.apps) != 0 {
		t.Errorf("applications left behind = %d, want the half-saved one removed", len(repo.apps))
	}
}

// ---------------------------------------------------------------------------
// Status updates
// ---------------------------------------------------------------------------

func createFixture(t *testing.T, svc Service) *Application {
	t.Helper()
	result, err := svc.Create(context.Background(), validInput())
	if err != nil {
		t.Fatalf("Create fixture: %v", err)
	}
	return result.Application
}

func TestUpdateStatusWritesHistory(t *testing.T) {
	repo, _, svc := newTestService(t)
	app := createFixture(t, svc)

	note := "recruiter emailed"
	updated, err := svc.UpdateStatus(
		context.Background(), app.ID,
		StatusUpdateInput{Status: StatusInterviewing, Note: &note},
	)
	if err != nil {
		t.Fatalf("UpdateStatus: unexpected error: %v", err)
	}
	if updated.Status != StatusInterviewing {
		t.Errorf("status = %q, want %q", updated.Status, StatusInterviewing)
	}

	history := repo.history[app.ID]
	if len(history) != 2 {
		t.Fatalf("history rows = %d, want 2 (create + transition)", len(history))
	}
	latest := history[1]
	if latest.FromStatus == nil || *latest.FromStatus != StatusApplied {
		t.Errorf("from status = %v, want %q", latest.FromStatus, StatusApplied)
	}
	if latest.ToStatus != StatusInterviewing {
		t.Errorf("to status = %q, want %q", latest.ToStatus, StatusInterviewing)
	}
	if latest.Note == nil || *latest.Note != note {
		t.Errorf("note = %v, want %q", latest.Note, note)
	}
	if latest.ChangedBy != ChangeSourceUser {
		t.Errorf("changed by = %q, want the default %q", latest.ChangedBy, ChangeSourceUser)
	}
}

func TestUpdateStatusDismissesPendingReminder(t *testing.T) {
	repo, _, svc := newTestService(t)
	app := createFixture(t, svc)

	if repo.pending(app.ID) == nil {
		t.Fatal("fixture should have left a pending reminder")
	}

	if _, err := svc.UpdateStatus(
		context.Background(), app.ID,
		StatusUpdateInput{Status: StatusAcknowledged},
	); err != nil {
		t.Fatalf("UpdateStatus: unexpected error: %v", err)
	}

	if repo.pending(app.ID) != nil {
		t.Error("reminder still pending; the company has responded, so there is nothing to chase")
	}
}

func TestUpdateStatusToAppliedStampsAndSchedules(t *testing.T) {
	repo, _, svc := newTestService(t)

	in := validInput()
	in.Status = StatusSaved
	result, err := svc.Create(context.Background(), in)
	if err != nil {
		t.Fatalf("Create: unexpected error: %v", err)
	}
	app := result.Application

	updated, err := svc.UpdateStatus(context.Background(), app.ID, StatusUpdateInput{Status: StatusApplied})
	if err != nil {
		t.Fatalf("UpdateStatus: unexpected error: %v", err)
	}
	if updated.AppliedAt == nil || !updated.AppliedAt.Equal(fixedNow) {
		t.Errorf("applied at = %v, want it stamped as %v", updated.AppliedAt, fixedNow)
	}

	pending := repo.pending(app.ID)
	if pending == nil {
		t.Fatal("no reminder scheduled after moving to Applied")
	}
	if want := fixedNow.Add(DefaultFollowUpDelay); !pending.dueAt.Equal(want) {
		t.Errorf("reminder due at = %v, want %v", pending.dueAt, want)
	}
}

func TestUpdateStatusRejectsNoOpAndUnknownValues(t *testing.T) {
	_, _, svc := newTestService(t)
	app := createFixture(t, svc)

	if _, err := svc.UpdateStatus(
		context.Background(), app.ID,
		StatusUpdateInput{Status: StatusApplied},
	); !errors.Is(err, ErrSameStatus) {
		t.Errorf("error = %v, want %v for a transition that changes nothing", err, ErrSameStatus)
	}

	if _, err := svc.UpdateStatus(
		context.Background(), app.ID,
		StatusUpdateInput{Status: "Ghosted"},
	); !errors.Is(err, ErrInvalidStatus) {
		t.Errorf("error = %v, want %v", err, ErrInvalidStatus)
	}

	if _, err := svc.UpdateStatus(
		context.Background(), app.ID,
		StatusUpdateInput{Status: StatusGhost, ChangedBy: "cron"},
	); !errors.Is(err, ErrInvalidChangeSource) {
		t.Errorf("error = %v, want %v", err, ErrInvalidChangeSource)
	}
}

func TestUpdateStatusUnknownApplication(t *testing.T) {
	_, _, svc := newTestService(t)

	_, err := svc.UpdateStatus(context.Background(), uuid.New(), StatusUpdateInput{Status: StatusRejected})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want %v", err, ErrNotFound)
	}
}

// ---------------------------------------------------------------------------
// Follow-up calculation
// ---------------------------------------------------------------------------

func TestFollowUpDueAt(t *testing.T) {
	applied := time.Date(2026, 5, 16, 9, 30, 0, 0, time.UTC)

	if got, want := FollowUpDueAt(applied, DefaultFollowUpDelay),
		time.Date(2026, 5, 23, 9, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("default delay: got %v, want %v", got, want)
	}
	if got, want := FollowUpDueAt(applied, 48*time.Hour),
		time.Date(2026, 5, 18, 9, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("custom delay: got %v, want %v", got, want)
	}
}

func TestCreateHonoursConfiguredFollowUpDelay(t *testing.T) {
	repo := newFakeRepo()
	repo.sites[linkedIn.Domain] = linkedIn
	svc := NewService(
		repo, &fakeCoverLetters{},
		WithClock(func() time.Time { return fixedNow }),
		WithIDGenerator(func() uuid.UUID { return newAppID }),
		WithFollowUpDelay(3*24*time.Hour),
	)

	result, err := svc.Create(context.Background(), validInput())
	if err != nil {
		t.Fatalf("Create: unexpected error: %v", err)
	}
	if want := fixedNow.Add(3 * 24 * time.Hour); !result.FollowUpDueAt.Equal(want) {
		t.Errorf("follow up due at = %v, want %v", result.FollowUpDueAt, want)
	}
}

// ---------------------------------------------------------------------------
// List / filter / search
// ---------------------------------------------------------------------------

func seedForListing(repo *fakeRepo) {
	mk := func(id, company, title string, status Status, applied time.Time, cv *uuid.UUID) {
		u := uuid.MustParse(id)
		at := applied
		repo.apps[u] = &Application{
			ID:          u,
			CompanyName: company,
			JobTitle:    title,
			SiteID:      linkedIn.ID,
			CVVersionID: cv,
			Status:      status,
			AppliedAt:   &at,
			CreatedAt:   applied,
		}
	}

	mk("aaaaaaaa-0000-0000-0000-000000000001", "Stripe", "Senior Backend Engineer",
		StatusApplied, time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC), &cvVersion)
	mk("aaaaaaaa-0000-0000-0000-000000000002", "Monzo", "Backend Engineer",
		StatusInterviewing, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC), nil)
	mk("aaaaaaaa-0000-0000-0000-000000000003", "Stripe", "Platform Engineer",
		StatusRejected, time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC), &cvVersion)
}

func TestListFilterCombinations(t *testing.T) {
	statusApplied := StatusApplied
	site := linkedIn.ID
	company := "Stripe"
	search := "backend"
	from := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name   string
		filter ListFilter
		want   int
	}{
		{"no filters", ListFilter{}, 3},
		{"by status", ListFilter{Status: &statusApplied}, 1},
		{"by company", ListFilter{Company: &company}, 2},
		{"by cv version", ListFilter{CVVersionID: &cvVersion}, 2},
		{"by site", ListFilter{SiteID: &site}, 3},
		{"by date range", ListFilter{From: &from, To: &to}, 2},
		{"search on title", ListFilter{Search: &search}, 2},
		{"company and date range", ListFilter{Company: &company, From: &from, To: &to}, 1},
		{"search and status", ListFilter{Search: &search, Status: &statusApplied}, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, _, svc := newTestService(t)
			seedForListing(repo)

			got, err := svc.List(context.Background(), tc.filter)
			if err != nil {
				t.Fatalf("List: unexpected error: %v", err)
			}
			if len(got) != tc.want {
				t.Errorf("results = %d, want %d", len(got), tc.want)
			}
		})
	}
}

func TestListClampsPaging(t *testing.T) {
	repo, _, svc := newTestService(t)

	if _, err := svc.List(context.Background(), ListFilter{Limit: 0, Offset: -5}); err != nil {
		t.Fatalf("List: unexpected error: %v", err)
	}
	if repo.lastFilter.Limit != DefaultListLimit {
		t.Errorf("limit = %d, want the default %d", repo.lastFilter.Limit, DefaultListLimit)
	}
	if repo.lastFilter.Offset != 0 {
		t.Errorf("offset = %d, want a negative offset clamped to 0", repo.lastFilter.Offset)
	}

	if _, err := svc.List(context.Background(), ListFilter{Limit: 5000}); err != nil {
		t.Fatalf("List: unexpected error: %v", err)
	}
	if repo.lastFilter.Limit != MaxListLimit {
		t.Errorf("limit = %d, want it capped at %d", repo.lastFilter.Limit, MaxListLimit)
	}
}

func TestListRejectsUnknownStatus(t *testing.T) {
	_, _, svc := newTestService(t)

	unknown := Status("Interviewd")
	if _, err := svc.List(context.Background(), ListFilter{Status: &unknown}); !errors.Is(err, ErrInvalidStatus) {
		t.Errorf("error = %v, want %v", err, ErrInvalidStatus)
	}
}

func TestListDropsBlankFilters(t *testing.T) {
	repo, _, svc := newTestService(t)

	blank := "   "
	if _, err := svc.List(context.Background(), ListFilter{Company: &blank, Search: &blank}); err != nil {
		t.Fatalf("List: unexpected error: %v", err)
	}
	if repo.lastFilter.Company != nil || repo.lastFilter.Search != nil {
		t.Error("whitespace-only filters should be dropped, not passed through as LIKE '%%'")
	}
}

// ---------------------------------------------------------------------------
// Duplicate check, get, update, delete
// ---------------------------------------------------------------------------

func TestCheckDuplicate(t *testing.T) {
	repo, _, svc := newTestService(t)
	seedForListing(repo)

	warning, err := svc.CheckDuplicate(context.Background(), "  stripe  ")
	if err != nil {
		t.Fatalf("CheckDuplicate: unexpected error: %v", err)
	}
	if warning == nil || len(warning.Matches) != 2 {
		t.Fatalf("warning = %+v, want 2 matches ignoring case and whitespace", warning)
	}

	none, err := svc.CheckDuplicate(context.Background(), "Vercel")
	if err != nil {
		t.Fatalf("CheckDuplicate: unexpected error: %v", err)
	}
	if none != nil {
		t.Errorf("warning = %+v, want nil for a company never applied to", none)
	}

	if _, err := svc.CheckDuplicate(context.Background(), " "); !errors.Is(err, ErrCompanyRequired) {
		t.Errorf("error = %v, want %v", err, ErrCompanyRequired)
	}
}

func TestGetLoadsHistory(t *testing.T) {
	_, _, svc := newTestService(t)
	app := createFixture(t, svc)

	got, err := svc.Get(context.Background(), app.ID)
	if err != nil {
		t.Fatalf("Get: unexpected error: %v", err)
	}
	if len(got.History) != 1 {
		t.Errorf("history entries = %d, want 1", len(got.History))
	}
}

func TestUpdateDoesNotTouchStatus(t *testing.T) {
	repo, _, svc := newTestService(t)
	app := createFixture(t, svc)

	updated, err := svc.Update(
		context.Background(), app.ID, UpdateInput{
			CompanyName:    "Stripe Inc",
			JobTitle:       "Staff Backend Engineer",
			JobDescription: "Updated description",
			JobURL:         app.JobURL,
		},
	)
	if err != nil {
		t.Fatalf("Update: unexpected error: %v", err)
	}
	if updated.JobTitle != "Staff Backend Engineer" {
		t.Errorf("job title = %q, want it updated", updated.JobTitle)
	}
	if updated.Status != StatusApplied {
		t.Errorf("status = %q, want it untouched by an edit", updated.Status)
	}
	if len(repo.history[app.ID]) != 1 {
		t.Errorf("history rows = %d, want an edit to add none", len(repo.history[app.ID]))
	}
}

func TestDeleteUnknownApplication(t *testing.T) {
	_, _, svc := newTestService(t)

	if err := svc.Delete(context.Background(), uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want %v", err, ErrNotFound)
	}
}
