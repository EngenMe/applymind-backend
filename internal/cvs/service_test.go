package cvs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Hand-written mocks (no third-party mocking dependency)
// ---------------------------------------------------------------------------

type mockRepo struct {
	createCV       func(ctx context.Context, name string, tag *string) (*CV, error)
	getCV          func(ctx context.Context, id uuid.UUID) (*CV, error)
	getCVByName    func(ctx context.Context, name string) (*CV, error)
	listCVs        func(ctx context.Context) ([]CV, error)
	deleteCV       func(ctx context.Context, id uuid.UUID) error
	createVersion  func(ctx context.Context, in NewVersion) (*CVVersion, error)
	getVersion     func(ctx context.Context, id uuid.UUID) (*CVVersion, error)
	listForCV      func(ctx context.Context, cvID uuid.UUID) ([]CVVersion, error)
	listAll        func(ctx context.Context) ([]CVVersion, error)
	byHash         func(ctx context.Context, hash string) (*CVVersion, error)
	byFilename     func(ctx context.Context, filename string) (*CVVersion, error)
	byCVAndSize    func(ctx context.Context, cvID uuid.UUID, size int64) (*CVVersion, error)
	byCVAndHash    func(ctx context.Context, cvID uuid.UUID, hash string) (*CVVersion, error)
	listUsage      func(ctx context.Context, versionID uuid.UUID) ([]ApplicationUsage, error)
	lastUsageForCV func(ctx context.Context, cvID uuid.UUID) (*ApplicationUsage, error)

	deletedCVs []uuid.UUID
}

func (m *mockRepo) CreateCV(ctx context.Context, name string, tag *string) (*CV, error) {
	if m.createCV != nil {
		return m.createCV(ctx, name, tag)
	}
	return &CV{ID: uuid.New(), Name: name, Tag: tag}, nil
}

func (m *mockRepo) GetCV(ctx context.Context, id uuid.UUID) (*CV, error) {
	if m.getCV != nil {
		return m.getCV(ctx, id)
	}
	return &CV{ID: id, Name: "Some CV"}, nil
}

func (m *mockRepo) GetCVByName(ctx context.Context, name string) (*CV, error) {
	if m.getCVByName != nil {
		return m.getCVByName(ctx, name)
	}
	return nil, ErrCVNotFound
}

func (m *mockRepo) ListCVs(ctx context.Context) ([]CV, error) {
	if m.listCVs != nil {
		return m.listCVs(ctx)
	}
	return nil, nil
}

func (m *mockRepo) DeleteCV(ctx context.Context, id uuid.UUID) error {
	m.deletedCVs = append(m.deletedCVs, id)
	if m.deleteCV != nil {
		return m.deleteCV(ctx, id)
	}
	return nil
}

func (m *mockRepo) CreateVersion(ctx context.Context, in NewVersion) (*CVVersion, error) {
	if m.createVersion != nil {
		return m.createVersion(ctx, in)
	}
	return &CVVersion{
		ID:               in.ID,
		CVID:             in.CVID,
		SHA256Hash:       in.SHA256Hash,
		FileSizeBytes:    in.FileSizeBytes,
		OriginalFilename: in.OriginalFilename,
		S3Key:            in.S3Key,
		UploadedAt:       time.Now(),
	}, nil
}

func (m *mockRepo) GetVersion(ctx context.Context, id uuid.UUID) (*CVVersion, error) {
	if m.getVersion != nil {
		return m.getVersion(ctx, id)
	}
	return nil, ErrVersionNotFound
}

func (m *mockRepo) ListVersionsForCV(ctx context.Context, cvID uuid.UUID) ([]CVVersion, error) {
	if m.listForCV != nil {
		return m.listForCV(ctx, cvID)
	}
	return nil, nil
}

func (m *mockRepo) ListAllVersions(ctx context.Context) ([]CVVersion, error) {
	if m.listAll != nil {
		return m.listAll(ctx)
	}
	return nil, nil
}

func (m *mockRepo) FindVersionByHash(ctx context.Context, hash string) (*CVVersion, error) {
	if m.byHash != nil {
		return m.byHash(ctx, hash)
	}
	return nil, nil
}

