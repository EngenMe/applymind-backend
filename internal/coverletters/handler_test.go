package coverletters

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
	saveText func(ctx context.Context, in SaveTextInput) (*CoverLetter, error)
	saveFile func(ctx context.Context, in SaveFileInput) (*CoverLetter, error)
	editText func(ctx context.Context, applicationID uuid.UUID, body string) (*CoverLetter, error)
	get      func(ctx context.Context, applicationID uuid.UUID) (*CoverLetter, error)
	download func(ctx context.Context, applicationID uuid.UUID) (*DownloadLink, error)

	lastTextInput SaveTextInput
	lastFileInput SaveFileInput
	lastEditID    uuid.UUID
	lastEditBody  string
}

func (m *mockService) SaveText(ctx context.Context, in SaveTextInput) (*CoverLetter, error) {
	m.lastTextInput = in
	if m.saveText != nil {
		return m.saveText(ctx, in)
	}
	body := in.BodyText
	return &CoverLetter{
		ID:            uuid.New(),
		ApplicationID: in.ApplicationID,
		Kind:          KindText,
		BodyText:      &body,
	}, nil
}

func (m *mockService) SaveFile(ctx context.Context, in SaveFileInput) (*CoverLetter, error) {
	m.lastFileInput = in
	if m.saveFile != nil {
		return m.saveFile(ctx, in)
	}
	name := in.Filename
	key := "cover-letters/x/y/" + name
	return &CoverLetter{
		ID:               uuid.New(),
		ApplicationID:    in.ApplicationID,
		Kind:             KindFile,
		S3Key:            &key,
		OriginalFilename: &name,
	}, nil
}

func (m *mockService) EditText(ctx context.Context, applicationID uuid.UUID, body string) (*CoverLetter, error) {
	m.lastEditID, m.lastEditBody = applicationID, body
	if m.editText != nil {
		return m.editText(ctx, applicationID, body)
	}
	return &CoverLetter{ID: uuid.New(), ApplicationID: applicationID, Kind: KindText, BodyText: &body}, nil
}

func (m *mockService) Get(ctx context.Context, applicationID uuid.UUID) (*CoverLetter, error) {
	if m.get != nil {
		return m.get(ctx, applicationID)
	}
	return nil, ErrNotFound
}

