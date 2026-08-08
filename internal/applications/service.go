package applications

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/EngenMe/applymind-backend/internal/coverletters"
	"github.com/EngenMe/applymind-backend/pkg/ai"
)

// CoverLetters is the subset of the coverletters service this module needs.
// Declared here rather than depending on the concrete implementation so Create
// can be tested without S3 or a database, following the cvs.Storage precedent.
// coverletters.Service satisfies it as written, so cmd/api wires it in directly.
type CoverLetters interface {
	SaveText(ctx context.Context, in coverletters.SaveTextInput) (*coverletters.CoverLetter, error)
}

// Scorer is the subset of the ai package this module needs. *ai.Client satisfies
// it; tests supply a fake and never touch the network. Same precedent as
// CoverLetters above.
type Scorer interface {
	ScoreJob(ctx context.Context, in ai.ScoreInput) (*ai.Score, error)
}

// ProfileSummaries reads the user's profile summary — the thing a job is scored
// against. settings.Service satisfies it, so this module depends on one method
// rather than on the settings module's whole surface.
type ProfileSummaries interface {
	ProfileSummary(ctx context.Context) (string, error)
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
	// Complete finishes an In Progress application — Flow 2's "Mark as
	// Complete". It attaches the CV version and cover letter the external site
	// received, moves the application to Applied and starts the follow-up clock,
	// all in one call.
	Complete(ctx context.Context, id uuid.UUID, in CompleteInput) (*CompleteResult, error)
	Delete(ctx context.Context, id uuid.UUID) error
	// CheckDuplicate backs GET /applications/check-duplicate, letting the sidebar
	// warn before the user commits to saving.
	CheckDuplicate(ctx context.Context, q DuplicateQuery) (*DuplicateWarning, error)
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
	// DefaultScoreTimeout bounds the AI call inside a save. The flows budget
	// ~1–3 seconds for it and the user watches a spinner the whole time, so this
	// is a ceiling on how long the save can be held up, not an expected wait.
	DefaultScoreTimeout = 15 * time.Second
)

// FollowUpDueAt is the follow-up rule in one place: a fixed delay after the
// application was sent.
func FollowUpDueAt(appliedAt time.Time, delay time.Duration) time.Time {
	return appliedAt.Add(delay)
}

