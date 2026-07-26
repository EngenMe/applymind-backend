package cvs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type mockService struct {
	match        func(ctx context.Context, q MatchQuery) (*MatchResult, error)
	upload       func(ctx context.Context, in UploadInput) (*UploadResult, error)
	list         func(ctx context.Context) ([]CV, error)
	listVersions func(ctx context.Context, cvID uuid.UUID) ([]CVVersion, error)
	download     func(ctx context.Context, cvID, versionID uuid.UUID) (*DownloadLink, error)
	usage        func(ctx context.Context, versionID uuid.UUID) ([]ApplicationUsage, error)

	lastMatchQuery  MatchQuery
	lastUploadInput UploadInput
}

func (m *mockService) Match(ctx context.Context, q MatchQuery) (*MatchResult, error) {
	m.lastMatchQuery = q
	if m.match != nil {
		return m.match(ctx, q)
	}
	return &MatchResult{Outcome: OutcomeUnknown}, nil
}

func (m *mockService) Upload(ctx context.Context, in UploadInput) (*UploadResult, error) {
	m.lastUploadInput = in
	if m.upload != nil {
		return m.upload(ctx, in)
	}
	cv := &CV{ID: uuid.New(), Name: "CV"}
	return &UploadResult{CV: cv, Version: &CVVersion{ID: uuid.New(), CVID: cv.ID}}, nil
}

func (m *mockService) ListWithVersions(ctx context.Context) ([]CV, error) {
	if m.list != nil {
		return m.list(ctx)
	}
	return nil, nil
}

func (m *mockService) ListVersions(ctx context.Context, cvID uuid.UUID) ([]CVVersion, error) {
	if m.listVersions != nil {
		return m.listVersions(ctx, cvID)
	}
	return nil, nil
}

func (m *mockService) DownloadURL(ctx context.Context, cvID, versionID uuid.UUID) (*DownloadLink, error) {
	if m.download != nil {
		return m.download(ctx, cvID, versionID)
	}
	return &DownloadLink{URL: "https://s3.example/x", ExpiresAt: time.Now().Add(time.Minute)}, nil
}

func (m *mockService) ApplicationsUsingVersion(ctx context.Context, versionID uuid.UUID) ([]ApplicationUsage, error) {
	if m.usage != nil {
		return m.usage(ctx, versionID)
	}
	return nil, nil
}

// newTestServer wires the handler onto a real chi.Router so path patterns
// resolve the same way they do in production.
func newTestServer(svc Service) chi.Router {
	r := chi.NewRouter()
	NewHandler(svc, slog.New(slog.NewTextHandler(io.Discard, nil))).RegisterRoutes(r)
	return r
}

func do(t *testing.T, r chi.Router, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding response %q: %v", rec.Body.String(), err)
	}
	return out
}

// ---------------------------------------------------------------------------
// POST /cvs/match
// ---------------------------------------------------------------------------

