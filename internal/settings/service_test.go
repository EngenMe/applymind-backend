package settings

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeRepo is an in-memory Repository. missing models the settings row not
// existing at all, which is what a broken install looks like.
type fakeRepo struct {
	current *Settings
	missing bool
	getErr  error
	setErr  error

	lastWritten string
}

func (f *fakeRepo) Get(context.Context) (*Settings, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.missing {
		return nil, ErrNotFound
	}
	if f.current == nil {
		return &Settings{CreatedAt: fixedNow, UpdatedAt: fixedNow}, nil
	}
	clone := *f.current
	return &clone, nil
}

func (f *fakeRepo) SetProfileSummary(_ context.Context, summary string) (*Settings, error) {
	if f.setErr != nil {
		return nil, f.setErr
	}
	f.lastWritten = summary
	f.missing = false
	f.current = &Settings{ProfileSummary: &summary, CreatedAt: fixedNow, UpdatedAt: fixedNow}
	clone := *f.current
	return &clone, nil
}

var fixedNow = time.Date(2026, 5, 16, 9, 0, 0, 0, time.UTC)

func newTestService() (*fakeRepo, Service) {
	repo := &fakeRepo{}
	return repo, NewService(repo)
}

// ---------------------------------------------------------------------------
// Set
// ---------------------------------------------------------------------------

func TestSetProfileSummaryStoresTrimmed(t *testing.T) {
	repo, svc := newTestService()

	updated, err := svc.SetProfileSummary(
		context.Background(),
		"  Backend engineer with six years of Go and Postgres.  ",
	)
	if err != nil {
		t.Fatalf("SetProfileSummary: unexpected error: %v", err)
	}
	want := "Backend engineer with six years of Go and Postgres."
	if repo.lastWritten != want {
		t.Errorf("stored = %q, want it trimmed to %q", repo.lastWritten, want)
	}
	if updated.Summary() != want {
		t.Errorf("returned = %q, want %q", updated.Summary(), want)
	}
	if !updated.HasProfileSummary() {
		t.Error("HasProfileSummary = false after a successful write")
	}
}

func TestSetProfileSummaryRejectsBlank(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\t "} {
		repo, svc := newTestService()

		if _, err := svc.SetProfileSummary(context.Background(), in); !errors.Is(err, ErrSummaryRequired) {
			t.Errorf("input %q: error = %v, want %v", in, err, ErrSummaryRequired)
		}
		if repo.lastWritten != "" {
			t.Errorf("input %q: nothing should have been written, got %q", in, repo.lastWritten)
		}
	}
}

func TestSetProfileSummaryRejectsOverlongSummary(t *testing.T) {
	_, svc := newTestService()

	tooLong := strings.Repeat("a", MaxProfileSummaryLength+1)
	if _, err := svc.SetProfileSummary(context.Background(), tooLong); !errors.Is(err, ErrSummaryTooLong) {
		t.Errorf("error = %v, want %v", err, ErrSummaryTooLong)
	}

	// Exactly at the cap is fine, and the cap counts characters rather than
	// bytes — a summary of accented characters gets the same allowance.
	atCap := strings.Repeat("é", MaxProfileSummaryLength)
	if _, err := svc.SetProfileSummary(context.Background(), atCap); err != nil {
		t.Errorf("a summary exactly at the cap was rejected: %v", err)
	}
}

func TestSetProfileSummaryOverwrites(t *testing.T) {
	repo, svc := newTestService()

	if _, err := svc.SetProfileSummary(context.Background(), "First version."); err != nil {
		t.Fatalf("SetProfileSummary: unexpected error: %v", err)
	}
	if _, err := svc.SetProfileSummary(context.Background(), "Second version."); err != nil {
		t.Fatalf("SetProfileSummary: unexpected error: %v", err)
	}
	if repo.lastWritten != "Second version." {
		t.Errorf("stored = %q, want the latest write", repo.lastWritten)
	}
}

func TestSetProfileSummaryHealsAMissingRow(t *testing.T) {
	repo, svc := newTestService()
	repo.missing = true

	if _, err := svc.SetProfileSummary(context.Background(), "Backend engineer."); err != nil {
		t.Fatalf("SetProfileSummary: an upsert should recreate a missing row, got: %v", err)
	}

	got, err := svc.ProfileSummary(context.Background())
	if err != nil {
		t.Fatalf("ProfileSummary: unexpected error: %v", err)
	}
	if got != "Backend engineer." {
		t.Errorf("summary = %q, want it readable after the write", got)
	}
}

// ---------------------------------------------------------------------------
// Get
// ---------------------------------------------------------------------------

func TestGetBeforeAnythingIsSet(t *testing.T) {
	_, svc := newTestService()

	current, err := svc.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: unexpected error: %v", err)
	}
	if current.HasProfileSummary() {
		t.Errorf("summary = %q, want nothing set yet", current.Summary())
	}
}

func TestGetTreatsAMissingRowAsUnset(t *testing.T) {
	repo, svc := newTestService()
	repo.missing = true

	current, err := svc.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: a missing row should read as unset, got error: %v", err)
	}
	if current.HasProfileSummary() {
		t.Error("summary should be empty when the row is missing")
	}
	if !current.UpdatedAt.IsZero() {
		t.Error("updated at should stay zero when there is no row to read it from")
	}
}

func TestGetPropagatesRealFailures(t *testing.T) {
	repo, svc := newTestService()
	repo.getErr = errors.New("connection refused")

	if _, err := svc.Get(context.Background()); err == nil {
		t.Fatal("Get: a database failure must not be swallowed as unset")
	}
}

// ---------------------------------------------------------------------------
// ProfileSummary — the adapter the applications module scores against
// ---------------------------------------------------------------------------

func TestProfileSummaryAdapter(t *testing.T) {
	repo, svc := newTestService()

	got, err := svc.ProfileSummary(context.Background())
	if err != nil {
		t.Fatalf("ProfileSummary: unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("summary = %q, want an empty string before one is set", got)
	}

	if _, err := svc.SetProfileSummary(context.Background(), "Backend engineer."); err != nil {
		t.Fatalf("SetProfileSummary: unexpected error: %v", err)
	}
	if got, err = svc.ProfileSummary(context.Background()); err != nil || got != "Backend engineer." {
		t.Errorf("summary = %q (err %v), want the stored value", got, err)
	}

	repo.getErr = errors.New("connection refused")
	if _, err := svc.ProfileSummary(context.Background()); err == nil {
		t.Error("ProfileSummary: a database failure must be reported, not read as unset")
	}
}
