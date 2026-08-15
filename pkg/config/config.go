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
	Port            string

	// CVBucket is the S3 bucket the cvs module stores uploaded files in.
	CVBucket string

	// CORSAllowedOrigins is the browser origin allow-list. Per the
	// architecture diagram this is the Vercel dashboard domain plus the
	// chrome-extension:// origin. Comma-separated in the env var.
	//
	// Since phase 14 the API authenticates the dashboard with a cookie, which
	// means CORS runs with credentials allowed, which means "*" is not a legal
	// value here — see parseOrigins. That constraint is enforced at load time
	// rather than left to be discovered as a browser console error.
	CORSAllowedOrigins []string

	// CookieSecure sets the Secure flag on the session cookie.
	//
	// Explicit rather than derived from the runtime. Deriving it from "are we
	// under Lambda" would tie a security property to an unrelated signal, and
	// would be wrong in the dangerous direction the first time somebody runs
	// this behind a local proxy or tests the Lambda over plain http. Defaults
	// to true: the only environment that should turn it off is local http
	// development, and turning it off should be a thing somebody typed.
	CookieSecure bool

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
		Port:            os.Getenv("PORT"),
		CVBucket:        os.Getenv("APPLYMIND_CV_BUCKET"),
		OpenAIAPIKey:    strings.TrimSpace(os.Getenv("OPENAI_API_KEY")),
		OpenAIModel:     strings.TrimSpace(os.Getenv("OPENAI_MODEL")),
	}

	var missing []string
	if cfg.NeonDatabaseURL == "" {
		missing = append(missing, "NEON_DATABASE_URL")
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

	origins, err := parseOrigins(os.Getenv("CORS_ALLOWED_ORIGINS"))
	if err != nil {
		return nil, err
	}
	cfg.CORSAllowedOrigins = origins

	cfg.CookieSecure = parseBool(os.Getenv("APPLYMIND_COOKIE_SECURE"), true)
	cfg.AIScoreTimeout = parseTimeout(os.Getenv("OPENAI_TIMEOUT_SECONDS"), DefaultAIScoreTimeout)

	return cfg, nil
}

// parseOrigins splits a comma-separated origin list, trimming blanks. An unset
// or empty value yields http://localhost:3000 so a fresh checkout can talk to
// a local Next.js dev server without extra configuration.
//
// "*" is rejected outright. The API sends AllowCredentials, and a wildcard with
// credentials is not something a browser will honour — but the failure shows up
// as the dashboard mysteriously failing every authenticated request, which is a
// long way from the env var that caused it. Failing at startup puts the error
// next to the mistake.
func parseOrigins(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return []string{"http://localhost:3000"}, nil
	}

	parts := strings.Split(raw, ",")
	origins := make([]string, 0, len(parts))
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed == "" {
			continue
		}
		if trimmed == "*" {
			return nil, fmt.Errorf(
				"config: CORS_ALLOWED_ORIGINS may not contain \"*\": the API sends " +
					"credentials, so origins must be listed explicitly",
			)
		}
		origins = append(origins, trimmed)
	}
	if len(origins) == 0 {
		return nil, fmt.Errorf("config: CORS_ALLOWED_ORIGINS was set but contained no origins")
	}
	return origins, nil
}

// parseBool reads a boolean env var.
//
// An unset or unparseable value falls back rather than failing startup, in the
// same spirit as parseTimeout. For CookieSecure the fallback is true, so the
// direction a typo sends this is towards the safe answer, not away from it.
func parseBool(raw string, fallback bool) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(trimmed)
	if err != nil {
		return fallback
	}
	return parsed
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
