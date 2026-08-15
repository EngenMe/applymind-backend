package sites

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/EngenMe/applymind-backend/pkg/middleware"
)

// testUserID is the caller every request in this file is authenticated as —
// see middleware.TestContextWithUserID for why these tests inject it directly
// rather than running a real RequireAuth in front of the router.
var testUserID = uuid.New()

// stubService lets each test drive one endpoint without a repository. Unset
// funcs return zero values, so a test only fills in what it exercises.
type stubService struct {
	seed   func(context.Context) (int, error)
	list   func(context.Context, uuid.UUID, ListFilter) ([]Site, error)
	get    func(context.Context, uuid.UUID, uuid.UUID) (*Site, error)
	add    func(context.Context, uuid.UUID, AddInput) (*Site, error)
	toggle func(context.Context, uuid.UUID, uuid.UUID) (*Site, error)
	del    func(context.Context, uuid.UUID, uuid.UUID) error
}

func (s *stubService) SeedPreconfigured(ctx context.Context) (int, error) {
	if s.seed == nil {
		return 0, nil
	}
	return s.seed(ctx)
}

func (s *stubService) List(ctx context.Context, userID uuid.UUID, f ListFilter) ([]Site, error) {
	if s.list == nil {
		return nil, nil
	}
	return s.list(ctx, userID, f)
}

func (s *stubService) Get(ctx context.Context, userID, id uuid.UUID) (*Site, error) {
	if s.get == nil {
		return nil, ErrNotFound
	}
	return s.get(ctx, userID, id)
}

func (s *stubService) Add(ctx context.Context, userID uuid.UUID, in AddInput) (*Site, error) {
	if s.add == nil {
		return nil, ErrNotFound
	}
	return s.add(ctx, userID, in)
}

func (s *stubService) ToggleActive(ctx context.Context, userID, id uuid.UUID) (*Site, error) {
	if s.toggle == nil {
		return nil, ErrNotFound
	}
	return s.toggle(ctx, userID, id)
}

func (s *stubService) Delete(ctx context.Context, userID, id uuid.UUID) error {
	if s.del == nil {
		return ErrNotFound
	}
	return s.del(ctx, userID, id)
}

func newTestRouter(svc Service) http.Handler {
	r := chi.NewRouter()
	NewHandler(svc, slog.New(slog.NewTextHandler(io.Discard, nil))).RegisterRoutes(r)
	return r
}

func do(t *testing.T, svc Service, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req = req.WithContext(middleware.TestContextWithUserID(req.Context(), testUserID))
	rec := httptest.NewRecorder()
	newTestRouter(svc).ServeHTTP(rec, req)
	return rec
}

func decodeInto(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
}

func assertStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, want, rec.Body.String())
	}
}

func assertErrorCode(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	var body errorBody
	decodeInto(t, rec, &body)
	if body.Error.Code != want {
		t.Errorf("error code = %q, want %q", body.Error.Code, want)
	}
}

// ---------------------------------------------------------------------------
// GET /sites
// ---------------------------------------------------------------------------

func TestListSitesReturnsEverySite(t *testing.T) {
	svc := &stubService{
		list: func(_ context.Context, _ uuid.UUID, _ ListFilter) ([]Site, error) {
			return []Site{
				preconfiguredSite("LinkedIn", "linkedin.com", true),
				customSite("Acme", "acme.com", false),
			}, nil
		},
	}

	rec := do(t, svc, http.MethodGet, "/sites", "")
	assertStatus(t, rec, http.StatusOK)

	var body struct {
		Sites []siteResponse `json:"sites"`
	}
	decodeInto(t, rec, &body)

	if len(body.Sites) != 2 {
		t.Fatalf("returned %d sites, want 2", len(body.Sites))
	}
	if !body.Sites[0].IsPreconfigured {
		t.Error("first site: is_preconfigured = false, want true")
	}
	if body.Sites[1].IsActive {
		t.Error("second site: is_active = true, want false")
	}
}

func TestListSitesPassesActiveFilter(t *testing.T) {
	var got ListFilter
	var gotUser uuid.UUID
	svc := &stubService{
		list: func(_ context.Context, userID uuid.UUID, f ListFilter) ([]Site, error) {
			got, gotUser = f, userID
			return nil, nil
		},
	}

	rec := do(t, svc, http.MethodGet, "/sites?active=true", "")
	assertStatus(t, rec, http.StatusOK)
	if !got.ActiveOnly {
		t.Error("ActiveOnly = false, want true")
	}
	if gotUser != testUserID {
		t.Errorf("userID passed to service = %s, want %s", gotUser, testUserID)
	}
}

func TestListSitesRejectsBadActiveValue(t *testing.T) {
	rec := do(t, &stubService{}, http.MethodGet, "/sites?active=maybe", "")
	assertStatus(t, rec, http.StatusBadRequest)
	assertErrorCode(t, rec, "invalid_active")
}

// ---------------------------------------------------------------------------
// POST /sites
// ---------------------------------------------------------------------------

