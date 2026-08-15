package cvs

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Domain models. No database or HTTP tags — mapping lives in repository.go and
// handler.go respectively.

// CV is a logical CV group. Versions holds its history when it has been loaded;
// it is nil when the caller did not ask for it.
type CV struct {
	ID        uuid.UUID
	Name      string
	Tag       *string
	CreatedAt time.Time
	UpdatedAt time.Time
	Versions  []CVVersion
}

// CVVersion is a single uploaded file belonging to a CV group.
type CVVersion struct {
	ID               uuid.UUID
	CVID             uuid.UUID
	SHA256Hash       string
	FileSizeBytes    int64
	OriginalFilename string
	S3Key            string
	UploadedAt       time.Time
}

// ApplicationUsage is a read-only projection of an application row, exposed so
// the dashboard can show "where has this version been sent". Deliberately not
// the applications module's own domain type — this module only reads it.
type ApplicationUsage struct {
	ApplicationID uuid.UUID
	CompanyName   string
	JobTitle      string
	Status        string
	AppliedAt     *time.Time
}

// MatchOutcome is the terminal outcome of the Flow 3 decision tree.
type MatchOutcome string

const (
	// OutcomeMatched is Flow 3 Terminal Outcome 1 — link the existing version,
	// no user notification.
	OutcomeMatched MatchOutcome = "matched"
	// OutcomeNewVersion is Terminal Outcome 2 — the caller should create a new
	// version under CV and tell the user.
	OutcomeNewVersion MatchOutcome = "new_version"
	// OutcomeNeedsConfirmation is Terminal Outcome 3 (Decision 5) — the filename
	// is recognised but the size could not be compared, so the user must decide.
	OutcomeNeedsConfirmation MatchOutcome = "needs_confirmation"
	// OutcomeUnknown is Terminal Outcome 4 — nothing in the records looks like
	// this file.
	OutcomeUnknown MatchOutcome = "unknown"
)

// MatchedBy records which rule produced a match, for the sidebar and for debugging.
type MatchedBy string

const (
	MatchedByHash            MatchedBy = "hash"
	MatchedByFilenameAndSize MatchedBy = "filename_and_size"
)

// MatchQuery is what the extension observed about the file. SHA256Hash is empty
// when the browser could not read the file content (Flow 3, Decision 1 = NO).
// FileSizeBytes is nil when the size was not available.
type MatchQuery struct {
	SHA256Hash    string
	Filename      string
	FileSizeBytes *int64
}

// MatchResult is the answer to a MatchQuery.
//
//	OutcomeMatched            → CV and Version are set, MatchedBy is set.
//	OutcomeNewVersion         → CV is set, Version is nil.
//	OutcomeNeedsConfirmation  → CV, Version (the most recent one) and Confirmation are set.
//	OutcomeUnknown            → everything is nil.
type MatchResult struct {
	Outcome      MatchOutcome
	MatchedBy    MatchedBy
	CV           *CV
	Version      *CVVersion
	Confirmation *ConfirmationDetails
}

// ConfirmationDetails is the copy the sidebar needs for the Decision 5 prompt:
// "This looks like [CVName] last used [LastUsedAt] for [LastCompany]."
// LastUsedAt and LastCompany are nil when the CV has never been attached to an
// application.
type ConfirmationDetails struct {
	CVName      string
	LastUsedAt  *time.Time
	LastCompany *string
}

// UploadInput is a new file arriving from the extension or dashboard. CVID is
// set when the file is known to belong to an existing CV group (the caller has
// already run a match and got OutcomeNewVersion); when nil a new group is created.
type UploadInput struct {
	Filename    string
	Content     []byte
	ContentType string
	CVID        *uuid.UUID
	Tag         *string
}

// UploadResult reports what was stored. AlreadyExisted is true when these exact
// bytes were already recorded under this CV and nothing new was written.
type UploadResult struct {
	CV             *CV
	Version        *CVVersion
	AlreadyExisted bool
}

// DownloadLink is a short-lived presigned S3 URL.
type DownloadLink struct {
	URL       string
	Filename  string
	ExpiresAt time.Time
}

// NewVersion is the repository-level input for inserting a version row.
//
// UserID is set by the service from the authenticated caller — never from
// client input — and is what CreateVersion writes onto cv_versions.user_id.
type NewVersion struct {
	ID               uuid.UUID
	UserID           uuid.UUID
	CVID             uuid.UUID
	SHA256Hash       string
	FileSizeBytes    int64
	OriginalFilename string
	S3Key            string
}

// Domain errors.
var (
	ErrCVNotFound      = errors.New("cvs: cv not found")
	ErrVersionNotFound = errors.New("cvs: cv version not found")
	ErrFilenameEmpty   = errors.New("cvs: filename is required")
	ErrEmptyFile       = errors.New("cvs: file is empty")
	ErrFileTooLarge    = errors.New("cvs: file exceeds maximum upload size")
)
