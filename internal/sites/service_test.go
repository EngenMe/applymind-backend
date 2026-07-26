package sites

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// fakeRepo is an in-memory Repository. It reproduces the two behaviours the
// service actually leans on: Ensure is a silent no-op when the name or domain is
// taken, and Delete can fail with ErrInUse the way the applications foreign key
// does.
type fakeRepo struct {
	rows      []Site
	deleteErr error
	deleted   []uuid.UUID
}

func newFakeRepo(seed ...Site) *fakeRepo {
	return &fakeRepo{rows: append([]Site(nil), seed...)}
}

func (f *fakeRepo) index(id uuid.UUID) int {
	for i := range f.rows {
		if f.rows[i].ID == id {
			return i
		}
	}
	return -1
}

func (f *fakeRepo) taken(name, domain string) bool {
	for i := range f.rows {
		if f.rows[i].Domain == domain || strings.EqualFold(f.rows[i].Name, name) {
			return true
		}
	}
	return false
}

func (f *fakeRepo) Create(_ context.Context, in NewSite) (*Site, error) {
	if f.taken(in.Name, in.Domain) {
		return nil, ErrDuplicate
	}
	site := Site{
		ID:              uuid.New(),
		Name:            in.Name,
		Domain:          in.Domain,
		IsPreconfigured: in.IsPreconfigured,
		IsActive:        in.IsActive,
	}
	f.rows = append(f.rows, site)
	return &site, nil
}

func (f *fakeRepo) Ensure(ctx context.Context, in NewSite) (*Site, error) {
	if f.taken(in.Name, in.Domain) {
		return nil, nil
	}
	return f.Create(ctx, in)
}

func (f *fakeRepo) List(_ context.Context, filter ListFilter) ([]Site, error) {
	out := make([]Site, 0, len(f.rows))
	for _, row := range f.rows {
		if filter.ActiveOnly && !row.IsActive {
			continue
		}
		out = append(out, row)
	}
	return out, nil
}

func (f *fakeRepo) Get(_ context.Context, id uuid.UUID) (*Site, error) {
	i := f.index(id)
	if i < 0 {
		return nil, ErrNotFound
	}
	site := f.rows[i]
	return &site, nil
}

func (f *fakeRepo) GetByDomain(_ context.Context, domain string) (*Site, error) {
	for i := range f.rows {
		if f.rows[i].Domain == domain {
			site := f.rows[i]
			return &site, nil
		}
	}
	return nil, ErrNotFound
}

func (f *fakeRepo) SetActive(_ context.Context, id uuid.UUID, active bool) (*Site, error) {
	i := f.index(id)
	if i < 0 {
		return nil, ErrNotFound
	}
	f.rows[i].IsActive = active
	site := f.rows[i]
	return &site, nil
}

func (f *fakeRepo) Delete(_ context.Context, id uuid.UUID) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	i := f.index(id)
	if i < 0 {
		return ErrNotFound
	}
	f.rows = append(f.rows[:i], f.rows[i+1:]...)
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeRepo) byDomain(t *testing.T, domain string) Site {
	t.Helper()
	for i := range f.rows {
		if f.rows[i].Domain == domain {
			return f.rows[i]
		}
	}
	t.Fatalf("no site stored for domain %q", domain)
	return Site{}
}

func customSite(name, domain string, active bool) Site {
	return Site{ID: uuid.New(), Name: name, Domain: domain, IsActive: active}
}

func preconfiguredSite(name, domain string, active bool) Site {
	s := customSite(name, domain, active)
	s.IsPreconfigured = true
	return s
}

// ---------------------------------------------------------------------------
// Seeding
// ---------------------------------------------------------------------------

func TestSeedPreconfiguredOnEmptyTable(t *testing.T) {
	repo := newFakeRepo()

	created, err := NewService(repo).SeedPreconfigured(context.Background())
	if err != nil {
		t.Fatalf("SeedPreconfigured: %v", err)
	}

	want := len(PreconfiguredSites())
	if created != want {
		t.Errorf("created = %d, want %d", created, want)
	}
	if len(repo.rows) != want {
		t.Fatalf("stored %d sites, want %d", len(repo.rows), want)
	}

	for _, row := range repo.rows {
		if !row.IsPreconfigured {
			t.Errorf("%s: is_preconfigured = false, want true", row.Domain)
		}
		// LinkedIn is the only site the MVP extension activates on.
		wantActive := row.Domain == "linkedin.com"
		if row.IsActive != wantActive {
			t.Errorf("%s: is_active = %v, want %v", row.Domain, row.IsActive, wantActive)
		}
	}
}

