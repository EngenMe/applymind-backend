package applications

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/EngenMe/applymind-backend/internal/coverletters"
)

// CoverLetters is the subset of the coverletters service this module needs.
// Declared here rather than depending on the concrete implementation so Create
// can be tested without S3 or a database, following the cvs.Storage precedent.
// coverletters.Service satisfies it as written, so cmd/api wires it in directly.
type CoverLetters interface {
	SaveText(ctx context.Context, in coverletters.SaveTextInput) (*coverletters.CoverLetter, error)
}

// Service is the business logic boundary for the applications module.
type Service interface {
	// Create saves an application, writes its first status history row and, when
	// it lands in Applied, schedules a follow-up reminder. A company-name
	// duplicate is reported in the result, never blocked.
	Create(ctx context.Context, in CreateInput) (*CreateResult, error)
	// Get returns an application with its status history loaded.
	Get(ctx context.Context, id uuid.UUID) (*Application, error)
	List(ctx context.Context, f ListFilter) ([]Application, error)
	// Update edits captured job data. It cannot move the status.
	Update(ctx context.Context, id uuid.UUID, in UpdateInput) (*Application, error)
	// UpdateStatus transitions the application and writes the audit row in the
	// same transaction, so a status can never change without a history entry.
	UpdateStatus(ctx context.Context, id uuid.UUID, in StatusUpdateInput) (*Application, error)
	Delete(ctx context.Context, id uuid.UUID) error
	// CheckDuplicate backs GET /applications/check-duplicate, letting the sidebar
	// warn before the user commits to saving.
	CheckDuplicate(ctx context.Context, company string) (*DuplicateWarning, error)
	StatusHistory(ctx context.Context, id uuid.UUID) ([]StatusHistory, error)
}

const (
	// DefaultFollowUpDelay is how long after applying an application is
	// considered to have gone quiet.
	DefaultFollowUpDelay = 7 * 24 * time.Hour
	// DefaultListLimit is the page size when the caller does not ask for one.
	DefaultListLimit = 50
	// MaxListLimit caps a caller-supplied page size.
	MaxListLimit = 200
)

// FollowUpDueAt is the follow-up rule in one place: a fixed delay after the
// application was sent.
func FollowUpDueAt(appliedAt time.Time, delay time.Duration) time.Time {
	return appliedAt.Add(delay)
}

type service struct {
	repo          Repository
	coverLetters  CoverLetters
	followUpDelay time.Duration
	now           func() time.Time // injectable for deterministic tests
	newID         func() uuid.UUID // injectable for deterministic tests
}

type ServiceOption func(*service)

func WithFollowUpDelay(d time.Duration) ServiceOption {
	return func(s *service) { s.followUpDelay = d }
}

func WithClock(f func() time.Time) ServiceOption {
	return func(s *service) { s.now = f }
}

func WithIDGenerator(f func() uuid.UUID) ServiceOption {
	return func(s *service) { s.newID = f }
}

