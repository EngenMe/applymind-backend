// Package ai holds the OpenAI client behind the GPT-4o-mini job-match score
// (applications.ai_score / ai_score_explanation).
//
// The exported surface is deliberately one method wide. Scorer is what callers
// depend on, so the applications module can be tested with a fake and never
// touches the network; Client is the real implementation.
//
// Every failure mode here is expected to be fail-soft at the call site: a score
// is a nice-to-have on top of a saved application, never a precondition for one.
// Nothing in this package retries, sleeps or falls back to another model — it
// makes one request and reports what happened.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is the OpenAI API root. Overridable so tests can point at
	// an httptest server, and so a proxy can be dropped in later.
	DefaultBaseURL = "https://api.openai.com/v1"
	// DefaultModel is the model the MVP scores with. Named in the phase file and
	// in both flow diagrams.
	DefaultModel = "gpt-4o-mini"
	// DefaultTimeout bounds one scoring call. The flows budget ~1–3 seconds for
	// the round trip; this leaves headroom without holding the Lambda open.
	DefaultTimeout = 15 * time.Second

	// MinScore and MaxScore are the range the prompt asks for and the range
	// applications.ai_score (numeric(4,1)) is documented to hold.
	MinScore = 1.0
	MaxScore = 10.0

	// maxJobDescriptionChars caps what is sent to the model. A LinkedIn job
	// description is normally well under this; the cap exists so a pathological
	// DOM scrape cannot turn one save into a very expensive request.
	maxJobDescriptionChars = 12000
	// maxProfileSummaryChars mirrors settings.MaxProfileSummaryLength.
	maxProfileSummaryChars = 2000
	// maxResponseTokens is generous for a number and two sentences.
	maxResponseTokens = 300
)

// Errors returned by this package. Callers are expected to log them and carry
// on rather than surface them to the user.
var (
	// ErrNoAPIKey is a construction error: NewClient was called without a key.
	ErrNoAPIKey = errors.New("ai: no openai api key configured")
	// ErrNoProfileSummary means there is nothing to score the job against.
	ErrNoProfileSummary = errors.New("ai: profile summary is empty")
	// ErrNoJobDescription means there is nothing to score.
	ErrNoJobDescription = errors.New("ai: job description is empty")
	// ErrNoResponse means the API answered but returned no message content.
	ErrNoResponse = errors.New("ai: model returned no content")
	// ErrUnparsableResponse means the content was not the JSON object the prompt
	// asked for.
	ErrUnparsableResponse = errors.New("ai: model response was not the expected json")
	// ErrScoreOutOfRange means the model invented a score outside 1–10. Treated
	// as a failure rather than clamped: a wrong number stored as fact is worse
	// than no number, and the column is nullable for exactly this reason.
	ErrScoreOutOfRange = errors.New("ai: model returned a score outside 1-10")
)

// APIError is a non-2xx response from OpenAI. It carries the status code so a
// caller can tell a rate limit from a bad key if it ever wants to.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("ai: openai returned %d", e.StatusCode)
	}
	return fmt.Sprintf("ai: openai returned %d: %s", e.StatusCode, e.Message)
}

// ScoreInput is everything the model is shown about one application.
type ScoreInput struct {
	CompanyName    string
	JobTitle       string
	JobDescription string
	// ProfileSummary is the user's 3–4 sentence summary from
	// PUT /settings/profile-summary. Without it there is nothing to compare the
	// job against, so scoring is skipped rather than guessed at.
	ProfileSummary string
}

// Score is one job-match verdict. Score is 1–10 to one decimal place, matching
// applications.ai_score's numeric(4, 1). Explanation is the model's two
// sentences, and may be empty if the model returned only a number.
type Score struct {
	Score       float64
	Explanation string
}

// Scorer is the one behaviour this package provides. Depend on this, not on
// *Client.
type Scorer interface {
	ScoreJob(ctx context.Context, in ScoreInput) (*Score, error)
}

// Client is the OpenAI-backed Scorer.
//
// It talks to /chat/completions directly over net/http rather than through the
// official SDK: one endpoint, one request shape, and no new module dependency.
type Client struct {
	apiKey     string
	baseURL    string
	model      string
	httpClient *http.Client
}

// Option configures a Client at construction.
type Option func(*Client)

// WithBaseURL points the client at a different API root (tests, or a proxy).
func WithBaseURL(u string) Option {
	return func(c *Client) {
		if u != "" {
			c.baseURL = strings.TrimRight(u, "/")
		}
	}
}

// WithModel overrides the model. An empty value leaves DefaultModel in place, so
// an unset env var behaves as "the default" rather than "no model".
func WithModel(m string) Option {
	return func(c *Client) {
		if strings.TrimSpace(m) != "" {
			c.model = strings.TrimSpace(m)
		}
	}
}

// WithTimeout sets the HTTP client timeout. A context deadline supplied by the
// caller still wins if it is shorter.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.httpClient.Timeout = d
		}
	}
}

// WithHTTPClient replaces the whole HTTP client.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.httpClient = h
		}
	}
}

