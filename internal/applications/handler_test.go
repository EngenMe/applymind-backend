package applications

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// fakeService records what the handler passed down and returns canned answers,
// so these tests cover routing, decoding, query parsing and status codes only.
type fakeService struct {
	createIn   CreateInput
	updateIn   UpdateInput
	statusIn   StatusUpdateInput
	listIn     ListFilter
	dupIn      DuplicateQuery
	deletedID  uuid.UUID
	completeID uuid.UUID
	completeIn CompleteInput

	createResult   *CreateResult
	application    *Application
	list           []Application
	duplicate      *DuplicateWarning
	completeResult *CompleteResult
	err            error
}

func (f *fakeService) Create(_ context.Context, in CreateInput) (*CreateResult, error) {
	f.createIn = in
	if f.err != nil {
		return nil, f.err
	}
	return f.createResult, nil
}

func (f *fakeService) Get(_ context.Context, _ uuid.UUID) (*Application, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.application, nil
}

func (f *fakeService) List(_ context.Context, filter ListFilter) ([]Application, error) {
	f.listIn = filter
	if f.err != nil {
		return nil, f.err
	}
	return f.list, nil
}

func (f *fakeService) Update(_ context.Context, _ uuid.UUID, in UpdateInput) (*Application, error) {
	f.updateIn = in
	if f.err != nil {
		return nil, f.err
	}
	return f.application, nil
}

func (f *fakeService) UpdateStatus(_ context.Context, _ uuid.UUID, in StatusUpdateInput) (*Application, error) {
	f.statusIn = in
	if f.err != nil {
		return nil, f.err
	}
	return f.application, nil
}

// Complete backs PATCH /applications/{id}/complete — Phase 13, Flow 2.
func (f *fakeService) Complete(_ context.Context, id uuid.UUID, in CompleteInput) (*CompleteResult, error) {
	f.completeID = id
	f.completeIn = in
	if f.err != nil {
		return nil, f.err
	}
	return f.completeResult, nil
}

func (f *fakeService) Delete(_ context.Context, id uuid.UUID) error {
	f.deletedID = id
	return f.err
}

// CheckDuplicate takes a DuplicateQuery as of Phase 13, rather than a bare
// company string, so a title (and site) can sharpen the match into
// LikelySame/CrossSite rather than only Matches.
func (f *fakeService) CheckDuplicate(_ context.Context, q DuplicateQuery) (*DuplicateWarning, error) {
	f.dupIn = q
	if f.err != nil {
		return nil, f.err
	}
	return f.duplicate, nil
}

func (f *fakeService) StatusHistory(_ context.Context, _ uuid.UUID) ([]StatusHistory, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.application == nil {
		return nil, nil
	}
	return f.application.History, nil
}

func sampleApplication() *Application {
	return &Application{
		ID:             uuid.MustParse("66666666-6666-6666-6666-666666666666"),
		CompanyName:    "Stripe",
		JobTitle:       "Senior Backend Engineer",
		JobDescription: "Go, Postgres, AWS.",
		JobURL:         "https://www.linkedin.com/jobs/view/4012345678",
		SiteID:         linkedIn.ID,
		Status:         StatusApplied,
		AppliedAt:      &fixedNow,
		CreatedAt:      fixedNow,
		UpdatedAt:      fixedNow,
	}
}

func newTestRouter(svc Service) *chi.Mux {
	r := chi.NewRouter()
	NewHandler(svc, nil).RegisterRoutes(r)
	return r
}

func do(t *testing.T, router *chi.Mux, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, rec.Body.String())
	}
	return out
}

func TestPostApplicationCreated(t *testing.T) {
	app := sampleApplication()
	due := fixedNow.Add(DefaultFollowUpDelay)
	svc := &fakeService{createResult: &CreateResult{Application: app, FollowUpDueAt: &due}}

	rec := do(
		t, newTestRouter(svc), http.MethodPost, "/applications", `{
			"company_name": "Stripe",
			"job_title": "Senior Backend Engineer",
			"job_description": "Go, Postgres, AWS.",
			"job_url": "https://www.linkedin.com/jobs/view/4012345678",
			"cover_letter_text": "Dear hiring manager..."
		}`,
	)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, http.StatusCreated, rec.Body.String())
	}
	if svc.createIn.CompanyName != "Stripe" {
		t.Errorf("company name reached the service as %q", svc.createIn.CompanyName)
	}
	if svc.createIn.CoverLetterText == nil {
		t.Error("cover letter text did not reach the service")
	}

	body := decodeBody(t, rec)
	if _, ok := body["application"]; !ok {
		t.Error("response has no application object")
	}
	if _, ok := body["duplicate_warning"]; ok {
		t.Error("duplicate_warning should be omitted when there is no duplicate")
	}
	if body["follow_up_due_at"] == nil {
		t.Error("follow_up_due_at missing from the response")
	}
}