func NewService(repo Repository, cl CoverLetters, opts ...ServiceOption) Service {
	s := &service{
		repo:          repo,
		coverLetters:  cl,
		followUpDelay: DefaultFollowUpDelay,
		now:           time.Now,
		newID:         uuid.New,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Create implements Flow 1, steps 17–24.
//
// The application, its first history row and its follow-up reminder are written
// in one transaction. The cover letter is not: it belongs to the coverletters
// module, which owns its own writes, and its foreign key means it cannot be
// inserted before the application row is committed anyway. If saving it fails
// the application is deleted again, so a half-saved application never survives.
func (s *service) Create(ctx context.Context, in CreateInput) (*CreateResult, error) {
	company := strings.TrimSpace(in.CompanyName)
	if company == "" {
		return nil, ErrCompanyRequired
	}
	title := strings.TrimSpace(in.JobTitle)
	if title == "" {
		return nil, ErrJobTitleRequired
	}
	jobURL := strings.TrimSpace(in.JobURL)
	if jobURL == "" {
		return nil, ErrJobURLRequired
	}

	// The sidebar's Save button means Applied; the dashboard can pass Saved for
	// a job the user is only tracking.
	status := in.Status
	if status == "" {
		status = StatusApplied
	}
	if !status.Valid() {
		return nil, ErrInvalidStatus
	}

	coverLetter := strings.TrimSpace(deref(in.CoverLetterText))
	if coverLetter != "" && s.coverLetters == nil {
		return nil, ErrCoverLettersUnavailable
	}

	siteID, err := s.resolveSiteID(ctx, in.SiteID, jobURL)
	if err != nil {
		return nil, err
	}

	// Flow 1, steps 19–20. A match is a warning the caller may act on; it never
	// stops the save. Embedding-based similarity is a later phase.
	warning, err := s.CheckDuplicate(ctx, company)
	if err != nil {
		return nil, err
	}

	appliedAt := s.appliedAtFor(status, in.AppliedAt)

	var dueAt *time.Time
	if status == StatusApplied && appliedAt != nil {
		due := FollowUpDueAt(*appliedAt, s.followUpDelay)
		dueAt = &due
	}

	// EXTENSION POINT (later phase): AI job scoring goes here, between the
	// duplicate check and the write — Flow 1 steps 21–22. It is fail-soft: on an
	// error or timeout, log it, leave ai_score NULL and carry on, because the
	// save must succeed without it. The columns already exist on applications.

	var created *Application
	err = s.repo.Tx(
		ctx, func(tx Repository) error {
			app, err := tx.Create(
				ctx, NewApplication{
					ID:             s.newID(),
					CompanyName:    company,
					JobTitle:       title,
					JobDescription: in.JobDescription,
					JobURL:         jobURL,
					SiteID:         siteID,
					CVVersionID:    in.CVVersionID,
					Status:         status,
					AppliedAt:      appliedAt,
				},
			)
			if err != nil {
				return err
			}

			// First transition: nothing precedes it, so from_status is NULL.
			if _, err := tx.CreateStatusHistory(
				ctx, NewStatusHistory{
					ApplicationID: app.ID,
					FromStatus:    nil,
					ToStatus:      status,
					ChangedBy:     ChangeSourceUser,
					Note:          in.Note,
				},
			); err != nil {
				return err
			}

			if dueAt != nil {
				if err := tx.EnsurePendingReminder(ctx, app.ID, *dueAt); err != nil {
					return err
				}
			}

			created = app
			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	if coverLetter != "" {
		if _, err := s.coverLetters.SaveText(
			ctx, coverletters.SaveTextInput{
				ApplicationID: created.ID,
				BodyText:      coverLetter,
			},
		); err != nil {
			// Compensating action for the one write that could not join the
			// transaction. The cascade takes the history row and reminder with it.
			_ = s.repo.Delete(ctx, created.ID)
			return nil, fmt.Errorf("applications: save cover letter: %w", err)
		}
	}

	return &CreateResult{Application: created, Duplicate: warning, FollowUpDueAt: dueAt}, nil
}

func (s *service) Get(ctx context.Context, id uuid.UUID) (*Application, error) {
	app, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	history, err := s.repo.ListStatusHistory(ctx, id)
	if err != nil {
		return nil, err
	}
	app.History = history
	return app, nil
}

func (s *service) List(ctx context.Context, f ListFilter) ([]Application, error) {
	if f.Status != nil && !f.Status.Valid() {
		return nil, ErrInvalidStatus
	}
	if f.Limit <= 0 {
		f.Limit = DefaultListLimit
	}
	if f.Limit > MaxListLimit {
		f.Limit = MaxListLimit
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	f.Company = trimmedOrNil(f.Company)
	f.Search = trimmedOrNil(f.Search)

	return s.repo.List(ctx, f)
}

func (s *service) Update(ctx context.Context, id uuid.UUID, in UpdateInput) (*Application, error) {
	company := strings.TrimSpace(in.CompanyName)
	if company == "" {
		return nil, ErrCompanyRequired
	}
	title := strings.TrimSpace(in.JobTitle)
	if title == "" {
		return nil, ErrJobTitleRequired
	}
	jobURL := strings.TrimSpace(in.JobURL)
	if jobURL == "" {
		return nil, ErrJobURLRequired
	}

	// Confirms the application exists before touching anything, so a bad id is a
	// 404 rather than a silent no-op.
	if _, err := s.repo.Get(ctx, id); err != nil {
		return nil, err
	}

	siteID, err := s.resolveSiteID(ctx, in.SiteID, jobURL)
	if err != nil {
		return nil, err
	}

	return s.repo.Update(
		ctx, id, UpdateFields{
			CompanyName:    company,
			JobTitle:       title,
			JobDescription: in.JobDescription,
			JobURL:         jobURL,
			SiteID:         siteID,
			CVVersionID:    in.CVVersionID,
		},
	)
}

func (s *service) UpdateStatus(ctx context.Context, id uuid.UUID, in StatusUpdateInput) (*Application, error) {
	if !in.Status.Valid() {
		return nil, ErrInvalidStatus
	}
	changedBy := in.ChangedBy
	if changedBy == "" {
		changedBy = ChangeSourceUser
	}
	if !changedBy.Valid() {
		return nil, ErrInvalidChangeSource
	}

	current, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if current.Status == in.Status {
		return nil, ErrSameStatus
	}

	// applied_at is write-once: it records when the application was actually
	// sent, so a later pass back through Applied does not move it.
	var appliedAt *time.Time
	if in.Status == StatusApplied && current.AppliedAt == nil {
		now := s.now().UTC()
		appliedAt = &now
	}

	var updated *Application
	err = s.repo.Tx(
		ctx, func(tx Repository) error {
			app, err := tx.UpdateStatus(ctx, id, in.Status, appliedAt)
			if err != nil {
				return err
			}

			from := current.Status
			if _, err := tx.CreateStatusHistory(
				ctx, NewStatusHistory{
					ApplicationID: id,
					FromStatus:    &from,
					ToStatus:      in.Status,
					ChangedBy:     changedBy,
					Note:          in.Note,
				},
			); err != nil {
				return err
			}

			// A follow-up reminder exists to chase silence. Entering Applied
			// starts the clock; leaving it means somebody responded, so there is
			// nothing left to chase.
			if in.Status == StatusApplied {
				at := app.AppliedAt
				if at == nil {
					now := s.now().UTC()
					at = &now
				}
				if err := tx.EnsurePendingReminder(ctx, id, FollowUpDueAt(*at, s.followUpDelay)); err != nil {
					return err
				}
			} else if err := tx.DismissPendingReminders(ctx, id); err != nil {
				return err
			}

			updated = app
			return nil
		},
	)
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func (s *service) Delete(ctx context.Context, id uuid.UUID) error {
	return s.repo.Delete(ctx, id)
}

func (s *service) CheckDuplicate(ctx context.Context, company string) (*DuplicateWarning, error) {
	name := strings.TrimSpace(company)
	if name == "" {
		return nil, ErrCompanyRequired
	}

	matches, err := s.repo.FindByCompanyName(ctx, name)
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, nil
	}
	return &DuplicateWarning{CompanyName: name, Matches: matches}, nil
}

func (s *service) StatusHistory(ctx context.Context, id uuid.UUID) ([]StatusHistory, error) {
	if _, err := s.repo.Get(ctx, id); err != nil {
		return nil, err
	}
	return s.repo.ListStatusHistory(ctx, id)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// resolveSiteID trusts an explicit site_id when the caller sends one, and
// otherwise derives the site from the job URL's host — Flow 1's payload carries
// the URL but never the site.
func (s *service) resolveSiteID(ctx context.Context, explicit *uuid.UUID, jobURL string) (uuid.UUID, error) {
	if explicit != nil {
		return *explicit, nil
	}

	domain, err := domainFromURL(jobURL)
	if err != nil {
		return uuid.Nil, ErrSiteUnresolvable
	}
	site, err := s.repo.FindSiteByDomain(ctx, domain)
	if err != nil {
		return uuid.Nil, err
	}
	if site == nil {
		return uuid.Nil, ErrSiteNotFound
	}
	return site.ID, nil
}

// appliedAtFor stamps the moment of application. An explicit value wins (the
// extension may be replaying a queued offline save), otherwise entering Applied
// means now, and any other status leaves it unset.
func (s *service) appliedAtFor(status Status, explicit *time.Time) *time.Time {
	if explicit != nil {
		at := explicit.UTC()
		return &at
	}
	if status == StatusApplied {
		at := s.now().UTC()
		return &at
	}
	return nil
}

// domainFromURL reduces a job URL to the bare host used in sites.domain:
// lowercased and without a leading www. A URL with no scheme is tolerated,
// because the DOM does not always hand over an absolute one.
func domainFromURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" {
		parsed, err = url.Parse("https://" + raw)
		if err != nil || parsed.Hostname() == "" {
			return "", fmt.Errorf("applications: %q has no host", raw)
		}
	}
	return strings.TrimPrefix(strings.ToLower(parsed.Hostname()), "www."), nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// trimmedOrNil drops a filter that is only whitespace, so `?company=` behaves as
// "no filter" rather than matching everything through a LIKE '%%'.
func trimmedOrNil(s *string) *string {
	if s == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*s)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}