func TestAddSiteCreated(t *testing.T) {
	var got AddInput
	svc := &stubService{
		add: func(_ context.Context, _ uuid.UUID, in AddInput) (*Site, error) {
			got = in
			site := customSite("Acme Careers", "acme.com", true)
			return &site, nil
		},
	}

	rec := do(t, svc, http.MethodPost, "/sites", `{"name":"Acme Careers","domain":"https://acme.com/jobs"}`)
	assertStatus(t, rec, http.StatusCreated)

	// The handler passes the raw domain through; normalising is the service's job.
	if got.Domain != "https://acme.com/jobs" {
		t.Errorf("domain passed to service = %q, want the raw input", got.Domain)
	}

	var body siteResponse
	decodeInto(t, rec, &body)
	if body.Domain != "acme.com" {
		t.Errorf("domain = %q, want %q", body.Domain, "acme.com")
	}
	if body.IsPreconfigured {
		t.Error("is_preconfigured = true, want false")
	}
}

func TestAddSiteRejectsInvalidJSON(t *testing.T) {
	rec := do(t, &stubService{}, http.MethodPost, "/sites", `{"name":`)
	assertStatus(t, rec, http.StatusBadRequest)
	assertErrorCode(t, rec, "invalid_body")
}

func TestAddSiteMapsServiceErrors(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		status   int
		wantCode string
	}{
		{"missing name", ErrNameRequired, http.StatusBadRequest, "name_required"},
		{"missing domain", ErrDomainRequired, http.StatusBadRequest, "domain_required"},
		{"bad domain", ErrInvalidDomain, http.StatusBadRequest, "invalid_domain"},
		{"duplicate", ErrDuplicate, http.StatusConflict, "site_already_exists"},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				svc := &stubService{
					add: func(_ context.Context, _ uuid.UUID, _ AddInput) (*Site, error) { return nil, tt.err },
				}
				rec := do(t, svc, http.MethodPost, "/sites", `{"name":"x","domain":"y"}`)
				assertStatus(t, rec, tt.status)
				assertErrorCode(t, rec, tt.wantCode)
			},
		)
	}
}

// ---------------------------------------------------------------------------
// PATCH /sites/{id}/toggle
// ---------------------------------------------------------------------------

func TestToggleSite(t *testing.T) {
	seed := customSite("Acme", "acme.com", false)
	var got uuid.UUID
	var gotUser uuid.UUID
	svc := &stubService{
		toggle: func(_ context.Context, userID, id uuid.UUID) (*Site, error) {
			got, gotUser = id, userID
			flipped := seed
			flipped.IsActive = true
			return &flipped, nil
		},
	}

	rec := do(t, svc, http.MethodPatch, "/sites/"+seed.ID.String()+"/toggle", "")
	assertStatus(t, rec, http.StatusOK)
	if got != seed.ID {
		t.Errorf("id passed to service = %s, want %s", got, seed.ID)
	}
	if gotUser != testUserID {
		t.Errorf("userID passed to service = %s, want %s", gotUser, testUserID)
	}

	var body siteResponse
	decodeInto(t, rec, &body)
	if !body.IsActive {
		t.Error("is_active = false, want true")
	}
}

func TestToggleSiteRejectsNonUUID(t *testing.T) {
	rec := do(t, &stubService{}, http.MethodPatch, "/sites/not-a-uuid/toggle", "")
	assertStatus(t, rec, http.StatusBadRequest)
	assertErrorCode(t, rec, "invalid_site_id")
}

func TestToggleSiteNotFound(t *testing.T) {
	svc := &stubService{
		toggle: func(_ context.Context, _, _ uuid.UUID) (*Site, error) { return nil, ErrNotFound },
	}

	rec := do(t, svc, http.MethodPatch, "/sites/"+uuid.New().String()+"/toggle", "")
	assertStatus(t, rec, http.StatusNotFound)
	assertErrorCode(t, rec, "site_not_found")
}

// ---------------------------------------------------------------------------
// DELETE /sites/{id}
// ---------------------------------------------------------------------------

func TestDeleteSiteNoContent(t *testing.T) {
	svc := &stubService{del: func(_ context.Context, _, _ uuid.UUID) error { return nil }}

	rec := do(t, svc, http.MethodDelete, "/sites/"+uuid.New().String(), "")
	assertStatus(t, rec, http.StatusNoContent)
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", rec.Body.String())
	}
}

func TestDeleteSiteMapsServiceErrors(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		status   int
		wantCode string
	}{
		{"preconfigured", ErrPreconfigured, http.StatusConflict, "site_is_preconfigured"},
		{"in use", ErrInUse, http.StatusConflict, "site_in_use"},
		{"unknown", ErrNotFound, http.StatusNotFound, "site_not_found"},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				svc := &stubService{del: func(_ context.Context, _, _ uuid.UUID) error { return tt.err }}
				rec := do(t, svc, http.MethodDelete, "/sites/"+uuid.New().String(), "")
				assertStatus(t, rec, tt.status)
				assertErrorCode(t, rec, tt.wantCode)
			},
		)
	}
}
