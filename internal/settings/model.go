package settings

import (
	"errors"
	"time"
)

// Domain models. No database or HTTP tags — mapping lives in repository.go and
// handler.go respectively, matching the other modules.

// Settings is the single row of user-level configuration.
//
// MVP scope is one user and no login, so there is exactly one row and it has no
// id worth exposing: migration 000011 seeds it and the boolean primary key makes
// a second row impossible. When the product grows a users table this becomes one
// row per user and the type stops needing that explanation.
//
// ProfileSummary is nil until the user sets one. Nil and "" are deliberately the
// same thing to every caller — see HasProfileSummary — but the column stays
// nullable so "never set" is distinguishable in the database itself.
type Settings struct {
	ProfileSummary *string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// HasProfileSummary reports whether there is a summary worth scoring against.
func (s Settings) HasProfileSummary() bool {
	return s.ProfileSummary != nil && *s.ProfileSummary != ""
}

// Summary returns the profile summary, or "" when it has never been set.
func (s Settings) Summary() string {
	if s.ProfileSummary == nil {
		return ""
	}
	return *s.ProfileSummary
}

// MaxProfileSummaryLength caps what the endpoint accepts. The phase asks for
// three or four sentences; this leaves generous room for that while keeping the
// AI prompt a predictable size, and mirrors the cap the ai package applies when
// it builds the prompt.
const MaxProfileSummaryLength = 2000

// Domain errors.
var (
	ErrSummaryRequired = errors.New("settings: profile summary is required")
	ErrSummaryTooLong  = errors.New("settings: profile summary is too long")
	// ErrNotFound means the seeded settings row is missing. It is a broken
	// install rather than a normal outcome: reads treat it as "nothing set yet"
	// and a write puts the row back.
	ErrNotFound = errors.New("settings: settings row not found")
)
