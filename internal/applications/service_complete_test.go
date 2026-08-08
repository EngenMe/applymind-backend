package applications

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/EngenMe/applymind-backend/internal/coverletters"
)

// Phase 13's own fakes. Named apart from the ones in service_test.go so both
// files can live in the package; if the existing fake repository grows the
// methods used here, delete this one and use that instead.

type p13Repo struct {
	app       *Application
	byCompany []Application
	site      *Site

	created   []NewApplication
	updates   []UpdateFields
	history   []NewStatusHistory
	reminders []time.Time
	dismissed int
}

func (r *p13Repo) Tx(ctx context.Context, fn func(Repository) error) error { return fn(r) }

func (r *p13Repo) Create(ctx context.Context, in NewApplication) (*Application, error) {
	r.created = append(r.created, in)
	r.app = &Application{
		ID:             in.ID,
		CompanyName:    in.CompanyName,
		JobTitle:       in.JobTitle,
		JobDescription: in.JobDescription,
		JobURL:         in.JobURL,
		SiteID:         in.SiteID,
		CVVersionID:    in.CVVersionID,
		Status:         in.Status,
		AppliedAt:      in.AppliedAt,
	}
	return r.snapshot(), nil
}

func (r *p13Repo) Get(ctx context.Context, id uuid.UUID) (*Application, error) {
	if r.app == nil || r.app.ID != id {
		return nil, ErrNotFound
	}
	return r.snapshot(), nil
}

func (r *p13Repo) List(ctx context.Context, f ListFilter) ([]Application, error) { return nil, nil }

func (r *p13Repo) Update(ctx context.Context, id uuid.UUID, in UpdateFields) (*Application, error) {
	if r.app == nil || r.app.ID != id {
		return nil, ErrNotFound
	}
	r.updates = append(r.updates, in)
	r.app.CompanyName = in.CompanyName
	r.app.JobTitle = in.JobTitle
	r.app.JobDescription = in.JobDescription
	r.app.JobURL = in.JobURL
	r.app.SiteID = in.SiteID
	r.app.CVVersionID = in.CVVersionID
	return r.snapshot(), nil
}

func (r *p13Repo) UpdateStatus(
	ctx context.Context,
	id uuid.UUID,
	status Status,
	appliedAt *time.Time,
) (*Application, error) {
	if r.app == nil || r.app.ID != id {
		return nil, ErrNotFound
	}
	r.app.Status = status
	// Mirrors UpdateApplicationStatus's COALESCE: applied_at is write-once.
	if r.app.AppliedAt == nil && appliedAt != nil {
		r.app.AppliedAt = appliedAt
	}
	return r.snapshot(), nil
}

func (r *p13Repo) Delete(ctx context.Context, id uuid.UUID) error {
	r.app = nil
	return nil
}

func (r *p13Repo) FindByCompanyName(ctx context.Context, company string) ([]Application, error) {
	return r.byCompany, nil
}

func (r *p13Repo) SetAIScore(
	ctx context.Context,
	id uuid.UUID,
	score float64,
	explanation *string,
) (*Application, error) {
	r.app.AIScore = &score
	r.app.AIScoreExplanation = explanation
	return r.snapshot(), nil
}

func (r *p13Repo) CreateStatusHistory(ctx context.Context, in NewStatusHistory) (*StatusHistory, error) {
	r.history = append(r.history, in)
	return &StatusHistory{ID: uuid.New(), ApplicationID: in.ApplicationID, ToStatus: in.ToStatus}, nil
}

func (r *p13Repo) ListStatusHistory(ctx context.Context, id uuid.UUID) ([]StatusHistory, error) {
	return nil, nil
}

func (r *p13Repo) EnsurePendingReminder(ctx context.Context, id uuid.UUID, dueAt time.Time) error {
	r.reminders = append(r.reminders, dueAt)
	return nil
}

func (r *p13Repo) DismissPendingReminders(ctx context.Context, id uuid.UUID) error {
	r.dismissed++
	return nil
}

func (r *p13Repo) FindSiteByDomain(ctx context.Context, domain string) (*Site, error) {
	return r.site, nil
}

func (r *p13Repo) snapshot() *Application {
	clone := *r.app
	return &clone
}

// p13CoverLetters fails on demand, so the ordering guarantee in Complete can be
// tested without S3.
type p13CoverLetters struct {
	saved []string
	err   error
}

func (c *p13CoverLetters) SaveText(
	ctx context.Context,
	in coverletters.SaveTextInput,
) (*coverletters.CoverLetter, error) {
	if c.err != nil {
		return nil, c.err
	}
	c.saved = append(c.saved, in.BodyText)
	return nil, nil
}

// ---------------------------------------------------------------------------
// Mark as Complete — Flow 2, steps 28–34
// ---------------------------------------------------------------------------

func inProgressRepo(id, siteID uuid.UUID) *p13Repo {
	return &p13Repo{
		app: &Application{
			ID:             id,
			CompanyName:    "Acme",
			JobTitle:       "Backend Engineer",
			JobDescription: "Go, Postgres, AWS.",
			JobURL:         "https://www.linkedin.com/jobs/view/123/",
			SiteID:         siteID,
			Status:         StatusInProgress,
		},
	}
}

