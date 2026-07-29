package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// capture records what the fake OpenAI server was sent. It is filled in during
// the request, so tests must read it after calling ScoreJob.
type capture struct {
	req  *http.Request
	body []byte
}

// completionServer stands in for OpenAI. handler decides what one
// /chat/completions call answers with; the captured request is available to the
// test afterwards.
func completionServer(t *testing.T, handler http.HandlerFunc) (*Client, *capture) {
	t.Helper()

	got := &capture{}

	srv := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				got.req = r
				got.body = body
				handler(w, r)
			},
		),
	)
	t.Cleanup(srv.Close)

	client, err := NewClient("sk-test", WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client, got
}

func respondWithContent(content string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(
			map[string]any{
				"choices": []map[string]any{
					{"message": map[string]string{"role": "assistant", "content": content}},
				},
			},
		)
	}
}

func validScoreInput() ScoreInput {
	return ScoreInput{
		CompanyName:    "Stripe",
		JobTitle:       "Senior Backend Engineer",
		JobDescription: "Go, Postgres, AWS Lambda. Five years of backend experience.",
		ProfileSummary: "Backend engineer with six years of Go and Postgres experience.",
	}
}

// ---------------------------------------------------------------------------
// Construction
// ---------------------------------------------------------------------------

func TestNewClientRequiresAPIKey(t *testing.T) {
	if _, err := NewClient("   "); !errors.Is(err, ErrNoAPIKey) {
		t.Errorf("error = %v, want %v", err, ErrNoAPIKey)
	}
}

func TestNewClientDefaultsAndOverrides(t *testing.T) {
	c, err := NewClient("sk-test")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.Model() != DefaultModel {
		t.Errorf("model = %q, want %q", c.Model(), DefaultModel)
	}
	if c.httpClient.Timeout != DefaultTimeout {
		t.Errorf("timeout = %v, want %v", c.httpClient.Timeout, DefaultTimeout)
	}

	// An unset env var arrives as "" and must not blank out the default.
	c, err = NewClient("sk-test", WithModel("  "), WithTimeout(0), WithModel("gpt-4o"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.Model() != "gpt-4o" {
		t.Errorf("model = %q, want the explicit override", c.Model())
	}
	if c.httpClient.Timeout != DefaultTimeout {
		t.Errorf("timeout = %v, want a zero timeout to leave the default alone", c.httpClient.Timeout)
	}
}

// ---------------------------------------------------------------------------
// Successful scoring
// ---------------------------------------------------------------------------

func TestScoreJobSuccess(t *testing.T) {
	client, _ := completionServer(
		t, respondWithContent(`{"score": 8, "explanation": "You match well. Little Kubernetes experience."}`),
	)

	score, err := client.ScoreJob(context.Background(), validScoreInput())
	if err != nil {
		t.Fatalf("ScoreJob: unexpected error: %v", err)
	}
	if score.Score != 8 {
		t.Errorf("score = %v, want 8", score.Score)
	}
	if !strings.HasPrefix(score.Explanation, "You match well.") {
		t.Errorf("explanation = %q, want the model's two sentences", score.Explanation)
	}
}

func TestScoreJobSendsExpectedRequest(t *testing.T) {
	client, got := completionServer(t, respondWithContent(`{"score": 7.5, "explanation": "Fine. Fine."}`))

	if _, err := client.ScoreJob(context.Background(), validScoreInput()); err != nil {
		t.Fatalf("ScoreJob: unexpected error: %v", err)
	}

	if got.req.URL.Path != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", got.req.URL.Path)
	}
	if auth := got.req.Header.Get("Authorization"); auth != "Bearer sk-test" {
		t.Errorf("authorization = %q, want the bearer key", auth)
	}

	var sent chatRequest
	if err := json.Unmarshal(got.body, &sent); err != nil {
		t.Fatalf("request body did not parse: %v", err)
	}
	if sent.Model != DefaultModel {
		t.Errorf("model = %q, want %q", sent.Model, DefaultModel)
	}
	if sent.ResponseFormat == nil || sent.ResponseFormat.Type != "json_object" {
		t.Errorf("response format = %+v, want json_object", sent.ResponseFormat)
	}
	if len(sent.Messages) != 2 {
		t.Fatalf("messages = %d, want a system and a user message", len(sent.Messages))
	}
	user := sent.Messages[1].Content
	for _, want := range []string{"Stripe", "Senior Backend Engineer", "six years of Go"} {
		if !strings.Contains(user, want) {
			t.Errorf("user prompt is missing %q:\n%s", want, user)
		}
	}
}

func TestScoreJobRoundsToOneDecimal(t *testing.T) {
	client, _ := completionServer(t, respondWithContent(`{"score": 7.46, "explanation": "Close. Enough."}`))

	score, err := client.ScoreJob(context.Background(), validScoreInput())
	if err != nil {
		t.Fatalf("ScoreJob: unexpected error: %v", err)
	}
	// numeric(4, 1) would round on the way in; rounding here means the stored
	// value and the returned value agree.
	if score.Score != 7.5 {
		t.Errorf("score = %v, want it rounded to 7.5", score.Score)
	}
}

func TestScoreJobSurvivesFencedJSON(t *testing.T) {
	client, _ := completionServer(
		t, respondWithContent("```json\n{\"score\": 6, \"explanation\": \"Partial. Gaps.\"}\n```"),
	)

	score, err := client.ScoreJob(context.Background(), validScoreInput())
	if err != nil {
		t.Fatalf("ScoreJob: unexpected error: %v", err)
	}
	if score.Score != 6 {
		t.Errorf("score = %v, want 6", score.Score)
	}
}

func TestScoreJobAcceptsMissingExplanation(t *testing.T) {
	client, _ := completionServer(t, respondWithContent(`{"score": 9}`))

	score, err := client.ScoreJob(context.Background(), validScoreInput())
	if err != nil {
		t.Fatalf("ScoreJob: unexpected error: %v", err)
	}
	if score.Score != 9 || score.Explanation != "" {
		t.Errorf("score = %+v, want the number kept and an empty explanation", score)
	}
}

func TestScoreJobTruncatesLongJobDescription(t *testing.T) {
	client, got := completionServer(t, respondWithContent(`{"score": 5, "explanation": "Ok. Ok."}`))

	// A multi-byte marker that appears nowhere else in the prompt: it makes the
	// count unambiguous and proves the cut lands on a rune boundary.
	in := validScoreInput()
	in.JobDescription = strings.Repeat("\u03a9", maxJobDescriptionChars+5000)

	if _, err := client.ScoreJob(context.Background(), in); err != nil {
		t.Fatalf("ScoreJob: unexpected error: %v", err)
	}

	var sent chatRequest
	if err := json.Unmarshal(got.body, &sent); err != nil {
		t.Fatalf("request body did not parse: %v", err)
	}
	user := sent.Messages[1].Content
	if n := strings.Count(user, "\u03a9"); n != maxJobDescriptionChars {
		t.Errorf("job description characters sent = %d, want exactly %d", n, maxJobDescriptionChars)
	}
	if !strings.Contains(user, "\u2026") {
		t.Error("truncated text should be marked with an ellipsis")
	}
	if !utf8.ValidString(user) {
		t.Error("truncation split a multi-byte character")
	}
}

// ---------------------------------------------------------------------------
// Failure modes — every one of these is fail-soft at the call site
// ---------------------------------------------------------------------------

func TestScoreJobRequiresBothSides(t *testing.T) {
	client, err := NewClient("sk-test", WithBaseURL("http://127.0.0.1:1"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	in := validScoreInput()
	in.ProfileSummary = "   "
	if _, err := client.ScoreJob(context.Background(), in); !errors.Is(err, ErrNoProfileSummary) {
		t.Errorf("error = %v, want %v", err, ErrNoProfileSummary)
	}

	in = validScoreInput()
	in.JobDescription = ""
	if _, err := client.ScoreJob(context.Background(), in); !errors.Is(err, ErrNoJobDescription) {
		t.Errorf("error = %v, want %v", err, ErrNoJobDescription)
	}
}

func TestScoreJobAPIError(t *testing.T) {
	client, _ := completionServer(
		t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error": {"message": "rate limit reached", "type": "rate_limit_error"}}`))
		},
	)

	_, err := client.ScoreJob(context.Background(), validScoreInput())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want an *APIError", err)
	}
	if apiErr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", apiErr.StatusCode)
	}
	if !strings.Contains(apiErr.Error(), "rate limit reached") {
		t.Errorf("error = %q, want the api message included", apiErr.Error())
	}
}

