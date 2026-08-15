package applications

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Domain models. No database or HTTP tags — mapping lives in repository.go and
// handler.go respectively, matching the cvs and coverletters modules.

// Status mirrors the application_status enum.
type Status string

const (
	StatusSaved Status = "Saved"
	// StatusInProgress is an application that has been started but not sent:
	// Flow 2 writes it before the browser leaves LinkedIn for the company's own
	// site, and "Mark as Complete" moves it on to Applied.
	StatusInProgress         Status = "In Progress"
	StatusApplied            Status = "Applied"
	StatusAcknowledged       Status = "Acknowledged"
	StatusInReview           Status = "In Review"
	StatusInterviewScheduled Status = "Interview Scheduled"
	StatusInterviewing       Status = "Interviewing"
	StatusOfferReceived      Status = "Offer Received"
	StatusAccepted           Status = "Accepted"
	StatusRejected           Status = "Rejected"
	StatusWithdrawn          Status = "Withdrawn"
	StatusGhost              Status = "Ghost"
)

// statusOrder is both the validity set and the order the dashboard should offer
// them in. The database enum is the real authority; this guards the boundary so
// a bad value becomes a 400 rather than a 500 from PostgreSQL.
var statusOrder = []Status{
	StatusSaved,
	StatusInProgress,
	StatusApplied,
	StatusAcknowledged,
	StatusInReview,
	StatusInterviewScheduled,
	StatusInterviewing,
	StatusOfferReceived,
	StatusAccepted,
	StatusRejected,
	StatusWithdrawn,
	StatusGhost,
}

// Valid reports whether s is a member of the application_status enum.
func (s Status) Valid() bool {
	for _, known := range statusOrder {
		if s == known {
			return true
		}
	}
	return false
}

// AllStatuses returns every status in dashboard order.
func AllStatuses() []Status {
	out := make([]Status, len(statusOrder))
	copy(out, statusOrder)
	return out
}

// ChangeSource mirrors the status_change_source enum: who moved the application.
type ChangeSource string

const (
	// ChangeSourceUser is a status the user set, from the sidebar or dashboard.
	ChangeSourceUser ChangeSource = "user"
	// ChangeSourceSystem is a status the backend set on its own — currently only
	// the scheduler marking a silent application as Ghost.
	ChangeSourceSystem ChangeSource = "system"
)

func (c ChangeSource) Valid() bool {
	return c == ChangeSourceUser || c == ChangeSourceSystem
}

// Application is one job application. History holds its status trail when it has
// been loaded; it is nil when the caller did not ask for it (same convention as
// cvs.CV.Versions).
//
// AIScore and AIScoreExplanation hold the GPT-4o-mini job-match score. Both are
// nullable and stay nil when scoring is switched off, when no profile summary
// has been set, or when the model call failed — a save never depends on them.
// They are written by Repository.SetAIScore, from service.Create.
type Application struct {
	ID                 uuid.UUID
	CompanyName        string
	JobTitle           string
	JobDescription     string
	JobURL             string
	SiteID             uuid.UUID
	CVVersionID        *uuid.UUID
	Status             Status
	AIScore            *float64
	AIScoreExplanation *string
	AppliedAt          *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
	History            []StatusHistory
}

// StatusHistory is one row of the append-only audit trail. FromStatus is nil on
// the first entry, which is written when the application is created.
type StatusHistory struct {
	ID            uuid.UUID
	ApplicationID uuid.UUID
	FromStatus    *Status
	ToStatus      Status
	ChangedBy     ChangeSource
	Note          *string
	ChangedAt     time.Time
}

// Site is a read-only projection of a sites row, used only to turn a job URL
// into a site_id. Deliberately not the sites module's own domain type — this
// module only reads it.
type Site struct {
	ID     uuid.UUID
	Name   string
	Domain string
}

// CreateInput is a save arriving from the extension sidebar (Flow 1, step 17)
// or from the dashboard.
//
// SiteID is optional: when the caller does not send one the service derives it
// from the JobURL host, because Flow 1's payload carries the URL but not the
// site. Status defaults to Applied, which is what the sidebar Save button means.
// CoverLetterText is the textarea the extension captured; file cover letters go
// to the coverletters module directly after this call returns.
type CreateInput struct {
	CompanyName     string
	JobTitle        string
	JobDescription  string
	JobURL          string
	SiteID          *uuid.UUID
	CVVersionID     *uuid.UUID
	Status          Status
	AppliedAt       *time.Time
	CoverLetterText *string
	Note            *string
}

// UpdateInput is a full edit of the captured job data. It deliberately cannot
// move the status — that goes through UpdateStatus so no transition can be made
// without an audit row.
type UpdateInput struct {
	CompanyName    string
	JobTitle       string
	JobDescription string
	JobURL         string
	SiteID         *uuid.UUID
	CVVersionID    *uuid.UUID
}

// StatusUpdateInput is a single transition. ChangedBy defaults to user.
type StatusUpdateInput struct {
	Status    Status
	Note      *string
	ChangedBy ChangeSource
}

// CompleteInput finishes an application started on one site and submitted on
// another — Flow 2, steps 28–33. It is the one call that carries everything the
// external site produced: which CV version went with it, the cover letter that
// was typed into the company's own form, and the moment it was sent.
//
// Every field is optional. A completion with nothing attached is still a
// completion: it means the user pressed the button and the application is out.
// CompletedAt is honoured only when the application has no applied_at yet —
// applied_at is write-once, as UpdateApplicationStatus's COALESCE enforces.
type CompleteInput struct {
	CVVersionID     *uuid.UUID
	CoverLetterText *string
	Note            *string
	CompletedAt     *time.Time
}