func (m *mockRepo) FindLatestVersionByFilename(ctx context.Context, filename string) (*CVVersion, error) {
	if m.byFilename != nil {
		return m.byFilename(ctx, filename)
	}
	return nil, nil
}

func (m *mockRepo) FindVersionByCVAndSize(ctx context.Context, cvID uuid.UUID, size int64) (*CVVersion, error) {
	if m.byCVAndSize != nil {
		return m.byCVAndSize(ctx, cvID, size)
	}
	return nil, nil
}

func (m *mockRepo) FindVersionByCVAndHash(ctx context.Context, cvID uuid.UUID, hash string) (*CVVersion, error) {
	if m.byCVAndHash != nil {
		return m.byCVAndHash(ctx, cvID, hash)
	}
	return nil, nil
}

func (m *mockRepo) ListApplicationsUsingVersion(ctx context.Context, versionID uuid.UUID) ([]ApplicationUsage, error) {
	if m.listUsage != nil {
		return m.listUsage(ctx, versionID)
	}
	return nil, nil
}

func (m *mockRepo) LastUsageForCV(ctx context.Context, cvID uuid.UUID) (*ApplicationUsage, error) {
	if m.lastUsageForCV != nil {
		return m.lastUsageForCV(ctx, cvID)
	}
	return nil, nil
}

type mockStorage struct {
	uploadErr  error
	presignErr error

	uploadedKeys []string
	deletedKeys  []string
	lastBody     []byte
}

func (m *mockStorage) Upload(ctx context.Context, key string, body []byte, contentType string) error {
	if m.uploadErr != nil {
		return m.uploadErr
	}
	m.uploadedKeys = append(m.uploadedKeys, key)
	m.lastBody = body
	return nil
}

func (m *mockStorage) PresignedDownloadURL(ctx context.Context, key, filename string, ttl time.Duration) (string, time.Time, error) {
	if m.presignErr != nil {
		return "", time.Time{}, m.presignErr
	}
	return "https://s3.example/" + key, time.Now().Add(ttl), nil
}

func (m *mockStorage) Delete(ctx context.Context, key string) error {
	m.deletedKeys = append(m.deletedKeys, key)
	return nil
}

func ptrInt64(v int64) *int64 { return &v }

// ---------------------------------------------------------------------------
// Flow 3 — one test per branch of the decision tree
// ---------------------------------------------------------------------------