func TestCompleteAppliesTheApplicationAndStartsTheFollowUpClock(t *testing.T) {
	now := time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC)
	id, siteID, cvVersionID := uuid.New(), uuid.New(), uuid.New()
	repo := inProgressRepo(id, siteID)
	letters := &p13CoverLetters{}
	svc := NewService(repo, letters, WithClock(func() time.Time { return now }))

	body := "Dear Acme, …"
	result, err := svc.Complete(
		context.Background(), id, CompleteInput{
			CVVersionID:     &cvVersionID,
			CoverLetterText: &body,
		},
	)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if result.Application.Status != StatusApplied {
		t.Fatalf("status = %q, want %q", result.Application.Status, StatusApplied)
	}
	if result.Application.AppliedAt == nil || !result.Application.AppliedAt.Equal(now) {
		t.Fatalf("applied_at = %v, want %v", result.Application.AppliedAt, now)
	}
	if result.Application.CVVersionID == nil || *result.Application.CVVersionID != cvVersionID {
		t.Fatalf("cv_version_id = %v, want %v", result.Application.CVVersionID, cvVersionID)
	}
	if len(letters.saved) != 1 || letters.saved[0] != body {
		t.Fatalf("cover letter saved = %v, want [%q]", letters.saved, body)
	}

	wantDue := now.Add(DefaultFollowUpDelay)
	if result.FollowUpDueAt == nil || !result.FollowUpDueAt.Equal(wantDue) {
		t.Fatalf("follow_up_due_at = %v, want %v", result.FollowUpDueAt, wantDue)
	}
	if len(repo.reminders) != 1 || !repo.reminders[0].Equal(wantDue) {
		t.Fatalf("reminders = %v, want one at %v", repo.reminders, wantDue)
	}

	if len(repo.history) != 1 {
		t.Fatalf("history rows = %d, want 1", len(repo.history))
	}
	entry := repo.history[0]
	if entry.FromStatus == nil || *entry.FromStatus != StatusInProgress || entry.ToStatus != StatusApplied {
		t.Fatalf("history = %v → %q, want %q → %q", entry.FromStatus, entry.ToStatus, StatusInProgress, StatusApplied)
	}
	if entry.ChangedBy != ChangeSourceUser {
		t.Fatalf("changed_by = %q, want %q", entry.ChangedBy, ChangeSourceUser)
	}

	// The external site never changes what the job is — only what was sent to it.
	if len(repo.updates) != 1 {
		t.Fatalf("captured-data updates = %d, want 1", len(repo.updates))
	}
	if repo.updates[0].JobTitle != "Backend Engineer" || repo.updates[0].JobURL != "https://www.linkedin.com/jobs/view/123/" {
		t.Fatalf("completion rewrote captured job data: %+v", repo.updates[0])
	}
}

func TestCompleteHonoursAnExplicitCompletedAt(t *testing.T) {
	now := time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC)
	sent := time.Date(2026, 8, 4, 17, 30, 0, 0, time.UTC)
	id, siteID := uuid.New(), uuid.New()
	repo := inProgressRepo(id, siteID)
	svc := NewService(repo, nil, WithClock(func() time.Time { return now }))

	result, err := svc.Complete(context.Background(), id, CompleteInput{CompletedAt: &sent})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if result.Application.AppliedAt == nil || !result.Application.AppliedAt.Equal(sent) {
		t.Fatalf("applied_at = %v, want the queued completion time %v", result.Application.AppliedAt, sent)
	}
	if want := sent.Add(DefaultFollowUpDelay); !result.FollowUpDueAt.Equal(want) {
		t.Fatalf("follow_up_due_at = %v, want %v", result.FollowUpDueAt, want)
	}
}

func TestCompleteRejectsAnApplicationThatHasAlreadyBeenSent(t *testing.T) {
	id, siteID := uuid.New(), uuid.New()
	repo := inProgressRepo(id, siteID)
	repo.app.Status = StatusApplied
	svc := NewService(repo, nil)

	if _, err := svc.Complete(context.Background(), id, CompleteInput{}); !errors.Is(err, ErrAlreadyCompleted) {
		t.Fatalf("error = %v, want ErrAlreadyCompleted", err)
	}
	if len(repo.reminders) != 0 || len(repo.history) != 0 {
		t.Fatal("a rejected completion still wrote to the database")
	}
}