func TestScoreJobNonJSONErrorBodyStillReportsStatus(t *testing.T) {
	client, _ := completionServer(
		t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html>gateway</html>"))
		},
	)

	_, err := client.ScoreJob(context.Background(), validScoreInput())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadGateway {
		t.Errorf("error = %v, want an *APIError carrying 502", err)
	}
}

func TestScoreJobEmptyChoices(t *testing.T) {
	client, _ := completionServer(
		t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"choices": []}`))
		},
	)

	if _, err := client.ScoreJob(context.Background(), validScoreInput()); !errors.Is(err, ErrNoResponse) {
		t.Errorf("error = %v, want %v", err, ErrNoResponse)
	}
}

func TestScoreJobUnparsableContent(t *testing.T) {
	client, _ := completionServer(t, respondWithContent("I would say about an 8 out of 10."))

	if _, err := client.ScoreJob(context.Background(), validScoreInput()); !errors.Is(err, ErrUnparsableResponse) {
		t.Errorf("error = %v, want %v", err, ErrUnparsableResponse)
	}
}

func TestScoreJobRejectsOutOfRangeScore(t *testing.T) {
	for _, content := range []string{
		`{"score": 0, "explanation": "No. No."}`,
		`{"score": 11, "explanation": "Yes. Yes."}`,
		`{"score": -3, "explanation": "No. No."}`,
	} {
		client, _ := completionServer(t, respondWithContent(content))

		if _, err := client.ScoreJob(context.Background(), validScoreInput()); !errors.Is(err, ErrScoreOutOfRange) {
			t.Errorf("content %s: error = %v, want %v", content, err, ErrScoreOutOfRange)
		}
	}
}

func TestScoreJobRespectsContextCancellation(t *testing.T) {
	client, _ := completionServer(
		t, func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
			_, _ = w.Write([]byte(`{"choices": []}`))
		},
	)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := client.ScoreJob(ctx, validScoreInput()); err == nil {
		t.Fatal("ScoreJob: want an error when the context deadline passes")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("returned after %v, want it to give up with the context", elapsed)
	}
}