func TestSeedPreconfiguredIsIdempotent(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)
	ctx := context.Background()

	first, err := svc.SeedPreconfigured(ctx)
	if err != nil {
		t.Fatalf("first seed: %v", err)
	}

	second, err := svc.SeedPreconfigured(ctx)
	if err != nil {
		t.Fatalf("second seed: %v", err)
	}
	if second != 0 {
		t.Errorf("second run created %d sites, want 0", second)
	}
	if len(repo.rows) != first {
		t.Errorf("stored %d sites after two runs, want %d", len(repo.rows), first)
	}
}

// Migration 000010 already inserts LinkedIn, because applications.site_id is NOT
// NULL and needs at least one site to exist. Seeding must skip that row rather
// than duplicate it or reset it.
func TestSeedPreconfiguredLeavesMigrationRowAlone(t *testing.T) {
	existing := preconfiguredSite("LinkedIn", "linkedin.com", true)
	repo := newFakeRepo(existing)

	created, err := NewService(repo).SeedPreconfigured(context.Background())
	if err != nil {
		t.Fatalf("SeedPreconfigured: %v", err)
	}

	if want := len(PreconfiguredSites()) - 1; created != want {
		t.Errorf("created = %d, want %d", created, want)
	}

	linkedIn := 0
	for _, row := range repo.rows {
		if row.Domain == "linkedin.com" {
			linkedIn++
			if row.ID != existing.ID {
				t.Error("linkedin row was replaced, want the migration's row untouched")
			}
		}
	}
	if linkedIn != 1 {
		t.Errorf("found %d linkedin rows, want 1", linkedIn)
	}
}

// ---------------------------------------------------------------------------
// Adding a custom site
// ---------------------------------------------------------------------------

func TestAddNormalizesDomainAndDefaults(t *testing.T) {
	repo := newFakeRepo()

	site, err := NewService(repo).Add(
		context.Background(),
		AddInput{Name: "  Acme Careers  ", Domain: "https://WWW.Acme.com/jobs?ref=1"},
	)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	if site.Name != "Acme Careers" {
		t.Errorf("name = %q, want %q", site.Name, "Acme Careers")
	}
	if site.Domain != "acme.com" {
		t.Errorf("domain = %q, want %q", site.Domain, "acme.com")
	}
	if site.IsPreconfigured {
		t.Error("is_preconfigured = true, want false for a custom site")
	}
	if !site.IsActive {
		t.Error("is_active = false, want true for a newly added site")
	}
}

func TestAddValidation(t *testing.T) {
	tests := []struct {
		name  string
		in    AddInput
		wants error
	}{
		{"empty name", AddInput{Name: "", Domain: "acme.com"}, ErrNameRequired},
		{"whitespace name", AddInput{Name: "   ", Domain: "acme.com"}, ErrNameRequired},
		{"empty domain", AddInput{Name: "Acme", Domain: ""}, ErrDomainRequired},
		{"whitespace domain", AddInput{Name: "Acme", Domain: "  "}, ErrDomainRequired},
		{"host with spaces", AddInput{Name: "Acme", Domain: "not a domain"}, ErrInvalidDomain},
		{"bare label", AddInput{Name: "Acme", Domain: "localhost"}, ErrInvalidDomain},
		{"scheme only", AddInput{Name: "Acme", Domain: "https://"}, ErrInvalidDomain},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				repo := newFakeRepo()
				if _, err := NewService(repo).Add(context.Background(), tt.in); !errors.Is(err, tt.wants) {
					t.Fatalf("err = %v, want %v", err, tt.wants)
				}
				if len(repo.rows) != 0 {
					t.Errorf("stored %d sites, want 0", len(repo.rows))
				}
			},
		)
	}
}

// A custom site for a domain that is already registered — including one that
// only matches after normalisation — is a conflict, not a second row.
func TestAddRejectsExistingDomain(t *testing.T) {
	repo := newFakeRepo(preconfiguredSite("LinkedIn", "linkedin.com", true))

	_, err := NewService(repo).Add(
		context.Background(),
		AddInput{Name: "LinkedIn Jobs", Domain: "https://www.linkedin.com/jobs"},
	)
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("err = %v, want ErrDuplicate", err)
	}
	if len(repo.rows) != 1 {
		t.Errorf("stored %d sites, want 1", len(repo.rows))
	}
}

// ---------------------------------------------------------------------------
// Toggling
// ---------------------------------------------------------------------------

