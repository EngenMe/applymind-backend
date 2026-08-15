package sites

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Domain models. No database or HTTP tags — mapping lives in repository.go and
// handler.go respectively, matching the applications, cvs and coverletters
// modules.

// Site is a job board or company career site that applications originate from.
//
// Selectors is the CSS/XPath configuration the browser extension uses to scrape
// this site. This module only reads it: it is NULL until the extension phase
// defines the scrape targets, and no endpoint here writes it.
type Site struct {
	ID              uuid.UUID
	Name            string
	Domain          string
	IsPreconfigured bool
	IsActive        bool
	Selectors       json.RawMessage
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// PreconfiguredSite is one entry of the list that ships with the product.
type PreconfiguredSite struct {
	Name            string
	Domain          string
	ActiveByDefault bool
}

// preconfiguredSites is the list seeded on first run. Every entry is inserted
// with is_preconfigured = true, which the service reads as "deactivate, never
// delete" — the ERD's business rule for this table.
//
// Only LinkedIn is active: the MVP extension activates on LinkedIn alone, and
// the rest exist so the dashboard settings page has something to switch on when
// later phases add support for them.
//
// Domains are bare registrable hosts, matching how domain is normalised on the
// way in. Sites whose postings actually live on a subdomain (boards.greenhouse.io,
// jobs.lever.co, <tenant>.wdN.myworkdayjobs.com) will not resolve by host alone —
// that is for the site-detection phase to solve, and it does not affect the MVP.
var preconfiguredSites = []PreconfiguredSite{
	{Name: "LinkedIn", Domain: "linkedin.com", ActiveByDefault: true},
	{Name: "Indeed", Domain: "indeed.com"},
	{Name: "Glassdoor", Domain: "glassdoor.com"},
	{Name: "Monster", Domain: "monster.com"},
	{Name: "Hired", Domain: "hired.com"},
	{Name: "Wellfound", Domain: "wellfound.com"},
	{Name: "Greenhouse", Domain: "greenhouse.io"},
	{Name: "Lever", Domain: "lever.co"},
	{Name: "Workday", Domain: "myworkdayjobs.com"},
	{Name: "IrishJobs", Domain: "irishjobs.ie"},
	{Name: "Jobs.ie", Domain: "jobs.ie"},
}

// PreconfiguredSites returns the shipped list. Exported so the dashboard and the
// tests can see what seeding will produce without reaching into the package.
func PreconfiguredSites() []PreconfiguredSite {
	out := make([]PreconfiguredSite, len(preconfiguredSites))
	copy(out, preconfiguredSites)
	return out
}

// AddInput is a user-added custom site from the settings page. Domain may be a
// bare host or a full URL; the service normalises it. Custom sites are always
// created with is_preconfigured = false and is_active = true — adding a site you
// then have to switch on would be a pointless second step.
type AddInput struct {
	Name   string
	Domain string
}

// ListFilter is the optional filter behind GET /sites. ActiveOnly is what the
// extension wants; the dashboard settings page lists everything.
type ListFilter struct {
	ActiveOnly bool
}

// NewSite is the repository-level input for inserting a site row, after the
// service has trimmed the name and normalised the domain.
//
// UserID is nil only for the pre-configured rows Ensure writes at boot — every
// row Create writes carries the caller's id, since a site added through the
// API always belongs to whoever added it.
type NewSite struct {
	UserID          *uuid.UUID
	Name            string
	Domain          string
	IsPreconfigured bool
	IsActive        bool
}

// Domain errors.
var (
	ErrNotFound       = errors.New("sites: site not found")
	ErrNameRequired   = errors.New("sites: name is required")
	ErrDomainRequired = errors.New("sites: domain is required")
	// ErrInvalidDomain means the input had no usable host — see NormalizeDomain.
	ErrInvalidDomain = errors.New("sites: domain is not a valid host")
	// ErrDuplicate is the unique(name) / unique(domain) constraint.
	ErrDuplicate = errors.New("sites: a site with that name or domain already exists")
	// ErrPreconfigured is the ERD's service-layer business rule: pre-configured
	// rows ship with the product and can only be deactivated.
	ErrPreconfigured = errors.New("sites: pre-configured sites cannot be deleted, only deactivated")
	// ErrInUse is applications.site_id's ON DELETE RESTRICT: applications still
	// point at this site, so removing it would orphan them.
	ErrInUse = errors.New("sites: site still has applications and cannot be deleted")
)
