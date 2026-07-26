package sites

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/google/uuid"
)

// Service is the business logic boundary for the sites module.
type Service interface {
	// SeedPreconfigured inserts every pre-configured site that is not in the
	// table yet and returns how many rows it created. It is idempotent, so it is
	// safe to call on every boot; migration 000010 already seeds LinkedIn as the
	// guard for applications.site_id being NOT NULL, and that row is left alone.
	SeedPreconfigured(ctx context.Context) (int, error)
	List(ctx context.Context, f ListFilter) ([]Site, error)
	Get(ctx context.Context, id uuid.UUID) (*Site, error)
	// Add registers a user's own site. The domain is normalised to a bare host
	// before it is stored, so it matches what a job URL resolves to later.
	Add(ctx context.Context, in AddInput) (*Site, error)
	// ToggleActive flips is_active. Allowed on pre-configured sites: the ERD
	// permits deactivating them, only not deleting them.
	ToggleActive(ctx context.Context, id uuid.UUID) (*Site, error)
	// Delete removes a custom site. Pre-configured sites are refused.
	Delete(ctx context.Context, id uuid.UUID) error
}

type service struct {
	repo Repository
}

func NewService(repo Repository) Service {
	return &service{repo: repo}
}

// SeedPreconfigured walks the shipped list and inserts what is missing.
//
// Idempotency lives in the repository's ON CONFLICT DO NOTHING rather than in a
// "have I run before?" check, so a partially seeded table (LinkedIn from the
// migration, or a run that died halfway) converges on the next boot.
func (s *service) SeedPreconfigured(ctx context.Context) (int, error) {
	created := 0
	for _, p := range preconfiguredSites {
		site, err := s.repo.Ensure(
			ctx, NewSite{
				Name:            p.Name,
				Domain:          p.Domain,
				IsPreconfigured: true,
				IsActive:        p.ActiveByDefault,
			},
		)
		if err != nil {
			return created, fmt.Errorf("sites: seed %s: %w", p.Domain, err)
		}
		if site != nil {
			created++
		}
	}
	return created, nil
}

func (s *service) List(ctx context.Context, f ListFilter) ([]Site, error) {
	return s.repo.List(ctx, f)
}

func (s *service) Get(ctx context.Context, id uuid.UUID) (*Site, error) {
	return s.repo.Get(ctx, id)
}

func (s *service) Add(ctx context.Context, in AddInput) (*Site, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, ErrNameRequired
	}
	if strings.TrimSpace(in.Domain) == "" {
		return nil, ErrDomainRequired
	}
	domain, err := NormalizeDomain(in.Domain)
	if err != nil {
		return nil, err
	}

	// A friendly error ahead of the unique constraint. The constraint is still
	// the authority — translateWriteError catches the race — but this way the
	// common case reports the domain rather than a generic collision.
	existing, err := s.repo.GetByDomain(ctx, domain)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if existing != nil {
		return nil, ErrDuplicate
	}

	return s.repo.Create(
		ctx, NewSite{
			Name:            name,
			Domain:          domain,
			IsPreconfigured: false,
			IsActive:        true,
		},
	)
}

func (s *service) ToggleActive(ctx context.Context, id uuid.UUID) (*Site, error) {
	// Read first so a missing id is a 404 rather than a silent no-op, and so the
	// endpoint can stay body-less: the caller does not have to know the current
	// value to flip it.
	current, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.repo.SetActive(ctx, id, !current.IsActive)
}

func (s *service) Delete(ctx context.Context, id uuid.UUID) error {
	site, err := s.repo.Get(ctx, id)
	if err != nil {
		return err
	}
	if site.IsPreconfigured {
		return ErrPreconfigured
	}
	// Still ErrInUse from here when applications reference the site: the
	// ON DELETE RESTRICT foreign key is the real guard, not this check.
	return s.repo.Delete(ctx, id)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// NormalizeDomain reduces user input to the bare host stored in sites.domain:
// lowercased, with no scheme, path, port or leading www. It accepts a full URL
// as readily as a bare host, because the settings page lets the user paste
// either.
//
// This deliberately mirrors, rather than imports, the same rule in the
// applications module. The two modules do not depend on each other — applications
// keeps its own read-only Site projection for the same reason — and the rule is
// small enough that duplication costs less than the coupling. If it grows,
// promote it to pkg/ and change both.
func NormalizeDomain(raw string) (string, error) {
	in := strings.TrimSpace(raw)
	if in == "" {
		return "", ErrDomainRequired
	}

	parsed, err := url.Parse(in)
	if err != nil || parsed.Hostname() == "" {
		// A bare host parses as a path, so retry with a scheme bolted on.
		parsed, err = url.Parse("https://" + in)
		if err != nil || parsed.Hostname() == "" {
			return "", ErrInvalidDomain
		}
	}

	host := strings.TrimPrefix(strings.ToLower(parsed.Hostname()), "www.")
	// A host with no dot is a bare label (localhost, or a typo) and cannot match
	// any real job URL.
	if !strings.Contains(host, ".") || strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") {
		return "", ErrInvalidDomain
	}
	return host, nil
}