// CreateResult is what a save produced.
//
// Duplicate is non-nil when another application already exists for this company.
// It is a warning only: the application is created regardless, and the caller
// decides what to tell the user. FollowUpDueAt is set when the save landed in
// Applied and a pending reminder was scheduled.
type CreateResult struct {
	Application   *Application
	Duplicate     *DuplicateWarning
	FollowUpDueAt *time.Time
}

// CompleteResult is what "Mark as Complete" produced. FollowUpDueAt is always
// set: completing means the application is now Applied, which is exactly when
// the follow-up clock starts.
type CompleteResult struct {
	Application   *Application
	FollowUpDueAt *time.Time
}

// DuplicateQuery is one duplicate check. Company is required; everything else
// sharpens the answer rather than filtering it.
//
// JobTitle turns "you have applied here before" into "you may have already
// applied to this exact job", which is the difference between a note and a
// warning worth stopping for. SiteID (or JobURL, from which the site is derived)
// is what makes a match cross-site: the same posting on LinkedIn and on the
// company's own board is the case the user most wants catching.
type DuplicateQuery struct {
	Company  string
	JobTitle string
	SiteID   *uuid.UUID
	JobURL   string
	// ExcludeID keeps an application from matching itself when the check runs
	// on behalf of an edit.
	ExcludeID *uuid.UUID
}

// DuplicateWarning lists the existing applications that matched, in three
// widening circles: LikelySame is the same job, CrossSite is the same job saved
// under a different site, and Matches is every application for this company.
//
// MVP scope is an exact (trimmed, case-insensitive) company name plus the token
// similarity in duplicates.go — embedding similarity is a later phase.
type DuplicateWarning struct {
	CompanyName string
	// JobTitle is the title the matches were compared against. Empty when the
	// caller did not supply one, in which case LikelySame and CrossSite are
	// empty too: with nothing to compare, every match is only a company match.
	JobTitle   string
	Matches    []Application
	LikelySame []Application
	CrossSite  []Application
}

// ListFilter is the combined filter/search query behind GET /applications.
// Every pointer field is an optional filter; nil means "do not filter on this".
// Search matches company name or job title.
//
// From and To bound the application's effective date — applied_at when it is
// set, created_at otherwise — so a Saved application still falls somewhere.
type ListFilter struct {
	Status      *Status
	SiteID      *uuid.UUID
	CVVersionID *uuid.UUID
	Company     *string
	Search      *string
	From        *time.Time
	To          *time.Time
	Limit       int
	Offset      int
}

// NewApplication is the repository-level input for inserting an application
// row. UserID is set by the service from the authenticated caller, never from
// client input.
type NewApplication struct {
	ID             uuid.UUID
	UserID         uuid.UUID
	CompanyName    string
	JobTitle       string
	JobDescription string
	JobURL         string
	SiteID         uuid.UUID
	CVVersionID    *uuid.UUID
	Status         Status
	AppliedAt      *time.Time
}

// UpdateFields is the repository-level input for a captured-data edit, after the
// service has trimmed the strings and resolved the site.
type UpdateFields struct {
	CompanyName    string
	JobTitle       string
	JobDescription string
	JobURL         string
	SiteID         uuid.UUID
	CVVersionID    *uuid.UUID
}

// NewStatusHistory is the repository-level input for one audit row. UserID is
// set by the service from the authenticated caller, mirroring NewApplication.
type NewStatusHistory struct {
	ApplicationID uuid.UUID
	UserID        uuid.UUID
	FromStatus    *Status
	ToStatus      Status
	ChangedBy     ChangeSource
	Note          *string
}

// Domain errors.
var (
	ErrNotFound            = errors.New("applications: application not found")
	ErrCompanyRequired     = errors.New("applications: company name is required")
	ErrJobTitleRequired    = errors.New("applications: job title is required")
	ErrJobURLRequired      = errors.New("applications: job url is required")
	ErrInvalidStatus       = errors.New("applications: unknown status")
	ErrInvalidChangeSource = errors.New("applications: unknown status change source")
	// ErrSameStatus is returned when a transition would not change anything. The
	// ck_status_history_actual_transition constraint would reject the audit row
	// anyway; this catches it before the write.
	ErrSameStatus = errors.New("applications: application is already in that status")
	// ErrAlreadyCompleted is Complete's own version of ErrSameStatus: the
	// application has already been marked complete and sent. Distinct because
	// the user reaching it is doing something reasonable — reopening a tab,
	// pressing the button twice, finishing a job they finished last week — and
	// "already in that status" reads as a fault where "already complete" reads
	// as an answer.
	ErrAlreadyCompleted = errors.New("applications: application has already been completed")
	// ErrSiteUnresolvable means no site_id was supplied and the job url has no
	// host to derive one from.
	ErrSiteUnresolvable = errors.New("applications: could not determine the site from the job url")
	// ErrSiteNotFound means the site_id does not exist, is not active, or does
	// not belong to this user (and is not global).
	ErrSiteNotFound      = errors.New("applications: site not found")
	ErrCVVersionNotFound = errors.New("applications: cv version not found")
	// ErrDuplicateJobURL is the unique(user_id, site_id, job_url) constraint:
	// this exact posting has already been saved by this user.
	ErrDuplicateJobURL = errors.New("applications: this job url has already been saved for this site")
	// ErrCoverLettersUnavailable is a wiring error: a cover letter was supplied
	// but the service was constructed without the coverletters dependency.
	ErrCoverLettersUnavailable = errors.New("applications: cover letter support is not configured")
)