// NewClient builds a Client. It returns ErrNoAPIKey rather than a client that
// fails on every call, so a deployment without a key is a decision made once at
// startup instead of an error logged on every save.
func NewClient(apiKey string, opts ...Option) (*Client, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, ErrNoAPIKey
	}

	c := &Client{
		apiKey:     strings.TrimSpace(apiKey),
		baseURL:    DefaultBaseURL,
		model:      DefaultModel,
		httpClient: &http.Client{Timeout: DefaultTimeout},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// Model reports which model this client scores with. Useful in logs.
func (c *Client) Model() string { return c.model }

// ---------------------------------------------------------------------------
// Wire format
// ---------------------------------------------------------------------------

type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []chatMessage   `json:"messages"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
	Temperature    float64         `json:"temperature"`
	MaxTokens      int             `json:"max_tokens"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responseFormat struct {
	Type string `json:"type"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// scorePayload is the JSON the prompt asks the model to return.
type scorePayload struct {
	Score       float64 `json:"score"`
	Explanation string  `json:"explanation"`
}

const systemPrompt = `You assess how well a candidate matches a job posting.

Reply with a single JSON object and nothing else:
{"score": <number between 1 and 10>, "explanation": "<exactly two sentences>"}

Scoring guide: 1-3 the candidate is a poor fit, 4-6 a partial fit with clear
gaps, 7-8 a strong fit, 9-10 an excellent fit. Judge only on the evidence given;
do not assume skills that are not stated. The explanation must be exactly two
sentences: the first giving the main reason for the score, the second naming the
largest gap or risk. Address the candidate as "you".`

// ScoreJob asks the model how well the user matches one job.
//
// Both the profile summary and the job description are required: with either
// missing there is nothing to compare, and a score produced from half the input
// would look just as authoritative as a real one.
func (c *Client) ScoreJob(ctx context.Context, in ScoreInput) (*Score, error) {
	summary := strings.TrimSpace(in.ProfileSummary)
	if summary == "" {
		return nil, ErrNoProfileSummary
	}
	description := strings.TrimSpace(in.JobDescription)
	if description == "" {
		return nil, ErrNoJobDescription
	}

	body, err := json.Marshal(
		chatRequest{
			Model: c.model,
			Messages: []chatMessage{
				{Role: "system", Content: systemPrompt},
				{Role: "user", Content: userPrompt(in, summary, description)},
			},
			ResponseFormat: &responseFormat{Type: "json_object"},
			// Low but non-zero: the score should be stable across saves of the
			// same job, while leaving the explanation readable.
			Temperature: 0.2,
			MaxTokens:   maxResponseTokens,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("ai: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ai: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ai: call openai: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Bounded read: a completion is small, and this is an external service.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("ai: read response: %w", err)
	}

	var parsed chatResponse
	// A non-2xx body is still worth parsing for its error message, but a body
	// that will not parse at all must not mask the status code.
	unmarshalErr := json.Unmarshal(raw, &parsed)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiErr := &APIError{StatusCode: resp.StatusCode}
		if unmarshalErr == nil && parsed.Error != nil {
			apiErr.Message = parsed.Error.Message
		}
		return nil, apiErr
	}
	if unmarshalErr != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnparsableResponse, unmarshalErr)
	}
	if len(parsed.Choices) == 0 || strings.TrimSpace(parsed.Choices[0].Message.Content) == "" {
		return nil, ErrNoResponse
	}

	return parseScore(parsed.Choices[0].Message.Content)
}

// userPrompt lays the job beside the profile. Both sides are truncated: the
// model only needs the gist, and an unbounded DOM scrape should not decide how
// large the request is.
func userPrompt(in ScoreInput, summary, description string) string {
	var b strings.Builder
	b.WriteString("Candidate profile:\n")
	b.WriteString(truncate(summary, maxProfileSummaryChars))
	b.WriteString("\n\nJob posting:\n")
	if title := strings.TrimSpace(in.JobTitle); title != "" {
		b.WriteString("Title: " + title + "\n")
	}
	if company := strings.TrimSpace(in.CompanyName); company != "" {
		b.WriteString("Company: " + company + "\n")
	}
	b.WriteString("Description:\n")
	b.WriteString(truncate(description, maxJobDescriptionChars))
	return b.String()
}

// parseScore reads the model's JSON object and validates the number.
//
// json_object response format makes a bare object the norm, but a fenced block
// is cheap to survive and costs one TrimPrefix.
func parseScore(content string) (*Score, error) {
	cleaned := stripCodeFence(strings.TrimSpace(content))

	var payload scorePayload
	if err := json.Unmarshal([]byte(cleaned), &payload); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnparsableResponse, err)
	}
	if math.IsNaN(payload.Score) || math.IsInf(payload.Score, 0) {
		return nil, ErrScoreOutOfRange
	}

	// One decimal place, to match numeric(4, 1) — Postgres would round anyway,
	// and rounding here means the value returned to the caller is the value
	// that was stored.
	rounded := math.Round(payload.Score*10) / 10
	if rounded < MinScore || rounded > MaxScore {
		return nil, fmt.Errorf("%w: %v", ErrScoreOutOfRange, payload.Score)
	}

	return &Score{Score: rounded, Explanation: strings.TrimSpace(payload.Explanation)}, nil
}

func stripCodeFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	return strings.TrimSpace(s)
}

// truncate cuts on a rune boundary so a multi-byte character is never split.
func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}