func TestHandlerMatch_PassesHashNameAndSizeThrough(t *testing.T) {
	svc := &mockService{}
	mux := newTestServer(svc)

	body := `{"sha256_hash":"abc","filename":"cv.pdf","file_size_bytes":4096}`
	req := httptest.NewRequest(http.MethodPost, "/cvs/match", bytes.NewBufferString(body))
	rec := do(t, mux, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := svc.lastMatchQuery
	if got.SHA256Hash != "abc" || got.Filename != "cv.pdf" {
		t.Errorf("query = %+v", got)
	}
	if got.FileSizeBytes == nil || *got.FileSizeBytes != 4096 {
		t.Error("file size was not passed to the service")
	}
}

func TestHandlerMatch_OmittedSizeArrivesAsNil(t *testing.T) {
	svc := &mockService{}
	mux := newTestServer(svc)

	req := httptest.NewRequest(http.MethodPost, "/cvs/match", bytes.NewBufferString(`{"filename":"cv.pdf"}`))
	do(t, mux, req)

	if svc.lastMatchQuery.FileSizeBytes != nil {
		t.Error("an omitted file_size_bytes must reach the service as nil, not 0")
	}
}

func TestHandlerMatch_SerialisesEachOutcome(t *testing.T) {
	cvID, versionID := uuid.New(), uuid.New()
	appliedAt := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	company := "Stripe"

	cases := []struct {
		name         string
		result       *MatchResult
		wantOutcome  string
		wantMatched  string
		wantCV       bool
		wantVersion  bool
		wantConfirm  bool
		wantHTTPCode int
	}{
		{
			name: "matched by hash",
			result: &MatchResult{
				Outcome:   OutcomeMatched,
				MatchedBy: MatchedByHash,
				CV:        &CV{ID: cvID, Name: "Backend CV"},
				Version:   &CVVersion{ID: versionID, CVID: cvID},
			},
			wantOutcome: "matched", wantMatched: "hash",
			wantCV: true, wantVersion: true, wantHTTPCode: http.StatusOK,
		},
		{
			name:        "new version",
			result:      &MatchResult{Outcome: OutcomeNewVersion, CV: &CV{ID: cvID, Name: "Backend CV"}},
			wantOutcome: "new_version", wantCV: true, wantHTTPCode: http.StatusOK,
		},
		{
			name: "needs confirmation",
			result: &MatchResult{
				Outcome:      OutcomeNeedsConfirmation,
				CV:           &CV{ID: cvID, Name: "Backend CV"},
				Version:      &CVVersion{ID: versionID, CVID: cvID},
				Confirmation: &ConfirmationDetails{CVName: "Backend CV", LastUsedAt: &appliedAt, LastCompany: &company},
			},
			wantOutcome: "needs_confirmation",
			wantCV:      true, wantVersion: true, wantConfirm: true, wantHTTPCode: http.StatusOK,
		},
		{
			name:        "unknown",
			result:      &MatchResult{Outcome: OutcomeUnknown},
			wantOutcome: "unknown", wantHTTPCode: http.StatusOK,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &mockService{match: func(context.Context, MatchQuery) (*MatchResult, error) {
				return tc.result, nil
			}}
			mux := newTestServer(svc)
			req := httptest.NewRequest(http.MethodPost, "/cvs/match", bytes.NewBufferString(`{"filename":"cv.pdf"}`))
			rec := do(t, mux, req)

			if rec.Code != tc.wantHTTPCode {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantHTTPCode)
			}
			got := decode[matchResponse](t, rec)
			if got.Outcome != tc.wantOutcome {
				t.Errorf("outcome = %q, want %q", got.Outcome, tc.wantOutcome)
			}
			if got.MatchedBy != tc.wantMatched {
				t.Errorf("matched_by = %q, want %q", got.MatchedBy, tc.wantMatched)
			}
			if (got.CV != nil) != tc.wantCV {
				t.Errorf("cv present = %v, want %v", got.CV != nil, tc.wantCV)
			}
			if (got.Version != nil) != tc.wantVersion {
				t.Errorf("version present = %v, want %v", got.Version != nil, tc.wantVersion)
			}
			if (got.Confirmation != nil) != tc.wantConfirm {
				t.Errorf("confirmation present = %v, want %v", got.Confirmation != nil, tc.wantConfirm)
			}
		})
	}
}