func TestPostApplicationReportsDuplicate(t *testing.T) {
	app := sampleApplication()
	svc := &fakeService{
		createResult: &CreateResult{
			Application: app,
			Duplicate:   &DuplicateWarning{CompanyName: "Stripe", Matches: []Application{*sampleApplication()}},
		},
	}

	rec := do(
		t, newTestRouter(svc), http.MethodPost, "/applications",
		`{"company_name":"Stripe","job_title":"Engineer","job_url":"https://linkedin.com/jobs/1"}`,
	)

	// A duplicate is a warning, not a rejection: the save still succeeded.
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d — a duplicate must not block", rec.Code, http.StatusCreated)
	}
	warning, ok := decodeBody(t, rec)["duplicate_warning"].(map[string]any)
	if !ok {
		t.Fatalf("duplicate_warning missing: %s", rec.Body.String())
	}
	if matches, ok := warning["matches"].([]any); !ok || len(matches) != 1 {
		t.Errorf("matches = %v, want 1", warning["matches"])
	}
}

func TestPostApplicationInvalidJSON(t *testing.T) {
	rec := do(t, newTestRouter(&fakeService{}), http.MethodPost, "/applications", `{"company_name":`)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"missing company", ErrCompanyRequired, http.StatusBadRequest},
		{"unknown status", ErrInvalidStatus, http.StatusBadRequest},
		{"unresolvable site", ErrSiteUnresolvable, http.StatusBadRequest},
		{"unknown site", ErrSiteNotFound, http.StatusNotFound},
		{"unknown cv version", ErrCVVersionNotFound, http.StatusNotFound},
		{"already saved", ErrDuplicateJobURL, http.StatusConflict},
		{"application missing", ErrNotFound, http.StatusNotFound},
	}

	for _, tc := range cases {
		t.Run(
			tc.name, func(t *testing.T) {
				svc := &fakeService{err: tc.err}
				rec := do(
					t, newTestRouter(svc), http.MethodPost, "/applications",
					`{"company_name":"Stripe","job_title":"Engineer","job_url":"https://linkedin.com/jobs/1"}`,
				)
				if rec.Code != tc.want {
					t.Errorf("status = %d, want %d", rec.Code, tc.want)
				}
				if body := decodeBody(t, rec); body["error"] == nil {
					t.Error("error envelope missing from the response")
				}
			},
		)
	}
}

