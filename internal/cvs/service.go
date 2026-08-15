package cvs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Storage is the subset of pkg/storage this module needs. Declared here (rather
// than imported as a concrete type) so the service can be tested without S3.
type Storage interface {
	Upload(ctx context.Context, key string, body []byte, contentType string) error
	PresignedDownloadURL(ctx context.Context, key, downloadFilename string, ttl time.Duration) (string, time.Time, error)
	Delete(ctx context.Context, key string) error
}

// Service is the business logic boundary for the cvs module.
//
// Phase 15: every method takes the caller's userID as its second parameter,
// straight after ctx. The handler reads it from the authenticated request
// context — nothing here trusts a client-supplied value.
type Service interface {
	// Match runs the Flow 3 decision tree. It has no side effects — the
	// extension calls it on every file input change event.
	Match(ctx context.Context, userID uuid.UUID, q MatchQuery) (*MatchResult, error)
	Upload(ctx context.Context, userID uuid.UUID, in UploadInput) (*UploadResult, error)
	ListWithVersions(ctx context.Context, userID uuid.UUID) ([]CV, error)
	ListVersions(ctx context.Context, userID, cvID uuid.UUID) ([]CVVersion, error)
	DownloadURL(ctx context.Context, userID, cvID, versionID uuid.UUID) (*DownloadLink, error)
	ApplicationsUsingVersion(ctx context.Context, userID, versionID uuid.UUID) ([]ApplicationUsage, error)
}

const (
	// DefaultMaxUploadBytes is deliberately below the 6 MB Lambda synchronous
	// payload limit, allowing for base64 expansion of binary bodies at API Gateway.
	DefaultMaxUploadBytes int64 = 4 << 20 // 4 MiB
	// DefaultDownloadTTL is how long a presigned download URL stays valid.
	DefaultDownloadTTL = 15 * time.Minute
)

type service struct {
	repo           Repository
	storage        Storage
	maxUploadBytes int64
	downloadTTL    time.Duration
	newID          func() uuid.UUID // injectable for deterministic tests
}

type ServiceOption func(*service)

func WithMaxUploadBytes(n int64) ServiceOption {
	return func(s *service) { s.maxUploadBytes = n }
}

func WithDownloadTTL(d time.Duration) ServiceOption {
	return func(s *service) { s.downloadTTL = d }
}

func WithIDGenerator(f func() uuid.UUID) ServiceOption {
	return func(s *service) { s.newID = f }
}

func NewService(repo Repository, store Storage, opts ...ServiceOption) Service {
	s := &service{
		repo:           repo,
		storage:        store,
		maxUploadBytes: DefaultMaxUploadBytes,
		downloadTTL:    DefaultDownloadTTL,
		newID:          uuid.New,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Match implements Flow 3 exactly, in priority order:
//
//	Decision 2  hash match                       → Terminal Outcome 1 (matched)
//	Decision 3  no filename match                → Terminal Outcome 4 (unknown)
//	Decision 5  filename match, size unavailable → Terminal Outcome 3 (needs_confirmation)
//	Decision 4  filename + size match            → Terminal Outcome 1 (matched)
//	Decision 4  filename match, size differs     → Terminal Outcome 2 (new_version)
//
// A missing hash (Decision 1 = NO in the browser) skips Decision 2 rather than
// querying for an empty hash. Every lookup is scoped to userID, so this can
// never surface — or silently attach to — another user's CV.
func (s *service) Match(ctx context.Context, userID uuid.UUID, q MatchQuery) (*MatchResult, error) {
	filename := strings.TrimSpace(q.Filename)
	if filename == "" {
		return nil, ErrFilenameEmpty
	}

	// Decision 2 — hash is canonical identity.
	if hash := strings.TrimSpace(q.SHA256Hash); hash != "" {
		version, err := s.repo.FindVersionByHash(ctx, userID, hash)
		if err != nil {
			return nil, err
		}
		if version != nil {
			cv, err := s.repo.GetCV(ctx, userID, version.CVID)
			if err != nil {
				return nil, err
			}
			return &MatchResult{
				Outcome:   OutcomeMatched,
				MatchedBy: MatchedByHash,
				CV:        cv,
				Version:   version,
			}, nil
		}
	}

	// Decision 3 — does any stored file carry this filename?
	latest, err := s.repo.FindLatestVersionByFilename(ctx, userID, filename)
	if err != nil {
		return nil, err
	}
	if latest == nil {
		return &MatchResult{Outcome: OutcomeUnknown}, nil
	}

	cv, err := s.repo.GetCV(ctx, userID, latest.CVID)
	if err != nil {
		return nil, err
	}

	// Decision 5 — filename recognised but size unavailable, so the size check
	// cannot run. Ask the user rather than guessing.
	if q.FileSizeBytes == nil {
		details := &ConfirmationDetails{CVName: cv.Name}
		usage, err := s.repo.LastUsageForCV(ctx, userID, cv.ID)
		if err != nil {
			return nil, err
		}
		if usage != nil {
			details.LastUsedAt = usage.AppliedAt
			company := usage.CompanyName
			details.LastCompany = &company
		}
		return &MatchResult{
			Outcome:      OutcomeNeedsConfirmation,
			CV:           cv,
			Version:      latest,
			Confirmation: details,
		}, nil
	}

	// Decision 4 — same name and same size within this CV group is treated as
	// the same file.
	sized, err := s.repo.FindVersionByCVAndSize(ctx, userID, cv.ID, *q.FileSizeBytes)
	if err != nil {
		return nil, err
	}
	if sized != nil {
		return &MatchResult{
			Outcome:   OutcomeMatched,
			MatchedBy: MatchedByFilenameAndSize,
			CV:        cv,
			Version:   sized,
		}, nil
	}

	// Same name, different size — the user updated the document.
	return &MatchResult{Outcome: OutcomeNewVersion, CV: cv}, nil
}

// Upload stores a file in S3 and records a version row. The hash is computed
// here, server side, so sha256_hash is never a client-supplied value.
//
// When in.CVID is set the version joins that existing CV group (this is how the
// extension acts on OutcomeNewVersion). When it is nil a new CV group is created,
// named after the file.
func (s *service) Upload(ctx context.Context, userID uuid.UUID, in UploadInput) (*UploadResult, error) {
	filename := strings.TrimSpace(in.Filename)
	if filename == "" {
		return nil, ErrFilenameEmpty
	}
	if len(in.Content) == 0 {
		return nil, ErrEmptyFile
	}
	if int64(len(in.Content)) > s.maxUploadBytes {
		return nil, ErrFileTooLarge
	}

	sum := sha256.Sum256(in.Content)
	hash := hex.EncodeToString(sum[:])

	var (
		cv         *CV
		createdNew bool
		err        error
	)
	if in.CVID != nil {
		cv, err = s.repo.GetCV(ctx, userID, *in.CVID)
		if err != nil {
			return nil, err
		}
	} else {
		var name string
		name, err = s.availableCVName(ctx, userID, deriveCVName(filename))
		if err != nil {
			return nil, err
		}
		cv, err = s.repo.CreateCV(ctx, userID, name, in.Tag)
		if err != nil {
			return nil, err
		}
		createdNew = true
	}

	// unique(user_id, cv_id, sha256_hash): these exact bytes may already be
	// recorded under this CV. Re-uploading is a no-op rather than an error.
	if existing, err := s.repo.FindVersionByCVAndHash(ctx, userID, cv.ID, hash); err != nil {
		return nil, err
	} else if existing != nil {
		return &UploadResult{CV: cv, Version: existing, AlreadyExisted: true}, nil
	}

	versionID := s.newID()
	key := s3Key(cv.ID, versionID, filename)

	contentType := in.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if err := s.storage.Upload(ctx, key, in.Content, contentType); err != nil {
		if createdNew {
			_ = s.repo.DeleteCV(ctx, userID, cv.ID)
		}
		return nil, fmt.Errorf("cvs: upload to storage: %w", err)
	}

	version, err := s.repo.CreateVersion(ctx, NewVersion{
		ID:               versionID,
		UserID:           userID,
		CVID:             cv.ID,
		SHA256Hash:       hash,
		FileSizeBytes:    int64(len(in.Content)),
		OriginalFilename: filename,
		S3Key:            key,
	})
	if err != nil {
		// Best effort: do not leave an object in S3 with no row pointing at it.
		_ = s.storage.Delete(ctx, key)
		if createdNew {
			_ = s.repo.DeleteCV(ctx, userID, cv.ID)
		}
		return nil, err
	}

	return &UploadResult{CV: cv, Version: version}, nil
}

func (s *service) ListWithVersions(ctx context.Context, userID uuid.UUID) ([]CV, error) {
	cvs, err := s.repo.ListCVs(ctx, userID)
	if err != nil {
		return nil, err
	}
	if len(cvs) == 0 {
		return []CV{}, nil
	}

	versions, err := s.repo.ListAllVersions(ctx, userID)
	if err != nil {
		return nil, err
	}
	byCV := make(map[uuid.UUID][]CVVersion, len(cvs))
	for _, v := range versions {
		byCV[v.CVID] = append(byCV[v.CVID], v)
	}
	for i := range cvs {
		group := byCV[cvs[i].ID]
		sort.Slice(group, func(a, b int) bool {
			return group[a].UploadedAt.After(group[b].UploadedAt)
		})
		if group == nil {
			group = []CVVersion{}
		}
		cvs[i].Versions = group
	}
	return cvs, nil
}

func (s *service) ListVersions(ctx context.Context, userID, cvID uuid.UUID) ([]CVVersion, error) {
	if _, err := s.repo.GetCV(ctx, userID, cvID); err != nil {
		return nil, err
	}
	return s.repo.ListVersionsForCV(ctx, userID, cvID)
}

func (s *service) DownloadURL(ctx context.Context, userID, cvID, versionID uuid.UUID) (*DownloadLink, error) {
	version, err := s.repo.GetVersion(ctx, userID, versionID)
	if err != nil {
		return nil, err
	}
	// A version is only reachable through its own CV.
	if version.CVID != cvID {
		return nil, ErrVersionNotFound
	}

	url, expiresAt, err := s.storage.PresignedDownloadURL(ctx, version.S3Key, version.OriginalFilename, s.downloadTTL)
	if err != nil {
		return nil, fmt.Errorf("cvs: presign download: %w", err)
	}
	return &DownloadLink{
		URL:       url,
		Filename:  version.OriginalFilename,
		ExpiresAt: expiresAt,
	}, nil
}

func (s *service) ApplicationsUsingVersion(ctx context.Context, userID, versionID uuid.UUID) (
	[]ApplicationUsage,
	error,
) {
	if _, err := s.repo.GetVersion(ctx, userID, versionID); err != nil {
		return nil, err
	}
	return s.repo.ListApplicationsUsingVersion(ctx, userID, versionID)
}

// availableCVName resolves the unique(user_id, name) constraint by suffixing.
func (s *service) availableCVName(ctx context.Context, userID uuid.UUID, base string) (string, error) {
	candidate := base
	for i := 2; i < 100; i++ {
		_, err := s.repo.GetCVByName(ctx, userID, candidate)
		if errors.Is(err, ErrCVNotFound) {
			return candidate, nil
		}
		if err != nil {
			return "", err
		}
		candidate = fmt.Sprintf("%s (%d)", base, i)
	}
	return "", fmt.Errorf("cvs: could not derive a unique name from %q", base)
}

// deriveCVName turns "Senior_Backend_CV_v3.pdf" into "Senior Backend CV v3".
// The user can rename the group later from the dashboard.
func deriveCVName(filename string) string {
	base := path.Base(filename)
	base = strings.TrimSuffix(base, path.Ext(base))
	base = strings.NewReplacer("_", " ", "-", " ").Replace(base)
	base = strings.Join(strings.Fields(base), " ")
	if base == "" {
		return "CV"
	}
	return base
}

var unsafeKeyChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// s3Key namespaces every stored file under its CV and version, so the key is
// unique even when two versions share a filename.
func s3Key(cvID, versionID uuid.UUID, filename string) string {
	safe := unsafeKeyChars.ReplaceAllString(path.Base(filename), "_")
	safe = strings.Trim(safe, "._")
	if safe == "" {
		safe = "cv"
	}
	return fmt.Sprintf("cvs/%s/%s/%s", cvID, versionID, safe)
}