func TestHandlerMatch_InvalidJSON(t *testing.T) {
	mux := newTestServer(&mockService{})
	req := httptest.NewRequest(http.MethodPost, "/cvs/match", bytes.NewBufferString(`{`))
	if rec := do(t, mux, req); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandlerMatch_MissingFilenameIsBadRequest(t *testing.T) {
	svc := &mockService{match: func(context.Context, MatchQuery) (*MatchResult, error) {
		return nil, ErrFilenameEmpty
	}}
	mux := newTestServer(svc)
	req := httptest.NewRequest(http.MethodPost, "/cvs/match", bytes.NewBufferString(`{}`))
	if rec := do(t, mux, req); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandlerMatch_ServiceFailureIs500(t *testing.T) {
	svc := &mockService{match: func(context.Context, MatchQuery) (*MatchResult, error) {
		return nil, errors.New("neon is down")
	}}
	mux := newTestServer(svc)
	req := httptest.NewRequest(http.MethodPost, "/cvs/match", bytes.NewBufferString(`{"filename":"cv.pdf"}`))
	rec := do(t, mux, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if body := decode[errorBody](t, rec); body.Error.Message == "neon is down" {
		t.Error("internal error details must not leak to the client")
	}
}

// ---------------------------------------------------------------------------
// POST /cvs
// ---------------------------------------------------------------------------

func multipartUpload(t *testing.T, filename string, content []byte, fields map[string]string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if filename != "" {
		part, err := w.CreateFormFile("file", filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/cvs", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	return req
}

func TestHandlerUpload_HappyPath(t *testing.T) {
	svc := &mockService{}
	mux := newTestServer(svc)

	rec := do(t, mux, multipartUpload(t, "backend_cv.pdf", []byte("%PDF fake"), nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if svc.lastUploadInput.Filename != "backend_cv.pdf" {
		t.Errorf("filename = %q", svc.lastUploadInput.Filename)
	}
	if string(svc.lastUploadInput.Content) != "%PDF fake" {
		t.Error("file content did not reach the service intact")
	}
	if svc.lastUploadInput.CVID != nil {
		t.Error("cv_id must be nil when the form omits it")
	}
}

func TestHandlerUpload_WithCVIDAndTag(t *testing.T) {
	cvID := uuid.New()
	svc := &mockService{}
	mux := newTestServer(svc)

	req := multipartUpload(t, "cv.pdf", []byte("x"), map[string]string{
		"cv_id": cvID.String(),
		"tag":   "backend",
	})
	if rec := do(t, mux, req); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	if svc.lastUploadInput.CVID == nil || *svc.lastUploadInput.CVID != cvID {
		t.Error("cv_id was not forwarded to the service")
	}
	if svc.lastUploadInput.Tag == nil || *svc.lastUploadInput.Tag != "backend" {
		t.Error("tag was not forwarded to the service")
	}
}

func TestHandlerUpload_AlreadyExistingVersionReturns200(t *testing.T) {
	svc := &mockService{upload: func(context.Context, UploadInput) (*UploadResult, error) {
		cv := &CV{ID: uuid.New()}
		return &UploadResult{CV: cv, Version: &CVVersion{ID: uuid.New(), CVID: cv.ID}, AlreadyExisted: true}, nil
	}}
	mux := newTestServer(svc)

	rec := do(t, mux, multipartUpload(t, "cv.pdf", []byte("x"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for an unchanged re-upload", rec.Code)
	}
	if !decode[uploadResponse](t, rec).AlreadyExisted {
		t.Error("already_existed should be true")
	}
}

func TestHandlerUpload_MissingFileField(t *testing.T) {
	mux := newTestServer(&mockService{})
	rec := do(t, mux, multipartUpload(t, "", nil, map[string]string{"tag": "backend"}))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandlerUpload_BadCVID(t *testing.T) {
	mux := newTestServer(&mockService{})
	rec := do(t, mux, multipartUpload(t, "cv.pdf", []byte("x"), map[string]string{"cv_id": "not-a-uuid"}))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandlerUpload_TooLargeIs413(t *testing.T) {
	svc := &mockService{upload: func(context.Context, UploadInput) (*UploadResult, error) {
		return nil, ErrFileTooLarge
	}}
	mux := newTestServer(svc)
	rec := do(t, mux, multipartUpload(t, "cv.pdf", []byte("x"), nil))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// GET routes
// ---------------------------------------------------------------------------

func TestHandlerList_EmptyListSerialisesAsArray(t *testing.T) {
	mux := newTestServer(&mockService{})
	rec := do(t, mux, httptest.NewRequest(http.MethodGet, "/cvs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Body.String(); got != "{\"cvs\":[]}\n" {
		t.Errorf("body = %q, want an empty array rather than null", got)
	}
}

func TestHandlerListVersions_BadUUIDIs400(t *testing.T) {
	mux := newTestServer(&mockService{})
	rec := do(t, mux, httptest.NewRequest(http.MethodGet, "/cvs/not-a-uuid/versions", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandlerListVersions_UnknownCVIs404(t *testing.T) {
	svc := &mockService{listVersions: func(context.Context, uuid.UUID) ([]CVVersion, error) {
		return nil, ErrCVNotFound
	}}
	mux := newTestServer(svc)
	rec := do(t, mux, httptest.NewRequest(http.MethodGet, "/cvs/"+uuid.New().String()+"/versions", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandlerDownload_ReturnsPresignedURL(t *testing.T) {
	cvID, versionID := uuid.New(), uuid.New()
	expires := time.Now().Add(15 * time.Minute).UTC().Truncate(time.Second)
	var gotCV, gotVersion uuid.UUID

	svc := &mockService{download: func(_ context.Context, cv, version uuid.UUID) (*DownloadLink, error) {
		gotCV, gotVersion = cv, version
		return &DownloadLink{URL: "https://s3.example/signed", Filename: "cv.pdf", ExpiresAt: expires}, nil
	}}
	mux := newTestServer(svc)

	rec := do(t, mux, httptest.NewRequest(http.MethodGet,
		"/cvs/"+cvID.String()+"/versions/"+versionID.String()+"/download", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if gotCV != cvID || gotVersion != versionID {
		t.Error("path parameters were not extracted correctly")
	}
	got := decode[downloadResponse](t, rec)
	if got.URL != "https://s3.example/signed" || got.Filename != "cv.pdf" {
		t.Errorf("body = %+v", got)
	}
	if !got.ExpiresAt.Equal(expires) {
		t.Errorf("expires_at = %v, want %v", got.ExpiresAt, expires)
	}
}

func TestHandlerDownload_VersionNotFoundIs404(t *testing.T) {
	svc := &mockService{download: func(context.Context, uuid.UUID, uuid.UUID) (*DownloadLink, error) {
		return nil, ErrVersionNotFound
	}}
	mux := newTestServer(svc)
	rec := do(t, mux, httptest.NewRequest(http.MethodGet,
		"/cvs/"+uuid.New().String()+"/versions/"+uuid.New().String()+"/download", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
