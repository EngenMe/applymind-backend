package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

const testKey = "test-secret-key-12345"

func newProtectedHandler(t *testing.T) http.Handler {
	t.Helper()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return APIKeyAuth(testKey)(next)
}

func TestAPIKeyAuth_ValidKey_Passes(t *testing.T) {
	handler := newProtectedHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/applications", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestAPIKeyAuth_MissingHeader_Rejects(t *testing.T) {
	handler := newProtectedHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/applications", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestAPIKeyAuth_WrongKey_Rejects(t *testing.T) {
	handler := newProtectedHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/applications", nil)
	req.Header.Set("Authorization", "Bearer wrong-key")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestAPIKeyAuth_MissingBearerPrefix_Rejects(t *testing.T) {
	handler := newProtectedHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/applications", nil)
	// Raw key with no "Bearer " prefix must be rejected, not silently
	// tolerated — the prefix is part of the contract, not decoration.
	req.Header.Set("Authorization", testKey)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestAPIKeyAuth_EmptyBearerValue_Rejects(t *testing.T) {
	handler := newProtectedHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/applications", nil)
	req.Header.Set("Authorization", "Bearer ")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestAPIKeyAuth_KeyIsPrefixOfExpected_Rejects(t *testing.T) {
	handler := newProtectedHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/applications", nil)
	// A key that is a truncated prefix of the real one must still fail —
	// guards against a naive strings.HasPrefix-style comparison bug.
	req.Header.Set("Authorization", "Bearer "+testKey[:len(testKey)-1])
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestAPIKeyAuth_UnauthorizedBody_IsJSON(t *testing.T) {
	handler := newProtectedHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/applications", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("expected application/json content type, got %q", ct)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("expected a JSON error body, got empty response")
	}
}

func TestAPIKeyAuth_EmptyExpectedKey_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected APIKeyAuth(\"\") to panic, it did not")
		}
	}()
	APIKeyAuth("")
}
