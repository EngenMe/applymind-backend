package settings

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func newTestServer(t *testing.T) (*fakeRepo, http.Handler) {
	t.Helper()

	repo := &fakeRepo{}
	r := chi.NewRouter()
	NewHandler(NewService(repo), slog.New(slog.NewTextHandler(io.Discard, nil))).RegisterRoutes(r)
	return repo, r
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()

	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response body did not parse: %v\nbody: %s", err, rec.Body.String())
	}
	return out
}

func TestGetProfileSummaryBeforeItIsSet(t *testing.T) {
	_, h := newTestServer(t)

	rec := do(t, h, http.MethodGet, "/settings/profile-summary", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the resource exists even when the value does not", rec.Code)
	}

	// profile_summary is null because nothing has been written yet; updated_at
	// describes the seeded row itself and is allowed to be set.
	body := decodeBody[map[string]any](t, rec)
	if body["profile_summary"] != nil {
		t.Errorf("profile_summary = %v, want null", body["profile_summary"])
	}
	if _, ok := body["updated_at"]; !ok {
		t.Error("updated_at is missing from the response entirely")
	}
}

func TestPutThenGetProfileSummary(t *testing.T) {
	repo, h := newTestServer(t)

	rec := do(
		t, h, http.MethodPut, "/settings/profile-summary",
		`{"profile_summary": "  Backend engineer with six years of Go.  "}`,
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", rec.Code, rec.Body.String())
	}

	body := decodeBody[profileSummaryResponse](t, rec)
	if body.ProfileSummary == nil || *body.ProfileSummary != "Backend engineer with six years of Go." {
		t.Errorf("profile_summary = %v, want the trimmed summary", body.ProfileSummary)
	}
	if body.UpdatedAt == nil {
		t.Error("updated_at = null, want the write timestamped")
	}
	if repo.lastWritten != "Backend engineer with six years of Go." {
		t.Errorf("stored = %q, want the trimmed summary", repo.lastWritten)
	}

	rec = do(t, h, http.MethodGet, "/settings/profile-summary", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := decodeBody[profileSummaryResponse](t, rec)
	if got.ProfileSummary == nil || *got.ProfileSummary != "Backend engineer with six years of Go." {
		t.Errorf("profile_summary = %v, want what was just written", got.ProfileSummary)
	}
}

func TestPutProfileSummaryValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		code string
	}{
		{"empty", `{"profile_summary": ""}`, "profile_summary_required"},
		{"whitespace only", `{"profile_summary": "   "}`, "profile_summary_required"},
		{"field missing", `{}`, "profile_summary_required"},
		{
			"too long",
			`{"profile_summary": "` + strings.Repeat("a", MaxProfileSummaryLength+1) + `"}`,
			"profile_summary_too_long",
		},
		{"not json", `not json`, "invalid_body"},
	}

	for _, tc := range cases {
		t.Run(
			tc.name, func(t *testing.T) {
				repo, h := newTestServer(t)

				rec := do(t, h, http.MethodPut, "/settings/profile-summary", tc.body)
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400. body: %s", rec.Code, rec.Body.String())
				}

				var body errorBody
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("error body did not parse: %v", err)
				}
				if body.Error.Code != tc.code {
					t.Errorf("error code = %q, want %q", body.Error.Code, tc.code)
				}
				if repo.lastWritten != "" {
					t.Errorf("a rejected request wrote %q", repo.lastWritten)
				}
			},
		)
	}
}
