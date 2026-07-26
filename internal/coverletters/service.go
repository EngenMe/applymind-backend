package coverletters

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
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

// Service is the business logic boundary for the coverletters module.
//
// There is no matching, hashing or version selection here: a cover letter
// belongs to exactly one application, so the application id is always the whole
// question and saving is always a replace.
type Service interface {
	// SaveText stores a typed cover letter, replacing any existing one.
	SaveText(ctx context.Context, in SaveTextInput) (*CoverLetter, error)
	// SaveFile validates the extension, stores the file in S3 and records the
	// row, replacing any existing cover letter.
	SaveFile(ctx context.Context, in SaveFileInput) (*CoverLetter, error)
	// EditText rewrites the body of a text cover letter in place. It returns
	// ErrNotTextKind for a file cover letter.
	EditText(ctx context.Context, applicationID uuid.UUID, body string) (*CoverLetter, error)
	// Get returns the cover letter for an application, whichever kind it is.
	Get(ctx context.Context, applicationID uuid.UUID) (*CoverLetter, error)
	// DownloadURL presigns the stored object. It returns ErrNotFileKind for a
	// text cover letter, which has nothing stored.
	DownloadURL(ctx context.Context, applicationID uuid.UUID) (*DownloadLink, error)
}

const (
	// DefaultMaxUploadBytes is deliberately below the 6 MB Lambda synchronous
	// payload limit, allowing for base64 expansion of binary bodies at API Gateway.
	DefaultMaxUploadBytes int64 = 4 << 20 // 4 MiB
	// DefaultDownloadTTL is how long a presigned download URL stays valid.
	DefaultDownloadTTL = 15 * time.Minute
)

// allowedExtensions is the accepted-format allow-list from the phase spec, and
// doubles as the content type to store when the browser did not send one.
// Anything not in this map is rejected before a byte reaches S3.
var allowedExtensions = map[string]string{
	".pdf":  "application/pdf",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".doc":  "application/msword",
}

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

func (s *service) SaveText(ctx context.Context, in SaveTextInput) (*CoverLetter, error) {
	body := strings.TrimSpace(in.BodyText)
	if body == "" {
		return nil, ErrEmptyBody
	}

	if err := s.clearExisting(ctx, in.ApplicationID); err != nil {
		return nil, err
	}

	return s.repo.Create(
		ctx, NewCoverLetter{
			ID:            s.newID(),
			ApplicationID: in.ApplicationID,
			Kind:          KindText,
			BodyText:      &body,
		},
	)
}

func (s *service) SaveFile(ctx context.Context, in SaveFileInput) (*CoverLetter, error) {
	filename := strings.TrimSpace(in.Filename)
	if filename == "" {
		return nil, ErrFilenameEmpty
	}

	// Extension is the gate, not the browser-supplied content type — the latter
	// is trivially spoofed and often just application/octet-stream.
	contentType, ok := allowedExtensions[strings.ToLower(path.Ext(filename))]
	if !ok {
		return nil, ErrUnsupportedFileType
	}
	if in.ContentType != "" {
		contentType = in.ContentType
	}

	if len(in.Content) == 0 {
		return nil, ErrEmptyFile
	}
	if int64(len(in.Content)) > s.maxUploadBytes {
		return nil, ErrFileTooLarge
	}

	if err := s.clearExisting(ctx, in.ApplicationID); err != nil {
		return nil, err
	}

	id := s.newID()
	key := s3Key(in.ApplicationID, id, filename)
	if err := s.storage.Upload(ctx, key, in.Content, contentType); err != nil {
		return nil, fmt.Errorf("coverletters: upload to storage: %w", err)
	}

	cl, err := s.repo.Create(
		ctx, NewCoverLetter{
			ID:               id,
			ApplicationID:    in.ApplicationID,
			Kind:             KindFile,
			S3Key:            &key,
			OriginalFilename: &filename,
		},
	)
	if err != nil {
		// Best effort: do not leave an object in S3 with no row pointing at it.
		// This is also the path taken when application_id does not exist.
		_ = s.storage.Delete(ctx, key)
		return nil, err
	}
	return cl, nil
}

func (s *service) EditText(ctx context.Context, applicationID uuid.UUID, body string) (*CoverLetter, error) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return nil, ErrEmptyBody
	}

	existing, err := s.repo.GetByApplicationID(ctx, applicationID)
	if err != nil {
		return nil, err
	}
	// A file cover letter is a record of the bytes that were actually sent to a
	// company. Editing it would make the record a lie, so it is immutable —
	// changing it means saving a new cover letter over it.
	if existing.Kind != KindText {
		return nil, ErrNotTextKind
	}

	return s.repo.UpdateTextBody(ctx, applicationID, trimmed)
}

func (s *service) Get(ctx context.Context, applicationID uuid.UUID) (*CoverLetter, error) {
	return s.repo.GetByApplicationID(ctx, applicationID)
}

func (s *service) DownloadURL(ctx context.Context, applicationID uuid.UUID) (*DownloadLink, error) {
	cl, err := s.repo.GetByApplicationID(ctx, applicationID)
	if err != nil {
		return nil, err
	}
	if cl.Kind != KindFile || cl.S3Key == nil {
		return nil, ErrNotFileKind
	}

	url, expiresAt, err := s.storage.PresignedDownloadURL(ctx, *cl.S3Key, cl.Filename(), s.downloadTTL)
	if err != nil {
		return nil, fmt.Errorf("coverletters: presign download: %w", err)
	}
	return &DownloadLink{
		URL:       url,
		Filename:  cl.Filename(),
		ExpiresAt: expiresAt,
	}, nil
}

// clearExisting removes whatever cover letter the application already has, so
// that a save is a replace. unique(application_id) means an insert could not
// succeed otherwise, and this module deliberately keeps no history.
func (s *service) clearExisting(ctx context.Context, applicationID uuid.UUID) error {
	existing, err := s.repo.GetByApplicationID(ctx, applicationID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}

	if err := s.repo.DeleteByApplicationID(ctx, applicationID); err != nil {
		return err
	}
	// Best effort, and only after the row is gone: a failed delete here leaves
	// an unreferenced object, which is cheaper than a row pointing at nothing.
	if existing.Kind == KindFile && existing.S3Key != nil {
		_ = s.storage.Delete(ctx, *existing.S3Key)
	}
	return nil
}

var unsafeKeyChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// s3Key namespaces every stored file under its application and cover letter id,
// so a replacement never collides with the object it is replacing. The
// cover-letters/ prefix matches the bucket layout in the architecture diagram.
func s3Key(applicationID, coverLetterID uuid.UUID, filename string) string {
	safe := unsafeKeyChars.ReplaceAllString(path.Base(filename), "_")
	safe = strings.Trim(safe, "._")
	if safe == "" {
		safe = "cover-letter"
	}
	return fmt.Sprintf("cover-letters/%s/%s/%s", applicationID, coverLetterID, safe)
}