// Decision 2 YES → Terminal Outcome 1.
func TestMatch_HashMatch_ReturnsMatched(t *testing.T) {
	cvID := uuid.New()
	versionID := uuid.New()
	repo := &mockRepo{
		byHash: func(_ context.Context, hash string) (*CVVersion, error) {
			if hash != "abc123" {
				t.Fatalf("expected hash abc123, got %q", hash)
			}
			return &CVVersion{ID: versionID, CVID: cvID}, nil
		},
		byFilename: func(context.Context, string) (*CVVersion, error) {
			t.Fatal("filename lookup must not run once the hash matches")
			return nil, nil
		},
		getCV: func(_ context.Context, id uuid.UUID) (*CV, error) {
			return &CV{ID: id, Name: "Backend CV"}, nil
		},
	}
	svc := NewService(repo, &mockStorage{})

	got, err := svc.Match(context.Background(), MatchQuery{
		SHA256Hash:    "abc123",
		Filename:      "totally_different_name.pdf",
		FileSizeBytes: ptrInt64(999),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Outcome != OutcomeMatched {
		t.Errorf("outcome = %q, want %q", got.Outcome, OutcomeMatched)
	}
	if got.MatchedBy != MatchedByHash {
		t.Errorf("matched_by = %q, want %q", got.MatchedBy, MatchedByHash)
	}
	if got.Version == nil || got.Version.ID != versionID {
		t.Errorf("expected version %s to be returned", versionID)
	}
}

// Decision 1 NO (no hash) → Decision 3 YES → Decision 4 YES → Terminal Outcome 1.
func TestMatch_FilenameAndSizeMatch_ReturnsMatched(t *testing.T) {
	cvID := uuid.New()
	existing := &CVVersion{ID: uuid.New(), CVID: cvID, FileSizeBytes: 51200}
	repo := &mockRepo{
		byHash: func(context.Context, string) (*CVVersion, error) {
			t.Fatal("hash lookup must be skipped when no hash was supplied")
			return nil, nil
		},
		byFilename: func(_ context.Context, filename string) (*CVVersion, error) {
			return &CVVersion{ID: uuid.New(), CVID: cvID, OriginalFilename: filename}, nil
		},
		byCVAndSize: func(_ context.Context, id uuid.UUID, size int64) (*CVVersion, error) {
			if id != cvID || size != 51200 {
				t.Fatalf("decision 4 queried with cv=%s size=%d", id, size)
			}
			return existing, nil
		},
		getCV: func(_ context.Context, id uuid.UUID) (*CV, error) {
			return &CV{ID: id, Name: "Backend CV"}, nil
		},
	}
	svc := NewService(repo, &mockStorage{})

	got, err := svc.Match(context.Background(), MatchQuery{
		Filename:      "backend_cv.pdf",
		FileSizeBytes: ptrInt64(51200),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Outcome != OutcomeMatched {
		t.Errorf("outcome = %q, want %q", got.Outcome, OutcomeMatched)
	}
	if got.MatchedBy != MatchedByFilenameAndSize {
		t.Errorf("matched_by = %q, want %q", got.MatchedBy, MatchedByFilenameAndSize)
	}
	if got.Version == nil || got.Version.ID != existing.ID {
		t.Error("expected the size-matched version to be returned")
	}
}

// Decision 3 YES → Decision 4 NO (size readable, nothing matches) → Terminal Outcome 2.
func TestMatch_SameFilenameDifferentSize_ReturnsNewVersion(t *testing.T) {
	cvID := uuid.New()
	repo := &mockRepo{
		byHash:     func(context.Context, string) (*CVVersion, error) { return nil, nil },
		byFilename: func(_ context.Context, _ string) (*CVVersion, error) { return &CVVersion{CVID: cvID}, nil },
		byCVAndSize: func(context.Context, uuid.UUID, int64) (*CVVersion, error) {
			return nil, nil
		},
		getCV: func(_ context.Context, id uuid.UUID) (*CV, error) {
			return &CV{ID: id, Name: "Backend CV"}, nil
		},
	}
	svc := NewService(repo, &mockStorage{})

	got, err := svc.Match(context.Background(), MatchQuery{
		SHA256Hash:    "nomatch",
		Filename:      "backend_cv.pdf",
		FileSizeBytes: ptrInt64(60000),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Outcome != OutcomeNewVersion {
		t.Errorf("outcome = %q, want %q", got.Outcome, OutcomeNewVersion)
	}
	if got.CV == nil || got.CV.ID != cvID {
		t.Error("new_version must identify which CV group to extend")
	}
	if got.Version != nil {
		t.Error("new_version must not point at an existing version")
	}
}

// Decision 3 YES → size unavailable → Decision 5 → Terminal Outcome 3.
func TestMatch_FilenameMatchWithoutSize_ReturnsNeedsConfirmation(t *testing.T) {
	cvID := uuid.New()
	appliedAt := time.Date(2026, 4, 2, 10, 0, 0, 0, time.UTC)
	repo := &mockRepo{
		byFilename: func(_ context.Context, _ string) (*CVVersion, error) { return &CVVersion{CVID: cvID}, nil },
		getCV: func(_ context.Context, id uuid.UUID) (*CV, error) {
			return &CV{ID: id, Name: "Backend CV"}, nil
		},
		byCVAndSize: func(context.Context, uuid.UUID, int64) (*CVVersion, error) {
			t.Fatal("decision 4 cannot run without a file size")
			return nil, nil
		},
		lastUsageForCV: func(context.Context, uuid.UUID) (*ApplicationUsage, error) {
			return &ApplicationUsage{CompanyName: "Stripe", AppliedAt: &appliedAt}, nil
		},
	}
	svc := NewService(repo, &mockStorage{})

	got, err := svc.Match(context.Background(), MatchQuery{Filename: "backend_cv.pdf"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Outcome != OutcomeNeedsConfirmation {
		t.Fatalf("outcome = %q, want %q", got.Outcome, OutcomeNeedsConfirmation)
	}
	if got.Confirmation == nil {
		t.Fatal("confirmation details are required to render the sidebar prompt")
	}
	if got.Confirmation.CVName != "Backend CV" {
		t.Errorf("cv name = %q", got.Confirmation.CVName)
	}
	if got.Confirmation.LastCompany == nil || *got.Confirmation.LastCompany != "Stripe" {
		t.Error("expected last company Stripe")
	}
	if got.Confirmation.LastUsedAt == nil || !got.Confirmation.LastUsedAt.Equal(appliedAt) {
		t.Error("expected last used date to be populated")
	}
}

func TestMatch_NeedsConfirmationWithNeverUsedCV_HasNilUsage(t *testing.T) {
	repo := &mockRepo{
		byFilename: func(context.Context, string) (*CVVersion, error) {
			return &CVVersion{CVID: uuid.New()}, nil
		},
		lastUsageForCV: func(context.Context, uuid.UUID) (*ApplicationUsage, error) { return nil, nil },
	}
	svc := NewService(repo, &mockStorage{})

	got, err := svc.Match(context.Background(), MatchQuery{Filename: "never_sent.pdf"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Outcome != OutcomeNeedsConfirmation {
		t.Fatalf("outcome = %q", got.Outcome)
	}
	if got.Confirmation.LastUsedAt != nil || got.Confirmation.LastCompany != nil {
		t.Error("a CV never attached to an application must report nil usage")
	}
}

// Decision 3 NO → Terminal Outcome 4.
func TestMatch_NoMatchAtAll_ReturnsUnknown(t *testing.T) {
	repo := &mockRepo{
		byHash:     func(context.Context, string) (*CVVersion, error) { return nil, nil },
		byFilename: func(context.Context, string) (*CVVersion, error) { return nil, nil },
	}
	svc := NewService(repo, &mockStorage{})

	got, err := svc.Match(context.Background(), MatchQuery{
		SHA256Hash:    "deadbeef",
		Filename:      "brand_new.pdf",
		FileSizeBytes: ptrInt64(1234),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Outcome != OutcomeUnknown {
		t.Errorf("outcome = %q, want %q", got.Outcome, OutcomeUnknown)
	}
	if got.CV != nil || got.Version != nil {
		t.Error("unknown must not identify a CV or version")
	}
}

func TestMatch_EmptyFilename_IsRejected(t *testing.T) {
	svc := NewService(&mockRepo{}, &mockStorage{})
	if _, err := svc.Match(context.Background(), MatchQuery{Filename: "   "}); !errors.Is(err, ErrFilenameEmpty) {
		t.Errorf("err = %v, want ErrFilenameEmpty", err)
	}
}

func TestMatch_RepositoryFailurePropagates(t *testing.T) {
	boom := errors.New("connection reset")
	repo := &mockRepo{
		byHash: func(context.Context, string) (*CVVersion, error) { return nil, boom },
	}
	svc := NewService(repo, &mockStorage{})
	if _, err := svc.Match(context.Background(), MatchQuery{SHA256Hash: "x", Filename: "a.pdf"}); !errors.Is(err, boom) {
		t.Errorf("err = %v, want the repository error", err)
	}
}

// ---------------------------------------------------------------------------
// Upload
// ---------------------------------------------------------------------------

func TestUpload_NewCV_ComputesHashAndStoresFile(t *testing.T) {
	content := []byte("%PDF-1.7 pretend cv")
	want := sha256.Sum256(content)
	wantHash := hex.EncodeToString(want[:])

	var created NewVersion
	repo := &mockRepo{
		createCV: func(_ context.Context, name string, _ *string) (*CV, error) {
			if name != "backend cv v3" {
				t.Errorf("derived cv name = %q, want %q", name, "backend cv v3")
			}
			return &CV{ID: uuid.New(), Name: name}, nil
		},
		createVersion: func(_ context.Context, in NewVersion) (*CVVersion, error) {
			created = in
			return &CVVersion{ID: in.ID, CVID: in.CVID, SHA256Hash: in.SHA256Hash, S3Key: in.S3Key}, nil
		},
	}
	store := &mockStorage{}
	svc := NewService(repo, store)

	got, err := svc.Upload(context.Background(), UploadInput{
		Filename:    "backend_cv_v3.pdf",
		Content:     content,
		ContentType: "application/pdf",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if created.SHA256Hash != wantHash {
		t.Errorf("hash = %q, want %q", created.SHA256Hash, wantHash)
	}
	if created.FileSizeBytes != int64(len(content)) {
		t.Errorf("size = %d, want %d", created.FileSizeBytes, len(content))
	}
	if created.OriginalFilename != "backend_cv_v3.pdf" {
		t.Errorf("original filename = %q", created.OriginalFilename)
	}
	if len(store.uploadedKeys) != 1 || store.uploadedKeys[0] != created.S3Key {
		t.Fatalf("expected the file to be stored under %q, got %v", created.S3Key, store.uploadedKeys)
	}
	if !strings.HasPrefix(created.S3Key, "cvs/"+got.CV.ID.String()+"/") {
		t.Errorf("s3 key %q is not namespaced under its CV", created.S3Key)
	}
	if got.AlreadyExisted {
		t.Error("a first upload must not report already_existed")
	}
}

func TestUpload_ExistingCV_DoesNotCreateAnotherGroup(t *testing.T) {
	cvID := uuid.New()
	repo := &mockRepo{
		createCV: func(context.Context, string, *string) (*CV, error) {
			t.Fatal("must not create a CV group when cv_id was supplied")
			return nil, nil
		},
		getCV: func(_ context.Context, id uuid.UUID) (*CV, error) { return &CV{ID: id, Name: "Backend CV"}, nil },
	}
	svc := NewService(repo, &mockStorage{})

	got, err := svc.Upload(context.Background(), UploadInput{
		Filename: "backend_cv.pdf",
		Content:  []byte("bytes"),
		CVID:     &cvID,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Version.CVID != cvID {
		t.Errorf("version attached to %s, want %s", got.Version.CVID, cvID)
	}
}

func TestUpload_IdenticalBytesUnderSameCV_IsIdempotent(t *testing.T) {
	cvID := uuid.New()
	existing := &CVVersion{ID: uuid.New(), CVID: cvID}
	repo := &mockRepo{
		getCV:       func(_ context.Context, id uuid.UUID) (*CV, error) { return &CV{ID: id}, nil },
		byCVAndHash: func(context.Context, uuid.UUID, string) (*CVVersion, error) { return existing, nil },
		createVersion: func(context.Context, NewVersion) (*CVVersion, error) {
			t.Fatal("must not insert a duplicate row — unique(cv_id, sha256_hash)")
			return nil, nil
		},
	}
	store := &mockStorage{}
	svc := NewService(repo, store)

	got, err := svc.Upload(context.Background(), UploadInput{
		Filename: "backend_cv.pdf",
		Content:  []byte("same bytes"),
		CVID:     &cvID,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.AlreadyExisted || got.Version.ID != existing.ID {
		t.Error("expected the existing version to be returned unchanged")
	}
	if len(store.uploadedKeys) != 0 {
		t.Error("nothing should have been written to S3")
	}
}

func TestUpload_DatabaseFailure_RollsBackS3AndCVGroup(t *testing.T) {
	boom := errors.New("insert failed")
	cvID := uuid.New()
	repo := &mockRepo{
		createCV:      func(_ context.Context, name string, _ *string) (*CV, error) { return &CV{ID: cvID, Name: name}, nil },
		createVersion: func(context.Context, NewVersion) (*CVVersion, error) { return nil, boom },
	}
	store := &mockStorage{}
	svc := NewService(repo, store)

	if _, err := svc.Upload(context.Background(), UploadInput{
		Filename: "backend_cv.pdf",
		Content:  []byte("bytes"),
	}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the repository error", err)
	}
	if len(store.deletedKeys) != 1 {
		t.Error("the orphaned S3 object should have been deleted")
	}
	if len(repo.deletedCVs) != 1 || repo.deletedCVs[0] != cvID {
		t.Error("the empty CV group created for this upload should have been removed")
	}
}

func TestUpload_Validation(t *testing.T) {
	svc := NewService(&mockRepo{}, &mockStorage{}, WithMaxUploadBytes(8))

	cases := []struct {
		name string
		in   UploadInput
		want error
	}{
		{"no filename", UploadInput{Content: []byte("x")}, ErrFilenameEmpty},
		{"no content", UploadInput{Filename: "cv.pdf"}, ErrEmptyFile},
		{"too large", UploadInput{Filename: "cv.pdf", Content: []byte("123456789")}, ErrFileTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.Upload(context.Background(), tc.in); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestDeriveCVName(t *testing.T) {
	cases := map[string]string{
		"backend_cv_v3.pdf":    "Backend cv v3",
		"Senior-Go-Dev CV.pdf": "Senior Go Dev CV",
		".pdf":                 "CV",
	}
	for in, want := range cases {
		if got := deriveCVName(in); !strings.EqualFold(got, want) {
			t.Errorf("deriveCVName(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Download and listing
// ---------------------------------------------------------------------------

func TestDownloadURL_VersionBelongingToAnotherCV_IsNotFound(t *testing.T) {
	versionID := uuid.New()
	repo := &mockRepo{
		getVersion: func(_ context.Context, id uuid.UUID) (*CVVersion, error) {
			return &CVVersion{ID: id, CVID: uuid.New(), S3Key: "cvs/x/y/cv.pdf"}, nil
		},
	}
	svc := NewService(repo, &mockStorage{})

	if _, err := svc.DownloadURL(context.Background(), uuid.New(), versionID); !errors.Is(err, ErrVersionNotFound) {
		t.Errorf("err = %v, want ErrVersionNotFound", err)
	}
}

func TestDownloadURL_ReturnsPresignedLink(t *testing.T) {
	cvID := uuid.New()
	repo := &mockRepo{
		getVersion: func(_ context.Context, id uuid.UUID) (*CVVersion, error) {
			return &CVVersion{ID: id, CVID: cvID, S3Key: "cvs/a/b/cv.pdf", OriginalFilename: "cv.pdf"}, nil
		},
	}
	svc := NewService(repo, &mockStorage{}, WithDownloadTTL(5*time.Minute))

	link, err := svc.DownloadURL(context.Background(), cvID, uuid.New())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if link.URL != "https://s3.example/cvs/a/b/cv.pdf" {
		t.Errorf("url = %q", link.URL)
	}
	if link.Filename != "cv.pdf" {
		t.Errorf("filename = %q", link.Filename)
	}
	if link.ExpiresAt.Before(time.Now()) {
		t.Error("expiry must be in the future")
	}
}

func TestListWithVersions_GroupsVersionsByCVNewestFirst(t *testing.T) {
	cvA, cvB := uuid.New(), uuid.New()
	older := time.Now().Add(-48 * time.Hour)
	newer := time.Now().Add(-1 * time.Hour)
	repo := &mockRepo{
		listCVs: func(context.Context) ([]CV, error) {
			return []CV{{ID: cvA, Name: "A"}, {ID: cvB, Name: "B"}}, nil
		},
		listAll: func(context.Context) ([]CVVersion, error) {
			return []CVVersion{
				{ID: uuid.New(), CVID: cvA, UploadedAt: older},
				{ID: uuid.New(), CVID: cvA, UploadedAt: newer},
			}, nil
		},
	}
	svc := NewService(repo, &mockStorage{})

	got, err := svc.ListWithVersions(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got[0].Versions) != 2 {
		t.Fatalf("cv A has %d versions, want 2", len(got[0].Versions))
	}
	if !got[0].Versions[0].UploadedAt.Equal(newer) {
		t.Error("versions must be newest first")
	}
	if got[1].Versions == nil || len(got[1].Versions) != 0 {
		t.Error("a CV with no versions must serialise as an empty list, not null")
	}
}
