package applications

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/EngenMe/applymind-backend/pkg/ai"
)

// ---------------------------------------------------------------------------
// Fakes
//
// fakeRepo, fakeCoverLetters and the fixtures live in service_test.go. This file
// only adds what AI scoring introduced — the new repository method, a wrapper
// that can make it fail, and fakes for the two scoring dependencies — so the
// existing test file did not have to change.
//
// CVVersionOwnedByUser is added here too, defaulting to true (owned). It is a
// phase 15 addition unrelated to scoring, but every Create call in this file
// now passes through checkCVVersionOwnership regardless of whether the test
// cares about CV versions, so fakeRepo has to answer it one way or the other —
// none of these tests supply a CVVersionID, so it is never actually exercised,
// only satisfied.
// ---------------------------------------------------------------------------

func (f *fakeRepo) CVVersionOwnedByUser(_ context.Context, _ uuid.UUID, _ uuid.UUID) (bool, error) {
	return true, nil
}

// SetAIScore completes fakeRepo's Repository implementation.
func (f *fakeRepo) SetAIScore(_ context.Context, _ uuid.UUID, id uuid.UUID, score float64, explanation *string) (
	*Application,
	error,
) {
	app, ok := f.apps[id]
	if !ok {
		return nil, ErrNotFound
	}
	s := score
	app.AIScore = &s
	app.AIScoreExplanation = explanation
	return copyApp(app), nil
}

// scoringRepo counts score writes and can make them fail.
//
// It overrides Tx as well as SetAIScore because fakeRepo.Tx hands its own
// receiver to fn: without this, the writes inside Create's transaction would go
// straight to the embedded fake and skip everything below.
type scoringRepo struct {
	*fakeRepo

	setErr   error
	setCalls int
}

func newScoringRepo() *scoringRepo {
	repo := newFakeRepo()
	repo.sites[linkedIn.Domain] = linkedIn
	return &scoringRepo{fakeRepo: repo}
}

func (r *scoringRepo) Tx(ctx context.Context, fn func(Repository) error) error {
	r.txCalls++
	return fn(r)
}

func (r *scoringRepo) SetAIScore(ctx context.Context, userID, id uuid.UUID, score float64, explanation *string) (
	*Application,
	error,
) {
	r.setCalls++
	if r.setErr != nil {
		return nil, r.setErr
	}
	return r.fakeRepo.SetAIScore(ctx, userID, id, score, explanation)
}

type fakeScorer struct {
	score *ai.Score
	err   error

	calls     int
	lastInput ai.ScoreInput
	// block simulates a slow model. It respects the context, so the scoring
	// timeout can be tested without waiting for one.
	block time.Duration
}

func (f *fakeScorer) ScoreJob(ctx context.Context, in ai.ScoreInput) (*ai.Score, error) {
	f.calls++
	f.lastInput = in

	if f.block > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(f.block):
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.score, nil
}

type fakeProfiles struct {
	summary string
	err     error
	calls   int
}

func (f *fakeProfiles) ProfileSummary(context.Context) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return f.summary, nil
}

const testProfileSummary = "Backend engineer with six years of Go, Postgres and AWS experience."

func newScoringService(
	t *testing.T,
	scorer Scorer,
	profiles ProfileSummaries,
	opts ...ServiceOption,
) (*scoringRepo, Service) {
	t.Helper()

	repo := newScoringRepo()
	base := []ServiceOption{
		WithClock(func() time.Time { return fixedNow }),
		WithIDGenerator(func() uuid.UUID { return newAppID }),
		// Scoring logs every time it gives up; discard it so a passing run stays
		// quiet and a failing one stays readable.
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithScoring(scorer, profiles),
	}

	return repo, NewService(repo, &fakeCoverLetters{}, append(base, opts...)...)
}

func scoredOK() *fakeScorer {
	return &fakeScorer{score: &ai.Score{Score: 8, Explanation: "You match well. Little Kubernetes experience."}}
}

// ---------------------------------------------------------------------------
// Successful scoring
// ---------------------------------------------------------------------------