func (m *mockService) DownloadURL(ctx context.Context, applicationID uuid.UUID) (*DownloadLink, error) {
	if m.download != nil {
		return m.download(ctx, applicationID)
	}
	return &DownloadLink{URL: "https://s3.example/x", ExpiresAt: time.Now().Add(time.Minute)}, nil
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

func coverLetterPath(appID uuid.UUID) string {
	return "/applications/" + appID.String() + "/coverletter"
}

func jsonRequest(method, url, body string) *http.Request {
	req := httptest.NewRequest(method, url, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func multipartRequest(t *testing.T, url, filename string, content []byte) *http.Request {
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
	} else if err := w.WriteField("note", "no file here"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, url, &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	return req
}

// ---------------------------------------------------------------------------
// POST /applications/{id}/coverletter
// ---------------------------------------------------------------------------

func TestHandlerSave_JSONBodyIsSavedAsText(t *testing.T) {
	appID := uuid.New()
	svc := &mockService{}
	mux := newTestServer(svc)

	rec := do(
		t, mux,
		jsonRequest(http.MethodPost, coverLetterPath(appID), `{"body_text":"Dear hiring manager,"}`),
	)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if svc.lastTextInput.ApplicationID != appID {
		t.Errorf("application id = %s, want %s", svc.lastTextInput.ApplicationID, appID)
	}
	if svc.lastTextInput.BodyText != "Dear hiring manager," {
		t.Errorf("body_text = %q", svc.lastTextInput.BodyText)
	}
	got := decode[coverLetterResponse](t, rec)
	if got.Kind != "text" {
		t.Errorf("kind = %q, want text", got.Kind)
	}
	if got.DownloadPath != "" {
		t.Error("a text cover letter must not advertise a download path")
	}
}

func TestHandlerSave_MultipartIsSavedAsFile(t *testing.T) {
	appID := uuid.New()
	svc := &mockService{}
	mux := newTestServer(svc)

	rec := do(t, mux, multipartRequest(t, coverLetterPath(appID), "cover_letter.pdf", []byte("%PDF fake")))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if svc.lastFileInput.ApplicationID != appID {
		t.Errorf("application id = %s, want %s", svc.lastFileInput.ApplicationID, appID)
	}
	if svc.lastFileInput.Filename != "cover_letter.pdf" {
		t.Errorf("filename = %q", svc.lastFileInput.Filename)
	}
	if string(svc.lastFileInput.Content) != "%PDF fake" {
		t.Error("file content did not reach the service intact")
	}
	got := decode[coverLetterResponse](t, rec)
	if got.Kind != "file" {
		t.Errorf("kind = %q, want file", got.Kind)
	}
	if got.DownloadPath != coverLetterPath(appID)+"/download" {
		t.Errorf("download_path = %q", got.DownloadPath)
	}
	if got.BodyText != nil {
		t.Error("a file cover letter must not carry body_text")
	}
}

func TestHandlerSave_UnsupportedFileTypeIs415(t *testing.T) {
	svc := &mockService{saveFile: func(context.Context, SaveFileInput) (*CoverLetter, error) {
		return nil, ErrUnsupportedFileType
	}}
	mux := newTestServer(svc)

	rec := do(t, mux, multipartRequest(t, coverLetterPath(uuid.New()), "notes.txt", []byte("x")))
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", rec.Code)
	}
	if code := decode[errorBody](t, rec).Error.Code; code != "unsupported_file_type" {
		t.Errorf("error code = %q", code)
	}
}

func TestHandlerSave_MissingFileFieldIs400(t *testing.T) {
	mux := newTestServer(&mockService{})
	rec := do(t, mux, multipartRequest(t, coverLetterPath(uuid.New()), "", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandlerSave_InvalidJSONIs400(t *testing.T) {
	mux := newTestServer(&mockService{})
	rec := do(t, mux, jsonRequest(http.MethodPost, coverLetterPath(uuid.New()), `{`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandlerSave_BlankBodyIs400(t *testing.T) {
	svc := &mockService{saveText: func(context.Context, SaveTextInput) (*CoverLetter, error) {
		return nil, ErrEmptyBody
	}}
	mux := newTestServer(svc)
	rec := do(t, mux, jsonRequest(http.MethodPost, coverLetterPath(uuid.New()), `{"body_text":"   "}`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandlerSave_BadApplicationIDIs400(t *testing.T) {
	mux := newTestServer(&mockService{})
	rec := do(t, mux, jsonRequest(http.MethodPost, "/applications/not-a-uuid/coverletter", `{"body_text":"x"}`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandlerSave_UnknownApplicationIs404(t *testing.T) {
	svc := &mockService{saveText: func(context.Context, SaveTextInput) (*CoverLetter, error) {
		return nil, ErrApplicationNotFound
	}}
	mux := newTestServer(svc)
	rec := do(t, mux, jsonRequest(http.MethodPost, coverLetterPath(uuid.New()), `{"body_text":"x"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if code := decode[errorBody](t, rec).Error.Code; code != "application_not_found" {
		t.Errorf("error code = %q", code)
	}
}

func TestHandlerSave_ServiceFailureIs500(t *testing.T) {
	svc := &mockService{saveText: func(context.Context, SaveTextInput) (*CoverLetter, error) {
		return nil, errors.New("neon is down")
	}}
	mux := newTestServer(svc)
	rec := do(t, mux, jsonRequest(http.MethodPost, coverLetterPath(uuid.New()), `{"body_text":"x"}`))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if body := decode[errorBody](t, rec); body.Error.Message == "neon is down" {
		t.Error("internal error details must not leak to the client")
	}
}

// ---------------------------------------------------------------------------
// GET /applications/{id}/coverletter
// ---------------------------------------------------------------------------

func TestHandlerGet_SerialisesBothKinds(t *testing.T) {
	appID := uuid.New()

	t.Run("text", func(t *testing.T) {
		svc := &mockService{get: func(context.Context, uuid.UUID) (*CoverLetter, error) {
			body := "Dear hiring manager,"
			return &CoverLetter{ID: uuid.New(), ApplicationID: appID, Kind: KindText, BodyText: &body}, nil
		}}
		rec := do(t, newTestServer(svc), httptest.NewRequest(http.MethodGet, coverLetterPath(appID), nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		got := decode[coverLetterResponse](t, rec)
		if got.Kind != "text" || got.BodyText == nil || *got.BodyText != "Dear hiring manager," {
			t.Errorf("body = %+v, want the typed text", got)
		}
		if got.OriginalFilename != nil {
			t.Error("a text cover letter has no filename")
		}
	})

	t.Run("file", func(t *testing.T) {
		svc := &mockService{get: func(context.Context, uuid.UUID) (*CoverLetter, error) {
			key, name := "cover-letters/a/b/letter.docx", "letter.docx"
			return &CoverLetter{
				ID: uuid.New(), ApplicationID: appID, Kind: KindFile,
				S3Key: &key, OriginalFilename: &name,
			}, nil
		}}
		rec := do(t, newTestServer(svc), httptest.NewRequest(http.MethodGet, coverLetterPath(appID), nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		got := decode[coverLetterResponse](t, rec)
		if got.Kind != "file" || got.OriginalFilename == nil || *got.OriginalFilename != "letter.docx" {
			t.Errorf("body = %+v, want the file reference", got)
		}
		if got.DownloadPath != coverLetterPath(appID)+"/download" {
			t.Errorf("download_path = %q", got.DownloadPath)
		}
		// The S3 key is an implementation detail and must not be exposed.
		if bytes.Contains(rec.Body.Bytes(), []byte("cover-letters/a/b")) {
			t.Error("the s3 key leaked into the response")
		}
	})
}

func TestHandlerGet_NoCoverLetterIs404(t *testing.T) {
	mux := newTestServer(&mockService{}) // default Get returns ErrNotFound
	rec := do(t, mux, httptest.NewRequest(http.MethodGet, coverLetterPath(uuid.New()), nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if code := decode[errorBody](t, rec).Error.Code; code != "cover_letter_not_found" {
		t.Errorf("error code = %q", code)
	}
}

func TestHandlerGet_BadApplicationIDIs400(t *testing.T) {
	mux := newTestServer(&mockService{})
	rec := do(t, mux, httptest.NewRequest(http.MethodGet, "/applications/not-a-uuid/coverletter", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// PUT /applications/{id}/coverletter
// ---------------------------------------------------------------------------

func TestHandlerEditText_Success(t *testing.T) {
	appID := uuid.New()
	svc := &mockService{}
	mux := newTestServer(svc)

	rec := do(t, mux, jsonRequest(http.MethodPut, coverLetterPath(appID), `{"body_text":"rewritten"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if svc.lastEditID != appID || svc.lastEditBody != "rewritten" {
		t.Errorf("edit called with (%s, %q)", svc.lastEditID, svc.lastEditBody)
	}
	if got := decode[coverLetterResponse](t, rec); got.Kind != "text" {
		t.Errorf("kind = %q", got.Kind)
	}
}

func TestHandlerEditText_FileKindIs409(t *testing.T) {
	svc := &mockService{editText: func(context.Context, uuid.UUID, string) (*CoverLetter, error) {
		return nil, ErrNotTextKind
	}}
	mux := newTestServer(svc)

	rec := do(t, mux, jsonRequest(http.MethodPut, coverLetterPath(uuid.New()), `{"body_text":"x"}`))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if code := decode[errorBody](t, rec).Error.Code; code != "not_editable" {
		t.Errorf("error code = %q", code)
	}
}

func TestHandlerEditText_MissingCoverLetterIs404(t *testing.T) {
	svc := &mockService{editText: func(context.Context, uuid.UUID, string) (*CoverLetter, error) {
		return nil, ErrNotFound
	}}
	mux := newTestServer(svc)
	rec := do(t, mux, jsonRequest(http.MethodPut, coverLetterPath(uuid.New()), `{"body_text":"x"}`))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// GET /applications/{id}/coverletter/download
// ---------------------------------------------------------------------------

func TestHandlerDownload_ReturnsPresignedURL(t *testing.T) {
	appID := uuid.New()
	expires := time.Now().Add(15 * time.Minute).UTC().Truncate(time.Second)
	var gotApp uuid.UUID

	svc := &mockService{download: func(_ context.Context, id uuid.UUID) (*DownloadLink, error) {
		gotApp = id
		return &DownloadLink{URL: "https://s3.example/signed", Filename: "letter.pdf", ExpiresAt: expires}, nil
	}}
	mux := newTestServer(svc)

	rec := do(t, mux, httptest.NewRequest(http.MethodGet, coverLetterPath(appID)+"/download", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if gotApp != appID {
		t.Error("the path parameter was not extracted correctly")
	}
	got := decode[downloadResponse](t, rec)
	if got.URL != "https://s3.example/signed" || got.Filename != "letter.pdf" {
		t.Errorf("body = %+v", got)
	}
	if !got.ExpiresAt.Equal(expires) {
		t.Errorf("expires_at = %v, want %v", got.ExpiresAt, expires)
	}
}

func TestHandlerDownload_TextKindIs409(t *testing.T) {
	svc := &mockService{download: func(context.Context, uuid.UUID) (*DownloadLink, error) {
		return nil, ErrNotFileKind
	}}
	mux := newTestServer(svc)

	rec := do(t, mux, httptest.NewRequest(http.MethodGet, coverLetterPath(uuid.New())+"/download", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if code := decode[errorBody](t, rec).Error.Code; code != "no_file_to_download" {
		t.Errorf("error code = %q", code)
	}
}

func TestHandlerDownload_NoCoverLetterIs404(t *testing.T) {
	svc := &mockService{download: func(context.Context, uuid.UUID) (*DownloadLink, error) {
		return nil, ErrNotFound
	}}
	mux := newTestServer(svc)
	rec := do(t, mux, httptest.NewRequest(http.MethodGet, coverLetterPath(uuid.New())+"/download", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
