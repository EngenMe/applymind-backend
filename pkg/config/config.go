// Package config loads and validates application configuration from
// environment variables. It is the single place env vars are read from;
// everything else receives config as a struct, so tests never need to
// set real env vars.
package config

import (
	"fmt"
	"os"
	"strings"
)

// Config holds all runtime configuration for the API and scheduler Lambdas.
type Config struct {
	NeonDatabaseURL string
	APIKey          string
	Port            string

	// CORSAllowedOrigins is the browser origin allow-list. Per the
	// architecture diagram this is the Vercel dashboard domain plus the
	// chrome-extension:// origin. Comma-separated in the env var.
	CORSAllowedOrigins []string
}

// Load reads required environment variables and returns an error listing
// everything missing, rather than failing on the first one — useful when
// setting up a new environment from scratch.
func Load() (*Config, error) {
	cfg := &Config{
		NeonDatabaseURL: os.Getenv("NEON_DATABASE_URL"),
		APIKey:          os.Getenv("APPLYMIND_API_KEY"),
		Port:            os.Getenv("PORT"),
	}

	var missing []string
	if cfg.NeonDatabaseURL == "" {
		missing = append(missing, "NEON_DATABASE_URL")
	}
	if cfg.APIKey == "" {
		missing = append(missing, "APPLYMIND_API_KEY")
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("config: missing required env vars: %v", missing)
	}

	if cfg.Port == "" {
		cfg.Port = "8080" // local default; unused when running under Lambda
	}

	cfg.CORSAllowedOrigins = parseOrigins(os.Getenv("CORS_ALLOWED_ORIGINS"))

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