func TestCreateStoresAIScore(t *testing.T) {
	repo, svc := newScoringService(t, scoredOK(), &fakeProfiles{summary: testProfileSummary})

	result, err := svc.Create(context.Background(), testUserID, validInput())
	if err != nil {
		t.Fatalf("Create: unexpected error: %v", err)
	}

	app := result.Application
	if app.AIScore == nil || *app.AIScore != 8 {
		t.Fatalf("ai score = %v, want 8 on the returned application", app.AIScore)
	}
	if app.AIScoreExplanation == nil || !strings.HasPrefix(*app.AIScoreExplanation, "You match well.") {
		t.Errorf("explanation = %v, want the model's two sentences", app.AIScoreExplanation)
	}

	// Stored, not just returned: GET /applications/{id} reads it back from here.
	stored := repo.apps[app.ID]
	if stored.AIScore == nil || *stored.AIScore != 8 {
		t.Errorf("stored ai score = %v, want 8", stored.AIScore)
	}
	if repo.setCalls != 1 {
		t.Errorf("score writes = %d, want exactly 1", repo.setCalls)
	}
	if repo.txCalls != 1 {
		t.Errorf("transactions = %d, want the score written in the same one as the row", repo.txCalls)
	}
}

func TestCreateScoresAgainstTheProfileAndTheJob(t *testing.T) {
	scorer := scoredOK()
	profiles := &fakeProfiles{summary: testProfileSummary}
	_, svc := newScoringService(t, scorer, profiles)

	if _, err := svc.Create(context.Background(), testUserID, validInput()); err != nil {
		t.Fatalf("Create: unexpected error: %v", err)
	}

	if scorer.calls != 1 {
		t.Fatalf("scorer calls = %d, want exactly 1 per save", scorer.calls)
	}
	if profiles.calls != 1 {
		t.Errorf("profile summary reads = %d, want exactly 1 per save", profiles.calls)
	}

	in := scorer.lastInput
	if in.ProfileSummary != testProfileSummary {
		t.Errorf("profile summary = %q, want the stored one", in.ProfileSummary)
	}
	if in.JobDescription != "Go, Postgres, AWS." {
		t.Errorf("job description = %q, want the captured one", in.JobDescription)
	}
	// The trimmed company name, not the raw "  Stripe " from the payload.
	if in.CompanyName != "Stripe" {
		t.Errorf("company = %q, want it trimmed", in.CompanyName)
	}
	if in.JobTitle != "Senior Backend Engineer" {
		t.Errorf("job title = %q, want the captured one", in.JobTitle)
	}
}

func TestCreateStoresAScoreWithNoExplanation(t *testing.T) {
	repo, svc := newScoringService(
		t, &fakeScorer{score: &ai.Score{Score: 6.5}},
		&fakeProfiles{summary: testProfileSummary},
	)

	result, err := svc.Create(context.Background(), testUserID, validInput())
	if err != nil {
		t.Fatalf("Create: unexpected error: %v", err)
	}
	if result.Application.AIScore == nil || *result.Application.AIScore != 6.5 {
		t.Errorf("ai score = %v, want 6.5", result.Application.AIScore)
	}
	if stored := repo.apps[result.Application.ID]; stored.AIScoreExplanation != nil {
		t.Errorf("explanation = %q, want NULL rather than an empty string", *stored.AIScoreExplanation)
	}
}

// ---------------------------------------------------------------------------
// Fail-soft: nothing in here may stop an application being saved
// ---------------------------------------------------------------------------

func assertSavedUnscored(t *testing.T, repo *fakeRepo, result *CreateResult, err error) {
	t.Helper()

	if err != nil {
		t.Fatalf("Create: scoring must not fail the save, got error: %v", err)
	}
	if result.Application.AIScore != nil {
		t.Errorf("ai score = %v, want it left unset", *result.Application.AIScore)
	}
	if _, ok := repo.apps[result.Application.ID]; !ok {
		t.Fatal("the application was not saved")
	}
	if len(repo.history[result.Application.ID]) != 1 {
		t.Error("the first status history row was not written")
	}
	if repo.pending(result.Application.ID) == nil {
		t.Error("the follow-up reminder was not scheduled")
	}
}

