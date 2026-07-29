package settings

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"
)

// Service is the business logic boundary for the settings module.
type Service interface {
	// Get reads the current settings. A missing row is reported as empty
	// settings rather than an error: from the user's side "I have not set a
	// profile summary yet" and "the row has not been created yet" are the same
	// situation, and the first write creates it.
	Get(ctx context.Context) (*Settings, error)
	// SetProfileSummary validates and stores the summary, trimmed.
	SetProfileSummary(ctx context.Context, summary string) (*Settings, error)
	// ProfileSummary returns just the summary, or "" when none is set. This is
	// the method the applications module depends on for AI scoring — it takes
	// the narrowest interface it can, so it never sees the rest of settings.
	ProfileSummary(ctx context.Context) (string, error)
}

type service struct {
	repo Repository
}

func NewService(repo Repository) Service {
	return &service{repo: repo}
}

func (s *service) Get(ctx context.Context) (*Settings, error) {
	current, err := s.repo.Get(ctx)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return &Settings{}, nil
		}
		return nil, err
	}
	return current, nil
}

func (s *service) SetProfileSummary(ctx context.Context, summary string) (*Settings, error) {
	trimmed := strings.TrimSpace(summary)
	if trimmed == "" {
		return nil, ErrSummaryRequired
	}
	// Counted in runes, not bytes: an accented or non-Latin summary should get
	// the same allowance as an ASCII one.
	if utf8.RuneCountInString(trimmed) > MaxProfileSummaryLength {
		return nil, ErrSummaryTooLong
	}

	return s.repo.SetProfileSummary(ctx, trimmed)
}

func (s *service) ProfileSummary(ctx context.Context) (string, error) {
	current, err := s.Get(ctx)
	if err != nil {
		return "", err
	}
	return current.Summary(), nil
}