type service struct {
	repo          Repository
	coverLetters  CoverLetters
	scorer        Scorer
	profiles      ProfileSummaries
	followUpDelay time.Duration
	scoreTimeout  time.Duration
	logger        *slog.Logger
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

func WithLogger(l *slog.Logger) ServiceOption {
	return func(s *service) {
		if l != nil {
			s.logger = l
		}
	}
}

// WithScoring turns on AI job scoring. Both halves are required — a scorer with
// nothing to score against is useless — so they are set together and scoring
// stays off unless both arrive. Without this option the module behaves exactly
// as it did before: applications save, ai_score stays NULL.
func WithScoring(scorer Scorer, profiles ProfileSummaries) ServiceOption {
	return func(s *service) {
		if scorer != nil && profiles != nil {
			s.scorer = scorer
			s.profiles = profiles
		}
	}
}

// WithScoreTimeout caps how long a save will wait on the model.
func WithScoreTimeout(d time.Duration) ServiceOption {
	return func(s *service) {
		if d > 0 {
			s.scoreTimeout = d
		}
	}
}

func NewService(repo Repository, cl CoverLetters, opts ...ServiceOption) Service {
	s := &service{
		repo:          repo,
		coverLetters:  cl,
		followUpDelay: DefaultFollowUpDelay,
		scoreTimeout:  DefaultScoreTimeout,
		logger:        slog.Default(),
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
// The application, its first history row, its AI score and its follow-up
// reminder are written in one transaction. The cover letter is not: it belongs
// to the coverletters module, which owns its own writes, and its foreign key
// means it cannot be inserted before the application row is committed anyway. If
// saving it fails the application is deleted again, so a half-saved application
// never survives.
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
	// a job the user is only tracking, and Flow 2's partial save passes
	// In Progress.
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
	warning, err := s.CheckDuplicate(ctx, DuplicateQuery{Company: company, JobTitle: title, SiteID: &siteID})
	if err != nil {
		return nil, err
	}

	appliedAt := s.appliedAtFor(status, in.AppliedAt)

	var dueAt *time.Time
	if status == StatusApplied && appliedAt != nil {
		due := FollowUpDueAt(*appliedAt, s.followUpDelay)
		dueAt = &due
	}

	// Flow 1, steps 21–22. Deliberately outside the transaction below: this is a
	// network call to a third party and holding a database transaction open
	// across it would tie up a connection for seconds at a time. scoreJob never
	// returns an error — every failure is logged and comes back as nil, leaving
	// ai_score NULL and the save unaffected.
	score := s.scoreJob(ctx, company, title, in.JobDescription)

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

			// Flow 1 step 23: the score joins the same transaction as the row it
			// belongs to, so an application is never briefly visible unscored
			// when a score does exist. A failure here is a database failure, not
			// an AI failure — the fail-soft branch is above — and it fails the
			// save like any other write in this transaction would.
			if score != nil {
				scored, err := tx.SetAIScore(ctx, app.ID, score.Score, optionalString(score.Explanation))
				if err != nil {
					return err
				}
				app = scored
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

	// The job description can change here, which makes any existing score stale.
	// Re-scoring on edit is deliberately not done: it is not in this phase's
	// scope, and an edit is usually a typo fix rather than a new posting.
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

// Complete implements Flow 2, steps 28–34: the user has submitted the form on
// the company's own site and pressed "Mark as Complete" in the extension.
//
// It is one call rather than the three the extension would otherwise have to
// make (attach CV, save cover letter, move status) because the three belong
// together: an application that ends up Applied with the CV missing is a worse
// record than no record at all, and the offline queue would have to replay all
// three in order to avoid exactly that.
//
// The cover letter is saved first, before anything moves. It is the one write
// that cannot join the transaction — it belongs to the coverletters module — and
// unlike Create there is no compensating delete available here, because the
// application predates this call and must survive a failure. Doing it first
// means a failure leaves the application exactly as it was, still In Progress
// and still completable.
//
// Scoring deliberately does not run here. The job description was captured on
// LinkedIn and scored when the partial application was created; re-scoring the
// same description would spend a model call to arrive at the same answer.
func (s *service) Complete(ctx context.Context, id uuid.UUID, in CompleteInput) (*CompleteResult, error) {
	current, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	// Already sent. Completing twice is the double-submit case — the tab left
	// open in the background, the button pressed again, the same job opened
	// again next week — and the honest answer is that there is nothing left to
	// do, not that something went wrong.
	if current.Status == StatusApplied {
		return nil, ErrAlreadyCompleted
	}

	coverLetter := strings.TrimSpace(deref(in.CoverLetterText))
	if coverLetter != "" && s.coverLetters == nil {
		return nil, ErrCoverLettersUnavailable
	}

	// applied_at records when the application was actually sent. Write-once, so
	// an application that somehow already carries one keeps it.
	appliedAt := current.AppliedAt
	if appliedAt == nil {
		at := s.now().UTC()
		if in.CompletedAt != nil {
			at = in.CompletedAt.UTC()
		}
		appliedAt = &at
	}
	dueAt := FollowUpDueAt(*appliedAt, s.followUpDelay)

	if coverLetter != "" {
		if _, err := s.coverLetters.SaveText(
			ctx, coverletters.SaveTextInput{
				ApplicationID: id,
				BodyText:      coverLetter,
			},
		); err != nil {
			return nil, fmt.Errorf("applications: save cover letter: %w", err)
		}
	}

	var completed *Application
	err = s.repo.Tx(
		ctx, func(tx Repository) error {
			// Attaching the CV version reuses the captured-data update with every
			// other field left as it was: the external site never changes what
			// the job is, only what was sent to it.
			if in.CVVersionID != nil {
				if _, err := tx.Update(
					ctx, id, UpdateFields{
						CompanyName:    current.CompanyName,
						JobTitle:       current.JobTitle,
						JobDescription: current.JobDescription,
						JobURL:         current.JobURL,
						SiteID:         current.SiteID,
						CVVersionID:    in.CVVersionID,
					},
				); err != nil {
					return err
				}
			}

			app, err := tx.UpdateStatus(ctx, id, StatusApplied, appliedAt)
			if err != nil {
				return err
			}

			from := current.Status
			if _, err := tx.CreateStatusHistory(
				ctx, NewStatusHistory{
					ApplicationID: id,
					FromStatus:    &from,
					ToStatus:      StatusApplied,
					ChangedBy:     ChangeSourceUser,
					Note:          in.Note,
				},
			); err != nil {
				return err
			}

			if err := tx.EnsurePendingReminder(ctx, id, dueAt); err != nil {
				return err
			}

			completed = app
			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return &CompleteResult{Application: completed, FollowUpDueAt: &dueAt}, nil
}

func (s *service) Delete(ctx context.Context, id uuid.UUID) error {
	return s.repo.Delete(ctx, id)
}

// CheckDuplicate answers "have I been here before?" in three widening circles.
//
// The company match is exact and done in SQL. Everything after it is sorting:
// which of those existing applications look like this same job, and which of
// those were saved from a different site — the LinkedIn posting the user already
// applied to, now in front of them again on the company's own board.
//
// A company with no previous application returns (nil, nil), which every caller
// reads as "nothing to say".
func (s *service) CheckDuplicate(ctx context.Context, q DuplicateQuery) (*DuplicateWarning, error) {
	company := strings.TrimSpace(q.Company)
	if company == "" {
		return nil, ErrCompanyRequired
	}

	found, err := s.repo.FindByCompanyName(ctx, company)
	if err != nil {
		return nil, err
	}

	matches := make([]Application, 0, len(found))
	for _, app := range found {
		if q.ExcludeID != nil && app.ID == *q.ExcludeID {
			continue
		}
		matches = append(matches, app)
	}
	if len(matches) == 0 {
		return nil, nil
	}

	title := strings.TrimSpace(q.JobTitle)
	warning := &DuplicateWarning{CompanyName: company, JobTitle: title, Matches: matches}
	if title == "" {
		return warning, nil
	}

	// Knowing which site this save is for is what makes a match cross-site. It
	// is a nicety, not a requirement: an unresolvable site leaves CrossSite
	// empty rather than failing a check that exists to be informative.
	siteID := q.SiteID
	if siteID == nil && strings.TrimSpace(q.JobURL) != "" {
		if resolved, err := s.resolveSiteID(ctx, nil, strings.TrimSpace(q.JobURL)); err == nil {
			siteID = &resolved
		}
	}

	for _, match := range matches {
		if !TitlesLikelySame(title, match.JobTitle) {
			continue
		}
		warning.LikelySame = append(warning.LikelySame, match)
		if siteID != nil && match.SiteID != *siteID {
			warning.CrossSite = append(warning.CrossSite, match)
		}
	}
	return warning, nil
}

func (s *service) StatusHistory(ctx context.Context, id uuid.UUID) ([]StatusHistory, error) {
	if _, err := s.repo.Get(ctx, id); err != nil {
		return nil, err
	}
	return s.repo.ListStatusHistory(ctx, id)
}

// ---------------------------------------------------------------------------
// AI scoring
// ---------------------------------------------------------------------------

// scoreJob asks the model how well this job matches the user's profile.
//
// It returns nil rather than an error, on purpose: nothing that happens in here
// is allowed to stop an application being saved. Every branch that gives up logs
// why and leaves ai_score NULL, which the column is nullable to allow and which
// SetAIScore can fill in later.
//
// Reasons it gives up, all of them normal:
//   - scoring is not configured (no OpenAI key, so no scorer was wired in)
//   - the user has not written a profile summary yet, so there is nothing to
//     compare against and a score would be invented rather than judged
//   - the job description was empty — external-site saves often start that way
//   - the model errored, timed out, or answered with something unusable
func (s *service) scoreJob(ctx context.Context, company, title, jobDescription string) *ai.Score {
	if s.scorer == nil || s.profiles == nil {
		return nil
	}
	if strings.TrimSpace(jobDescription) == "" {
		s.logger.DebugContext(
			ctx,
			"applications: skipping ai score, no job description",
			slog.String("company", company),
		)
		return nil
	}

	summary, err := s.profiles.ProfileSummary(ctx)
	if err != nil {
		s.logger.WarnContext(
			ctx, "applications: skipping ai score, could not read profile summary",
			slog.String("error", err.Error()),
		)
		return nil
	}
	if strings.TrimSpace(summary) == "" {
		s.logger.InfoContext(
			ctx, "applications: skipping ai score, no profile summary set",
			slog.String("hint", "PUT /settings/profile-summary"),
		)
		return nil
	}

	scoreCtx, cancel := context.WithTimeout(ctx, s.scoreTimeout)
	defer cancel()

	score, err := s.scorer.ScoreJob(
		scoreCtx, ai.ScoreInput{
			CompanyName:    company,
			JobTitle:       title,
			JobDescription: jobDescription,
			ProfileSummary: summary,
		},
	)
	if err != nil {
		// Error, not warning: a save that silently loses its score is worth
		// noticing in the logs, even though the user never sees it.
		s.logger.ErrorContext(
			ctx, "applications: ai scoring failed, saving without a score",
			slog.String("company", company),
			slog.String("job_title", title),
			slog.String("error", err.Error()),
		)
		return nil
	}
	return score
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