func TestToggleActiveFlipsBothWays(t *testing.T) {
	seed := customSite("Acme", "acme.com", false)
	repo := newFakeRepo(seed)
	svc := NewService(repo)
	ctx := context.Background()

	site, err := svc.ToggleActive(ctx, seed.ID)
	if err != nil {
		t.Fatalf("first toggle: %v", err)
	}
	if !site.IsActive {
		t.Error("is_active = false after first toggle, want true")
	}

	site, err = svc.ToggleActive(ctx, seed.ID)
	if err != nil {
		t.Fatalf("second toggle: %v", err)
	}
	if site.IsActive {
		t.Error("is_active = true after second toggle, want false")
	}
}

// The ERD allows pre-configured sites to be deactivated — that is the whole
// point of the is_active flag on them.
func TestTogglePreconfiguredIsAllowed(t *testing.T) {
	seed := preconfiguredSite("LinkedIn", "linkedin.com", true)
	repo := newFakeRepo(seed)

	site, err := NewService(repo).ToggleActive(context.Background(), seed.ID)
	if err != nil {
		t.Fatalf("ToggleActive: %v", err)
	}
	if site.IsActive {
		t.Error("is_active = true, want false after deactivating")
	}
	if len(repo.rows) != 1 {
		t.Errorf("stored %d sites, want the row to survive", len(repo.rows))
	}
}

func TestToggleUnknownSite(t *testing.T) {
	repo := newFakeRepo()

	if _, err := NewService(repo).ToggleActive(context.Background(), uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// Deleting
// ---------------------------------------------------------------------------

func TestDeleteBlockedForPreconfigured(t *testing.T) {
	seed := preconfiguredSite("LinkedIn", "linkedin.com", true)
	repo := newFakeRepo(seed)

	err := NewService(repo).Delete(context.Background(), seed.ID)
	if !errors.Is(err, ErrPreconfigured) {
		t.Fatalf("err = %v, want ErrPreconfigured", err)
	}
	if len(repo.deleted) != 0 {
		t.Error("repository Delete was called, want the service to refuse before reaching it")
	}
	if len(repo.rows) != 1 {
		t.Errorf("stored %d sites, want 1", len(repo.rows))
	}
}

func TestDeleteAllowedForCustom(t *testing.T) {
	seed := customSite("Acme", "acme.com", true)
	repo := newFakeRepo(seed)

	if err := NewService(repo).Delete(context.Background(), seed.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(repo.rows) != 0 {
		t.Errorf("stored %d sites, want 0", len(repo.rows))
	}
}

func TestDeleteUnknownSite(t *testing.T) {
	repo := newFakeRepo()

	if err := NewService(repo).Delete(context.Background(), uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// applications.site_id is ON DELETE RESTRICT, so a custom site with applications
// attached still fails — the service does not swallow it.
func TestDeleteSurfacesInUse(t *testing.T) {
	seed := customSite("Acme", "acme.com", true)
	repo := newFakeRepo(seed)
	repo.deleteErr = ErrInUse

	if err := NewService(repo).Delete(context.Background(), seed.ID); !errors.Is(err, ErrInUse) {
		t.Fatalf("err = %v, want ErrInUse", err)
	}
}

// ---------------------------------------------------------------------------
// Listing and normalisation
// ---------------------------------------------------------------------------

func TestListActiveOnly(t *testing.T) {
	repo := newFakeRepo(
		preconfiguredSite("LinkedIn", "linkedin.com", true),
		preconfiguredSite("Indeed", "indeed.com", false),
		customSite("Acme", "acme.com", true),
	)
	svc := NewService(repo)
	ctx := context.Background()

	all, err := svc.List(ctx, ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("unfiltered list returned %d sites, want 3", len(all))
	}

	active, err := svc.List(ctx, ListFilter{ActiveOnly: true})
	if err != nil {
		t.Fatalf("List active: %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("active list returned %d sites, want 2", len(active))
	}
	for _, row := range active {
		if !row.IsActive {
			t.Errorf("%s is inactive but was returned", row.Domain)
		}
	}
}

func TestNormalizeDomain(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"acme.com", "acme.com"},
		{"ACME.com", "acme.com"},
		{"www.acme.com", "acme.com"},
		{"  acme.com  ", "acme.com"},
		{"https://www.acme.com", "acme.com"},
		{"https://acme.com/careers/123?ref=x", "acme.com"},
		{"http://acme.com:8080/jobs", "acme.com"},
		{"jobs.acme.co.uk", "jobs.acme.co.uk"},
	}

	for _, tt := range tests {
		t.Run(
			tt.in, func(t *testing.T) {
				got, err := NormalizeDomain(tt.in)
				if err != nil {
					t.Fatalf("NormalizeDomain(%q): %v", tt.in, err)
				}
				if got != tt.want {
					t.Errorf("NormalizeDomain(%q) = %q, want %q", tt.in, got, tt.want)
				}
			},
		)
	}
}