func TestGetApplicationIncludesHistory(t *testing.T) {
	app := sampleApplication()
	from := StatusSaved
	app.History = []StatusHistory{
		{ID: uuid.New(), ToStatus: StatusSaved, ChangedBy: ChangeSourceUser, ChangedAt: fixedNow},
		{ID: uuid.New(), FromStatus: &from, ToStatus: StatusApplied, ChangedBy: ChangeSourceUser, ChangedAt: fixedNow},
	}
	svc := &fakeService{application: app}

	rec := do(t, newTestRouter(svc), http.MethodGet, "/applications/"+app.ID.String(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	history, ok := decodeBody(t, rec)["status_history"].([]any)
	if !ok || len(history) != 2 {
		t.Fatalf("status_history = %v, want 2 entries", decodeBody(t, rec)["status_history"])
	}
	first := history[0].(map[string]any)
	if first["from_status"] != nil {
		t.Errorf("first from_status = %v, want null", first["from_status"])
	}
}

func TestGetApplicationBadID(t *testing.T) {
	rec := do(t, newTestRouter(&fakeService{}), http.MethodGet, "/applications/not-a-uuid", "")

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestListParsesFilters(t *testing.T) {
	svc := &fakeService{list: []Application{*sampleApplication()}}
	target := "/applications?status=Interviewing&company=Stripe&q=backend" +
		"&site_id=" + linkedIn.ID.String() +
		"&cv_version_id=" + cvVersion.String() +
		"&from=2026-05-01&to=2026-05-31T23:59:59Z&limit=10&offset=20"

	rec := do(t, newTestRouter(svc), http.MethodGet, target, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	f := svc.listIn
	if f.Status == nil || *f.Status != StatusInterviewing {
		t.Errorf("status filter = %v", f.Status)
	}
	if f.Company == nil || *f.Company != "Stripe" {
		t.Errorf("company filter = %v", f.Company)
	}
	if f.Search == nil || *f.Search != "backend" {
		t.Errorf("search filter = %v", f.Search)
	}
	if f.SiteID == nil || *f.SiteID != linkedIn.ID {
		t.Errorf("site filter = %v", f.SiteID)
	}
	if f.CVVersionID == nil || *f.CVVersionID != cvVersion {
		t.Errorf("cv version filter = %v", f.CVVersionID)
	}
	if f.From == nil || !f.From.Equal(time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("from filter = %v, want a bare date to parse", f.From)
	}
	if f.To == nil || !f.To.Equal(time.Date(2026, 5, 31, 23, 59, 59, 0, time.UTC)) {
		t.Errorf("to filter = %v, want an RFC3339 timestamp to parse", f.To)
	}
	if f.Limit != 10 || f.Offset != 20 {
		t.Errorf("paging = limit %d offset %d, want 10/20", f.Limit, f.Offset)
	}
}

func TestListRejectsBadDate(t *testing.T) {
	rec := do(t, newTestRouter(&fakeService{}), http.MethodGet, "/applications?from=last-tuesday", "")

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestListEmptyIsAnEmptyArray(t *testing.T) {
	rec := do(t, newTestRouter(&fakeService{}), http.MethodGet, "/applications", "")

	apps, ok := decodeBody(t, rec)["applications"].([]any)
	if !ok {
		t.Fatalf("applications missing or not an array: %s", rec.Body.String())
	}
	if len(apps) != 0 {
		t.Errorf("applications = %v, want an empty array rather than null", apps)
	}
}

func TestPatchStatus(t *testing.T) {
	app := sampleApplication()
	svc := &fakeService{application: app}

	rec := do(
		t, newTestRouter(svc), http.MethodPatch, "/applications/"+app.ID.String()+"/status",
		`{"status":"Interviewing","note":"phone screen booked"}`,
	)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if svc.statusIn.Status != StatusInterviewing {
		t.Errorf("status reached the service as %q", svc.statusIn.Status)
	}
	if svc.statusIn.Note == nil || *svc.statusIn.Note != "phone screen booked" {
		t.Errorf("note = %v", svc.statusIn.Note)
	}
	// Left empty so the service can apply its default rather than the handler.
	if svc.statusIn.ChangedBy != "" {
		t.Errorf("changed_by = %q, want it left to the service default", svc.statusIn.ChangedBy)
	}
}

func TestPatchStatusConflictOnNoOp(t *testing.T) {
	svc := &fakeService{err: ErrSameStatus}

	rec := do(
		t, newTestRouter(svc), http.MethodPatch,
		"/applications/"+uuid.New().String()+"/status", `{"status":"Applied"}`,
	)
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusConflict)
	}
}

func TestPutUpdate(t *testing.T) {
	app := sampleApplication()
	svc := &fakeService{application: app}

	rec := do(
		t, newTestRouter(svc), http.MethodPut, "/applications/"+app.ID.String(),
		`{"company_name":"Stripe","job_title":"Staff Engineer","job_url":"https://linkedin.com/jobs/1"}`,
	)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if svc.updateIn.JobTitle != "Staff Engineer" {
		t.Errorf("job title reached the service as %q", svc.updateIn.JobTitle)
	}
}

func TestDeleteNoContent(t *testing.T) {
	app := sampleApplication()
	svc := &fakeService{}

	rec := do(t, newTestRouter(svc), http.MethodDelete, "/applications/"+app.ID.String(), "")

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if svc.deletedID != app.ID {
		t.Errorf("deleted id = %v, want %v", svc.deletedID, app.ID)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", rec.Body.String())
	}
}

func TestCheckDuplicateEndpoint(t *testing.T) {
	svc := &fakeService{
		duplicate: &DuplicateWarning{CompanyName: "Stripe", Matches: []Application{*sampleApplication()}},
	}

	rec := do(t, newTestRouter(svc), http.MethodGet, "/applications/check-duplicate?company=Stripe", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if svc.dupIn.Company != "Stripe" {
		t.Errorf("company reached the service as %q", svc.dupIn.Company)
	}
	if body := decodeBody(t, rec); body["duplicate"] != true {
		t.Errorf("duplicate = %v, want true", body["duplicate"])
	}
}

func TestCheckDuplicateNoMatch(t *testing.T) {
	rec := do(t, newTestRouter(&fakeService{}), http.MethodGet, "/applications/check-duplicate?company=Vercel", "")

	body := decodeBody(t, rec)
	if body["duplicate"] != false {
		t.Errorf("duplicate = %v, want false", body["duplicate"])
	}
	if matches, ok := body["matches"].([]any); !ok || len(matches) != 0 {
		t.Errorf("matches = %v, want an empty array", body["matches"])
	}
}

// The check-duplicate route is static and must not be swallowed by
// /applications/{id}.
func TestCheckDuplicateRouteBeatsIDRoute(t *testing.T) {
	svc := &fakeService{}

	rec := do(t, newTestRouter(svc), http.MethodGet, "/applications/check-duplicate?company=Stripe", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d — the id route captured check-duplicate", rec.Code, http.StatusOK)
	}
	if svc.dupIn.Company == "" {
		t.Error("the request did not reach CheckDuplicate")
	}
}

// TestCheckDuplicateEndpointPassesTitleAndSite — Phase 13. Both title and
// site_id turn "applied to this company" into "applied to this exact job",
// and site_id is what makes a match cross-site; the endpoint has to forward
// both rather than only company.
func TestCheckDuplicateEndpointPassesTitleAndSite(t *testing.T) {
	svc := &fakeService{}

	rec := do(
		t, newTestRouter(svc), http.MethodGet,
		"/applications/check-duplicate?company=Stripe&title=Backend+Engineer&site_id="+linkedIn.ID.String(), "",
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if svc.dupIn.JobTitle != "Backend Engineer" {
		t.Errorf("title reached the service as %q", svc.dupIn.JobTitle)
	}
	if svc.dupIn.SiteID == nil || *svc.dupIn.SiteID != linkedIn.ID {
		t.Errorf("site_id reached the service as %v", svc.dupIn.SiteID)
	}
}

func TestCheckDuplicateEndpointRejectsBadSiteID(t *testing.T) {
	rec := do(
		t, newTestRouter(&fakeService{}), http.MethodGet,
		"/applications/check-duplicate?company=Stripe&site_id=not-a-uuid", "",
	)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

// ---------------------------------------------------------------------------
// PATCH /applications/{id}/complete — Phase 13, Flow 2
// ---------------------------------------------------------------------------

func TestPatchCompleteAppliesTheCompletion(t *testing.T) {
	app := sampleApplication()
	due := fixedNow.Add(DefaultFollowUpDelay)
	cvVersionID := uuid.New()
	svc := &fakeService{completeResult: &CompleteResult{Application: app, FollowUpDueAt: &due}}

	rec := do(
		t, newTestRouter(svc), http.MethodPatch, "/applications/"+app.ID.String()+"/complete",
		`{"cv_version_id":"`+cvVersionID.String()+`","cover_letter_text":"Dear Acme, ...","note":"submitted via Greenhouse"}`,
	)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if svc.completeID != app.ID {
		t.Errorf("completed id = %v, want %v", svc.completeID, app.ID)
	}
	if svc.completeIn.CVVersionID == nil || *svc.completeIn.CVVersionID != cvVersionID {
		t.Errorf("cv_version_id reached the service as %v", svc.completeIn.CVVersionID)
	}
	if svc.completeIn.CoverLetterText == nil || *svc.completeIn.CoverLetterText != "Dear Acme, ..." {
		t.Errorf("cover_letter_text reached the service as %v", svc.completeIn.CoverLetterText)
	}
	if svc.completeIn.Note == nil || *svc.completeIn.Note != "submitted via Greenhouse" {
		t.Errorf("note reached the service as %v", svc.completeIn.Note)
	}

	body := decodeBody(t, rec)
	if _, ok := body["application"]; !ok {
		t.Error("response has no application object")
	}
	if body["follow_up_due_at"] == nil {
		t.Error("follow_up_due_at missing from the response")
	}
}

// An empty body still means something: "Mark as Complete" pressed with
// nothing attached is still a completion.
func TestPatchCompleteWithNoBodyIsStillValid(t *testing.T) {
	app := sampleApplication()
	svc := &fakeService{completeResult: &CompleteResult{Application: app}}

	rec := do(t, newTestRouter(svc), http.MethodPatch, "/applications/"+app.ID.String()+"/complete", `{}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if svc.completeIn.CVVersionID != nil || svc.completeIn.CoverLetterText != nil {
		t.Errorf("completeIn = %+v, want every field nil", svc.completeIn)
	}
}

// Completing an application already Applied is the double-submit case and
// comes back as a conflict, the same as re-applying the same status twice.
func TestPatchCompleteConflictWhenAlreadyApplied(t *testing.T) {
	svc := &fakeService{err: ErrSameStatus}

	rec := do(
		t, newTestRouter(svc), http.MethodPatch,
		"/applications/"+uuid.New().String()+"/complete", `{}`,
	)
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusConflict)
	}
}

func TestPatchCompleteBadID(t *testing.T) {
	rec := do(t, newTestRouter(&fakeService{}), http.MethodPatch, "/applications/not-a-uuid/complete", `{}`)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}
