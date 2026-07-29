// Package config loads and validates application configuration from
// environment variables. It is the single place env vars are read from;
// everything else receives config as a struct, so tests never need to
// set real env vars.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime configuration for the API and scheduler Lambdas.
type Config struct {
	NeonDatabaseURL string
	APIKey          string
	Port            string

	// CVBucket is the S3 bucket the cvs module stores uploaded files in.
	CVBucket string

	// CORSAllowedOrigins is the browser origin allow-list. Per the
	// architecture diagram this is the Vercel dashboard domain plus the
	// chrome-extension:// origin. Comma-separated in the env var.
	CORSAllowedOrigins []string

	// OpenAIAPIKey enables the AI job-match score. Deliberately optional: with
	// it unset the API runs exactly as it did before, saving applications with
	// ai_score left NULL. That keeps local development and any already-deployed
	// environment working without a key, and matches the fail-soft rule the
	// scoring path follows everywhere else — a missing score is never an error.
	OpenAIAPIKey string
	// OpenAIModel overrides the scoring model. Empty means the ai package's
	// default, gpt-4o-mini.
	OpenAIModel string
	// AIScoreTimeout caps how long a save waits on the model.
	AIScoreTimeout time.Duration
}

// DefaultAIScoreTimeout is used when OPENAI_TIMEOUT_SECONDS is unset or
// unparseable. The flows budget ~1-3 seconds for the call; this is the ceiling,
// not the expectation.
const DefaultAIScoreTimeout = 15 * time.Second

// AIScoringEnabled reports whether an OpenAI key was supplied. cmd/api uses it
// to decide whether to build the client at all.
func (c *Config) AIScoringEnabled() bool {
	return c.OpenAIAPIKey != ""
}

// Load reads required environment variables and returns an error listing
// everything missing, rather than failing on the first one — useful when
// setting up a new environment from scratch.
func Load() (*Config, error) {
	cfg := &Config{
		NeonDatabaseURL: os.Getenv("NEON_DATABASE_URL"),
		APIKey:          os.Getenv("APPLYMIND_API_KEY"),
		Port:            os.Getenv("PORT"),
		CVBucket:        os.Getenv("APPLYMIND_CV_BUCKET"),
		OpenAIAPIKey:    strings.TrimSpace(os.Getenv("OPENAI_API_KEY")),
		OpenAIModel:     strings.TrimSpace(os.Getenv("OPENAI_MODEL")),
	}

	var missing []string
	if cfg.NeonDatabaseURL == "" {
		missing = append(missing, "NEON_DATABASE_URL")
	}
	if cfg.APIKey == "" {
		missing = append(missing, "APPLYMIND_API_KEY")
	}
	if cfg.CVBucket == "" {
		missing = append(missing, "APPLYMIND_CV_BUCKET")
	}

	// OPENAI_API_KEY is not in that list on purpose — see the field comment.

	if len(missing) > 0 {
		return nil, fmt.Errorf("config: missing required env vars: %v", missing)
	}

	if cfg.Port == "" {
		cfg.Port = "8080" // local default; unused when running under Lambda
	}

	cfg.CORSAllowedOrigins = parseOrigins(os.Getenv("CORS_ALLOWED_ORIGINS"))
	cfg.AIScoreTimeout = parseTimeout(os.Getenv("OPENAI_TIMEOUT_SECONDS"), DefaultAIScoreTimeout)

	return cfg, nil
}

// parseOrigins splits a comma-separated origin list, trimming blanks. An unset
// or empty value yields http://localhost:3000 so a fresh checkout can talk to
// a local Next.js dev server without extra configuration.
func parseOrigins(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return []string{"http://localhost:3000"}
	}

	parts := strings.Split(raw, ",")
	origins := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			origins = append(origins, trimmed)
		}
	}
	return origins
}

// parseTimeout reads a whole number of seconds. Anything unset, unparseable or
// not positive falls back to the default rather than failing startup: a typo in
// an optional tuning knob should not take the API down.
func parseTimeout(raw string, fallback time.Duration) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || seconds <= 0 {
		return fallback
	}
	return time.Duration(seconds) * time.Second
}