func TestCreateSucceedsWhenTheModelFails(t *testing.T) {
	scorer := &fakeScorer{err: errors.New("openai returned 429")}
	repo, svc := newScoringService(t, scorer, &fakeProfiles{summary: testProfileSummary})

	result, err := svc.Create(context.Background(), testUserID, validInput())
	assertSavedUnscored(t, repo.fakeRepo, result, err)

	if scorer.calls != 1 {
		t.Errorf("scorer calls = %d, want the failing call to have been made once", scorer.calls)
	}
	if repo.setCalls != 0 {
		t.Errorf("score writes = %d, want none when there is no score", repo.setCalls)
	}
}

func TestCreateSucceedsWhenScoringTimesOut(t *testing.T) {
	scorer := &fakeScorer{score: &ai.Score{Score: 9}, block: 5 * time.Second}
	repo, svc := newScoringService(
		t, scorer, &fakeProfiles{summary: testProfileSummary},
		WithScoreTimeout(20*time.Millisecond),
	)

	start := time.Now()
	result, err := svc.Create(context.Background(), testUserID, validInput())
	assertSavedUnscored(t, repo.fakeRepo, result, err)

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("save took %v, want the score timeout to cut it short", elapsed)
	}
}

func TestCreateSkipsScoringWithoutAProfileSummary(t *testing.T) {
	scorer := scoredOK()
	repo, svc := newScoringService(t, scorer, &fakeProfiles{summary: "   "})

	result, err := svc.Create(context.Background(), testUserID, validInput())
	assertSavedUnscored(t, repo.fakeRepo, result, err)

	// The point of skipping: a job scored against no profile would produce a
	// number that looks just as authoritative as a real one.
	if scorer.calls != 0 {
		t.Errorf("scorer calls = %d, want the model left alone with nothing to compare against", scorer.calls)
	}
}

func TestCreateSkipsScoringWhenTheProfileCannotBeRead(t *testing.T) {
	scorer := scoredOK()
	repo, svc := newScoringService(t, scorer, &fakeProfiles{err: errors.New("connection refused")})

	result, err := svc.Create(context.Background(), testUserID, validInput())
	assertSavedUnscored(t, repo.fakeRepo, result, err)

	if scorer.calls != 0 {
		t.Errorf("scorer calls = %d, want none when the profile could not be read", scorer.calls)
	}
}

func TestCreateSkipsScoringWithoutAJobDescription(t *testing.T) {
	scorer := scoredOK()
	repo, svc := newScoringService(t, scorer, &fakeProfiles{summary: testProfileSummary})

	in := validInput()
	in.JobDescription = "   "

	result, err := svc.Create(context.Background(), testUserID, in)
	assertSavedUnscored(t, repo.fakeRepo, result, err)

	if scorer.calls != 0 {
		t.Errorf("scorer calls = %d, want none when there is nothing to score", scorer.calls)
	}
}

func TestCreateWithoutScoringConfigured(t *testing.T) {
	// No OpenAI key in the environment means cmd/api never passes WithScoring,
	// which is the shape newTestService already has.
	repo, _, svc := newTestService(t)

	result, err := svc.Create(context.Background(), testUserID, validInput())
	assertSavedUnscored(t, repo, result, err)
}

func TestCreateHalfConfiguredScoringStaysOff(t *testing.T) {
	// A scorer with nothing to score against is not scoring, so WithScoring
	// refuses to half-enable itself.
	scorer := scoredOK()
	repo, svc := newScoringService(t, scorer, nil)

	result, err := svc.Create(context.Background(), testUserID, validInput())
	assertSavedUnscored(t, repo.fakeRepo, result, err)

	if scorer.calls != 0 {
		t.Errorf("scorer calls = %d, want scoring to stay off without a profile source", scorer.calls)
	}
}

// ---------------------------------------------------------------------------
// Not fail-soft: a failed database write is a failed save
// ---------------------------------------------------------------------------

func TestCreateFailsWhenTheScoreCannotBeWritten(t *testing.T) {
	repo, svc := newScoringService(t, scoredOK(), &fakeProfiles{summary: testProfileSummary})
	repo.setErr = errors.New("deadlock detected")

	if _, err := svc.Create(context.Background(), testUserID, validInput()); err == nil {
		t.Fatal("Create: a failed write inside the transaction must fail the save")
	}
	// The real repository rolls the whole transaction back here. The fake has no
	// rollback — same convention as service_test.go — so this asserts only that
	// the caller was told.
	if repo.txCalls != 1 {
		t.Errorf("transactions = %d, want 1", repo.txCalls)
	}
}
