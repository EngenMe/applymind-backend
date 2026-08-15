package coverletters

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Domain models. No database or HTTP tags — mapping lives in repository.go and
// handler.go respectively.

// Kind mirrors the cover_letter_kind enum. It also encodes the table's check
// constraint: a text cover letter carries BodyText and nothing else; a file
// cover letter carries S3Key and OriginalFilename and nothing else.
type Kind string

const (
	KindText Kind = "text"
	KindFile Kind = "file"
)

// CoverLetter is the cover letter for one application.
//
// Deliberately unlike CV: unique(application_id) means there is at most one row
// per application, there is no version history, and nothing is shared between
// applications. Saving again replaces what was there.
type CoverLetter struct {
	ID               uuid.UUID
	ApplicationID    uuid.UUID
	Kind             Kind
	BodyText         *string
	S3Key            *string
	OriginalFilename *string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Text is the body of a text cover letter, or "" for a file one.
func (c CoverLetter) Text() string {
	if c.BodyText == nil {
		return ""
	}
	return *c.BodyText
}

// Filename is the original filename of a file cover letter, or "" for a text one.
func (c CoverLetter) Filename() string {
	if c.OriginalFilename == nil {
		return ""
	}
	return *c.OriginalFilename
}

// SaveTextInput is a cover letter the user typed or that the extension captured
// from the job site's textarea.
type SaveTextInput struct {
	ApplicationID uuid.UUID
	BodyText      string
}

// SaveFileInput is a cover letter the user attached as a document. ContentType
// is what the browser claimed; the service trusts the file extension over it
// and falls back to a type derived from the extension when it is empty.
type SaveFileInput struct {
	ApplicationID uuid.UUID
	Filename      string
	Content       []byte
	ContentType   string
}

// NewCoverLetter is the repository-level input for inserting a row. Exactly one
// of the two field groups is populated, matching the table's check constraint.
//
// UserID is set by the service from the authenticated caller, never from the
// application_id alone — this module's routes have no dependency on the
// applications module confirming ownership first, so the write itself has to
// carry it.
type NewCoverLetter struct {
	ID               uuid.UUID
	ApplicationID    uuid.UUID
	UserID           uuid.UUID
	Kind             Kind
	BodyText         *string
	S3Key            *string
	OriginalFilename *string
}

// DownloadLink is a short-lived presigned S3 URL for a file cover letter.
type DownloadLink struct {
	URL       string
	Filename  string
	ExpiresAt time.Time
}

// Domain errors.
var (
	ErrNotFound            = errors.New("coverletters: cover letter not found")
	ErrApplicationNotFound = errors.New("coverletters: application not found")
	ErrAlreadyExists       = errors.New("coverletters: application already has a cover letter")
	ErrEmptyBody           = errors.New("coverletters: body text is required")
	ErrFilenameEmpty       = errors.New("coverletters: filename is required")
	ErrEmptyFile           = errors.New("coverletters: file is empty")
	ErrFileTooLarge        = errors.New("coverletters: file exceeds maximum upload size")
	// ErrUnsupportedFileType is returned for anything that is not PDF, DOCX or DOC.
	ErrUnsupportedFileType = errors.New("coverletters: only PDF, DOCX and DOC files are accepted")
	// ErrNotTextKind is returned when an edit is attempted on a file cover
	// letter. The stored bytes are what was actually sent, so they are immutable
	// — replacing them means saving a new cover letter.
	ErrNotTextKind = errors.New("coverletters: only a text cover letter can be edited")
	// ErrNotFileKind is returned when a download is attempted on a text cover
	// letter, which has no stored object.
	ErrNotFileKind = errors.New("coverletters: only a file cover letter can be downloaded")
)