func TestCompleteLeavesTheApplicationInProgressWhenTheCoverLetterFails(t *testing.T) {
	id, siteID := uuid.New(), uuid.New()
	repo := inProgressRepo(id, siteID)
	letters := &p13CoverLetters{err: errors.New("s3 unavailable")}
	svc := NewService(repo, letters)

	body := "Dear Acme, …"
	if _, err := svc.Complete(context.Background(), id, CompleteInput{CoverLetterText: &body}); err == nil {
		t.Fatal("Complete succeeded despite the cover letter failing")
	}

	// The whole point of saving the cover letter first: a failure must not leave
	// an application recorded as Applied with the letter missing.
	if repo.app.Status != StatusInProgress {
		t.Fatalf("status = %q, want it left at %q", repo.app.Status, StatusInProgress)
	}
	if len(repo.history) != 0 || len(repo.reminders) != 0 {
		t.Fatal("a failed completion still moved the application")
	}
}

// ---------------------------------------------------------------------------
// Duplicate detection — a warning, never a block
// ---------------------------------------------------------------------------

func TestCreateSavesTheApplicationEvenWhenItIsADuplicate(t *testing.T) {
	siteID := uuid.New()
	existing := Application{
		ID:          uuid.New(),
		CompanyName: "Acme",
		JobTitle:    "Backend Engineer",
		SiteID:      siteID,
		Status:      StatusApplied,
	}
	repo := &p13Repo{
		byCompany: []Application{existing},
		site:      &Site{ID: siteID, Name: "LinkedIn", Domain: "linkedin.com"},
	}
	svc := NewService(repo, nil)

	result, err := svc.Create(
		context.Background(), CreateInput{
			CompanyName: "Acme",
			JobTitle:    "Backend Engineer (Go)",
			JobURL:      "https://www.linkedin.com/jobs/view/456/",
		},
	)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The save is the point: a duplicate warning that stopped the write would
	// lose an application the user did in fact send.
	if result.Application == nil {
		t.Fatal("no application returned")
	}
	if len(repo.created) != 1 {
		t.Fatalf("applications inserted = %d, want 1", len(repo.created))
	}
	if result.Duplicate == nil {
		t.Fatal("no duplicate warning returned")
	}
	if len(result.Duplicate.Matches) != 1 || result.Duplicate.Matches[0].ID != existing.ID {
		t.Fatalf("matches = %v, want the existing application", result.Duplicate.Matches)
	}
	if len(result.Duplicate.LikelySame) != 1 {
		t.Fatalf("likely-same matches = %d, want 1", len(result.Duplicate.LikelySame))
	}
	// Same site, so it is a repeat rather than a cross-site duplicate.
	if len(result.Duplicate.CrossSite) != 0 {
		t.Fatalf("cross-site matches = %d, want 0", len(result.Duplicate.CrossSite))
	}
}

func TestCheckDuplicateFlagsTheSameJobFoundOnAnotherSite(t *testing.T) {
	linkedIn, greenhouse := uuid.New(), uuid.New()
	repo := &p13Repo{
		byCompany: []Application{
			{ID: uuid.New(), CompanyName: "Acme", JobTitle: "Senior Backend Engineer", SiteID: linkedIn},
			{ID: uuid.New(), CompanyName: "Acme", JobTitle: "Product Designer", SiteID: linkedIn},
		},
	}
	svc := NewService(repo, nil)

	warning, err := svc.CheckDuplicate(
		context.Background(), DuplicateQuery{
			Company:  "  acme ",
			JobTitle: "Senior Backend Engineer — Payments",
			SiteID:   &greenhouse,
		},
	)
	if err != nil {
		t.Fatalf("CheckDuplicate: %v", err)
	}
	if warning == nil {
		t.Fatal("no warning returned")
	}
	if len(warning.Matches) != 2 {
		t.Fatalf("company matches = %d, want 2", len(warning.Matches))
	}
	if len(warning.LikelySame) != 1 || warning.LikelySame[0].JobTitle != "Senior Backend Engineer" {
		t.Fatalf("likely-same = %v, want only the backend role", warning.LikelySame)
	}
	if len(warning.CrossSite) != 1 {
		t.Fatalf("cross-site = %d, want 1", len(warning.CrossSite))
	}
}

func TestCheckDuplicateWithoutATitleReportsOnlyCompanyMatches(t *testing.T) {
	repo := &p13Repo{
		byCompany: []Application{{ID: uuid.New(), CompanyName: "Acme", JobTitle: "Backend Engineer"}},
	}
	svc := NewService(repo, nil)

	warning, err := svc.CheckDuplicate(context.Background(), DuplicateQuery{Company: "Acme"})
	if err != nil {
		t.Fatalf("CheckDuplicate: %v", err)
	}
	if warning == nil || len(warning.Matches) != 1 {
		t.Fatalf("warning = %v, want one company match", warning)
	}
	if len(warning.LikelySame) != 0 {
		t.Fatal("claimed to recognise the same job with no title to compare")
	}
}

func TestCheckDuplicateIsSilentForANewCompany(t *testing.T) {
	svc := NewService(&p13Repo{}, nil)

	warning, err := svc.CheckDuplicate(
		context.Background(),
		DuplicateQuery{Company: "Acme", JobTitle: "Backend Engineer"},
	)
	if err != nil {
		t.Fatalf("CheckDuplicate: %v", err)
	}
	if warning != nil {
		t.Fatalf("warning = %v, want nil", warning)
	}
}
