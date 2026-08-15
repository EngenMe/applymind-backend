package coverletters

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// testUserID is declared in handler_test.go — Go test files in a package share
// one namespace, so it is declared once there and used here too.

// ---------------------------------------------------------------------------
// Hand-written mocks (no third-party mocking dependency)
// ---------------------------------------------------------------------------

type mockRepo struct {
	create     func(ctx context.Context, in NewCoverLetter) (*CoverLetter, error)
	get        func(ctx context.Context, userID, applicationID uuid.UUID) (*CoverLetter, error)
	updateText func(ctx context.Context, userID, applicationID uuid.UUID, body string) (*CoverLetter, error)
	remove     func(ctx context.Context, userID, applicationID uuid.UUID) error

	created     []NewCoverLetter
	deletedRows []uuid.UUID
}

func (m *mockRepo) Create(ctx context.Context, in NewCoverLetter) (*CoverLetter, error) {
	m.created = append(m.created, in)
	if m.create != nil {
		return m.create(ctx, in)
	}
	return &CoverLetter{
		ID:               in.ID,
		ApplicationID:    in.ApplicationID,
		Kind:             in.Kind,
		BodyText:         in.BodyText,
		S3Key:            in.S3Key,
		OriginalFilename: in.OriginalFilename,
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}, nil
}

// The default is "this application has no cover letter yet", which is the
// starting state for every save test.
func (m *mockRepo) GetByApplicationID(ctx context.Context, userID, applicationID uuid.UUID) (*CoverLetter, error) {
	if m.get != nil {
		return m.get(ctx, userID, applicationID)
	}
	return nil, ErrNotFound
}

func (m *mockRepo) UpdateTextBody(ctx context.Context, userID, applicationID uuid.UUID, body string) (
	*CoverLetter,
	error,
) {
	if m.updateText != nil {
		return m.updateText(ctx, userID, applicationID, body)
	}
	return &CoverLetter{
		ID:            uuid.New(),
		ApplicationID: applicationID,
		Kind:          KindText,
		BodyText:      &body,
		UpdatedAt:     time.Now(),
	}, nil
}

func (m *mockRepo) DeleteByApplicationID(ctx context.Context, userID, applicationID uuid.UUID) error {
	m.deletedRows = append(m.deletedRows, applicationID)
	if m.remove != nil {
		return m.remove(ctx, userID, applicationID)
	}
	return nil
}

type mockStorage struct {
	uploadErr  error
	presignErr error

	uploadedKeys  []string
	uploadedTypes []string
	deletedKeys   []string
	lastBody      []byte
}

func (m *mockStorage) Upload(ctx context.Context, key string, body []byte, contentType string) error {
	if m.uploadErr != nil {
		return m.uploadErr
	}
	m.uploadedKeys = append(m.uploadedKeys, key)
	m.uploadedTypes = append(m.uploadedTypes, contentType)
	m.lastBody = body
	return nil
}

func (m *mockStorage) PresignedDownloadURL(ctx context.Context, key, filename string, ttl time.Duration) (
	string,
	time.Time,
	error,
) {
	if m.presignErr != nil {
		return "", time.Time{}, m.presignErr
	}
	return "https://s3.example/" + key, time.Now().Add(ttl), nil
}

func (m *mockStorage) Delete(ctx context.Context, key string) error {
	m.deletedKeys = append(m.deletedKeys, key)
	return nil
}

func ptrString(v string) *string { return &v }

// ---------------------------------------------------------------------------
// SaveText
// ---------------------------------------------------------------------------

func TestSaveText_StoresTrimmedBodyAsTextKind(t *testing.T) {
	appID, id := uuid.New(), uuid.New()
	repo := &mockRepo{}
	store := &mockStorage{}
	svc := NewService(repo, store, WithIDGenerator(func() uuid.UUID { return id }))

	got, err := svc.SaveText(
		context.Background(), testUserID, SaveTextInput{
			ApplicationID: appID,
			BodyText:      "  Dear hiring manager,\n\nI would like to apply.  ",
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(repo.created) != 1 {
		t.Fatalf("created %d rows, want 1", len(repo.created))
	}

	in := repo.created[0]
	if in.Kind != KindText {
		t.Errorf("kind = %q, want %q", in.Kind, KindText)
	}
	if in.ID != id || in.ApplicationID != appID {
		t.Errorf("row = %+v, want id %s on application %s", in, id, appID)
	}
	if in.UserID != testUserID {
		t.Errorf("row user_id = %s, want %s", in.UserID, testUserID)
	}
	if in.BodyText == nil {
		t.Fatal("body_text must be set for a text cover letter")
	}
	if body := *in.BodyText; !strings.HasPrefix(body, "Dear hiring manager") || strings.HasSuffix(body, " ") {
		t.Errorf("body_text = %q, want the surrounding whitespace trimmed", body)
	}
	// The check constraint on cover_letters requires the file columns to be null.
	if in.S3Key != nil || in.OriginalFilename != nil {
		t.Error("a text cover letter must not carry s3_key or original_filename")
	}
	if len(store.uploadedKeys) != 0 {
		t.Error("a typed cover letter must not touch S3")
	}
	if got.Kind != KindText {
		t.Errorf("returned kind = %q", got.Kind)
	}
}

func TestSaveText_BlankBodyIsRejected(t *testing.T) {
	repo := &mockRepo{}
	svc := NewService(repo, &mockStorage{})

	for _, body := range []string{"", "   ", "\n\t "} {
		if _, err := svc.SaveText(
			context.Background(), testUserID,
			SaveTextInput{ApplicationID: uuid.New(), BodyText: body},
		); !errors.Is(err, ErrEmptyBody) {
			t.Errorf("SaveText(%q) err = %v, want ErrEmptyBody", body, err)
		}
	}
	if len(repo.created) != 0 {
		t.Error("nothing should have been written")
	}
}

func TestSaveText_ReplacesExistingFileCoverLetter(t *testing.T) {
	appID := uuid.New()
	oldKey := "cover-letters/" + appID.String() + "/old/letter.pdf"
	repo := &mockRepo{
		get: func(context.Context, uuid.UUID, uuid.UUID) (*CoverLetter, error) {
			return &CoverLetter{
				ID:               uuid.New(),
				ApplicationID:    appID,
				Kind:             KindFile,
				S3Key:            ptrString(oldKey),
				OriginalFilename: ptrString("letter.pdf"),
			}, nil
		},
	}
	store := &mockStorage{}
	svc := NewService(repo, store)

	if _, err := svc.SaveText(
		context.Background(), testUserID,
		SaveTextInput{ApplicationID: appID, BodyText: "typed instead"},
	); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// unique(application_id) means the old row has to go before the new one lands.
	if len(repo.deletedRows) != 1 || repo.deletedRows[0] != appID {
		t.Errorf("deleted rows = %v, want the previous cover letter for %s", repo.deletedRows, appID)
	}
	if len(store.deletedKeys) != 1 || store.deletedKeys[0] != oldKey {
		t.Errorf("deleted keys = %v, want the replaced file %q removed from S3", store.deletedKeys, oldKey)
	}
	if len(repo.created) != 1 || repo.created[0].Kind != KindText {
		t.Error("expected exactly one new text row")
	}
}

// ---------------------------------------------------------------------------
// SaveFile — format allow-list
// ---------------------------------------------------------------------------

func TestSaveFile_AcceptsOnlyPDFDOCXAndDOC(t *testing.T) {
	cases := []struct {
		filename string
		wantErr  error
	}{
		{"cover_letter.pdf", nil},
		{"cover_letter.docx", nil},
		{"cover_letter.doc", nil},
		{"COVER_LETTER.PDF", nil},  // extension check is case-insensitive
		{"Cover Letter.DocX", nil}, //
		{"cover_letter.txt", ErrUnsupportedFileType},
		{"cover_letter.png", ErrUnsupportedFileType},
		{"cover_letter.rtf", ErrUnsupportedFileType},
		{"cover_letter.pages", ErrUnsupportedFileType},
		{"cover_letter", ErrUnsupportedFileType},         // no extension at all
		{"cover_letter.pdf.exe", ErrUnsupportedFileType}, // only the final extension counts
	}

	for _, tc := range cases {
		t.Run(tc.filename, func(t *testing.T) {
			repo := &mockRepo{}
			store := &mockStorage{}
			svc := NewService(repo, store)

			_, err := svc.SaveFile(
				context.Background(), testUserID, SaveFileInput{
					ApplicationID: uuid.New(),
					Filename:      tc.filename,
					Content:       []byte("%PDF pretend"),
				},
			)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr == nil {
				return
			}
			if len(store.uploadedKeys) != 0 {
				t.Error("a rejected format must not reach S3")
			}
			if len(repo.created) != 0 {
				t.Error("a rejected format must not create a row")
			}
		})
	}
}

func TestSaveFile_RejectsBadFormatBeforeTouchingTheExistingRow(t *testing.T) {
	repo := &mockRepo{
		get: func(context.Context, uuid.UUID, uuid.UUID) (*CoverLetter, error) {
			t.Fatal("validation must run before the existing cover letter is looked up")
			return nil, nil
		},
	}
	svc := NewService(repo, &mockStorage{})

	if _, err := svc.SaveFile(
		context.Background(), testUserID, SaveFileInput{
			ApplicationID: uuid.New(),
			Filename:      "notes.txt",
			Content:       []byte("x"),
		},
	); !errors.Is(err, ErrUnsupportedFileType) {
		t.Fatalf("err = %v, want ErrUnsupportedFileType", err)
	}
	if len(repo.deletedRows) != 0 {
		t.Error("a rejected upload must leave the existing cover letter in place")
	}
}

// ---------------------------------------------------------------------------
// SaveFile — storage and row
// ---------------------------------------------------------------------------

func TestSaveFile_StoresFileAndRecordsRow(t *testing.T) {
	appID, id := uuid.New(), uuid.New()
	content := []byte("%PDF-1.7 pretend cover letter")
	repo := &mockRepo{}
	store := &mockStorage{}
	svc := NewService(repo, store, WithIDGenerator(func() uuid.UUID { return id }))

	got, err := svc.SaveFile(
		context.Background(), testUserID, SaveFileInput{
			ApplicationID: appID,
			Filename:      "Cover Letter v2.pdf",
			Content:       content,
			ContentType:   "application/pdf",
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	in := repo.created[0]
	if in.Kind != KindFile {
		t.Errorf("kind = %q, want %q", in.Kind, KindFile)
	}
	// The check constraint requires body_text to be null for a file row.
	if in.BodyText != nil {
		t.Error("a file cover letter must not carry body_text")
	}
	if in.OriginalFilename == nil || *in.OriginalFilename != "Cover Letter v2.pdf" {
		t.Errorf("original_filename = %v, want the name as the user had it", in.OriginalFilename)
	}
	if in.S3Key == nil {
		t.Fatal("s3_key must be recorded")
	}
	if len(store.uploadedKeys) != 1 || store.uploadedKeys[0] != *in.S3Key {
		t.Fatalf("uploaded %v, want the file stored under %q", store.uploadedKeys, *in.S3Key)
	}
	if !strings.HasPrefix(*in.S3Key, "cover-letters/"+appID.String()+"/"+id.String()+"/") {
		t.Errorf("s3 key %q is not namespaced under its application and cover letter", *in.S3Key)
	}
	if strings.Contains(*in.S3Key, " ") {
		t.Errorf("s3 key %q should have the spaces sanitised out", *in.S3Key)
	}
	if string(store.lastBody) != string(content) {
		t.Error("the file bytes did not reach storage intact")
	}
	if got.Kind != KindFile {
		t.Errorf("returned kind = %q", got.Kind)
	}
}

func TestSaveFile_DerivesContentTypeWhenBrowserSendsNone(t *testing.T) {
	cases := map[string]string{
		"letter.pdf":  "application/pdf",
		"letter.doc":  "application/msword",
		"letter.docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	}
	for filename, want := range cases {
		t.Run(filename, func(t *testing.T) {
			store := &mockStorage{}
			svc := NewService(&mockRepo{}, store)

			if _, err := svc.SaveFile(
				context.Background(), testUserID, SaveFileInput{
					ApplicationID: uuid.New(),
					Filename:      filename,
					Content:       []byte("bytes"),
				},
			); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(store.uploadedTypes) != 1 || store.uploadedTypes[0] != want {
				t.Errorf("content type = %v, want %q", store.uploadedTypes, want)
			}
		})
	}
}

func TestSaveFile_Validation(t *testing.T) {
	svc := NewService(&mockRepo{}, &mockStorage{}, WithMaxUploadBytes(8))

	cases := []struct {
		name string
		in   SaveFileInput
		want error
	}{
		{"no filename", SaveFileInput{Content: []byte("x")}, ErrFilenameEmpty},
		{"no content", SaveFileInput{Filename: "letter.pdf"}, ErrEmptyFile},
		{"too large", SaveFileInput{Filename: "letter.pdf", Content: []byte("123456789")}, ErrFileTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.in.ApplicationID = uuid.New()
			if _, err := svc.SaveFile(context.Background(), testUserID, tc.in); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestSaveFile_StorageFailure_DoesNotCreateRow(t *testing.T) {
	boom := errors.New("s3 unavailable")
	repo := &mockRepo{}
	svc := NewService(repo, &mockStorage{uploadErr: boom})

	if _, err := svc.SaveFile(
		context.Background(), testUserID, SaveFileInput{
			ApplicationID: uuid.New(),
			Filename:      "letter.pdf",
			Content:       []byte("bytes"),
		},
	); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the storage error", err)
	}
	if len(repo.created) != 0 {
		t.Error("no row should point at a file that was never stored")
	}
}

func TestSaveFile_DatabaseFailure_RemovesOrphanedObject(t *testing.T) {
	boom := errors.New("insert failed")
	repo := &mockRepo{
		create: func(context.Context, NewCoverLetter) (*CoverLetter, error) { return nil, boom },
	}
	store := &mockStorage{}
	svc := NewService(repo, store)

	if _, err := svc.SaveFile(
		context.Background(), testUserID, SaveFileInput{
			ApplicationID: uuid.New(),
			Filename:      "letter.docx",
			Content:       []byte("bytes"),
		},
	); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the repository error", err)
	}
	if len(store.deletedKeys) != 1 {
		t.Error("the orphaned S3 object should have been deleted")
	}
}

// The WHERE EXISTS ownership guard on CreateCoverLetter is what tells us the
// application id was made up, or belongs to somebody else. The error has to
// survive the rollback.
func TestSaveFile_UnknownApplication_IsReportedAsSuch(t *testing.T) {
	repo := &mockRepo{
		create: func(context.Context, NewCoverLetter) (*CoverLetter, error) { return nil, ErrApplicationNotFound },
	}
	store := &mockStorage{}
	svc := NewService(repo, store)

	if _, err := svc.SaveFile(
		context.Background(), testUserID, SaveFileInput{
			ApplicationID: uuid.New(),
			Filename:      "letter.pdf",
			Content:       []byte("bytes"),
		},
	); !errors.Is(err, ErrApplicationNotFound) {
		t.Fatalf("err = %v, want ErrApplicationNotFound", err)
	}
	if len(store.deletedKeys) != 1 {
		t.Error("the uploaded file should not survive a rejected insert")
	}
}

// ---------------------------------------------------------------------------
// EditText
// ---------------------------------------------------------------------------

func TestEditText_UpdatesTheBodyInPlace(t *testing.T) {
	appID := uuid.New()
	var gotBody string
	repo := &mockRepo{
		get: func(_ context.Context, _, id uuid.UUID) (*CoverLetter, error) {
			return &CoverLetter{ID: uuid.New(), ApplicationID: id, Kind: KindText, BodyText: ptrString("old")}, nil
		},
		updateText: func(_ context.Context, _, _ uuid.UUID, body string) (*CoverLetter, error) {
			gotBody = body
			return &CoverLetter{ApplicationID: appID, Kind: KindText, BodyText: &body}, nil
		},
	}
	svc := NewService(repo, &mockStorage{})

	got, err := svc.EditText(context.Background(), testUserID, appID, "  rewritten opening paragraph  ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotBody != "rewritten opening paragraph" {
		t.Errorf("body = %q, want it trimmed", gotBody)
	}
	if got.Text() != "rewritten opening paragraph" {
		t.Errorf("returned body = %q", got.Text())
	}
	// No new row, no S3 traffic — this is an in-place edit.
	if len(repo.created) != 0 || len(repo.deletedRows) != 0 {
		t.Error("editing must not replace the row")
	}
}

func TestEditText_FileCoverLetterIsRejected(t *testing.T) {
	repo := &mockRepo{
		get: func(_ context.Context, _, id uuid.UUID) (*CoverLetter, error) {
			return &CoverLetter{
				ID:               uuid.New(),
				ApplicationID:    id,
				Kind:             KindFile,
				S3Key:            ptrString("cover-letters/a/b/letter.pdf"),
				OriginalFilename: ptrString("letter.pdf"),
			}, nil
		},
		updateText: func(context.Context, uuid.UUID, uuid.UUID, string) (*CoverLetter, error) {
			t.Fatal("a file cover letter is immutable — no update may be attempted")
			return nil, nil
		},
	}
	store := &mockStorage{}
	svc := NewService(repo, store)

	if _, err := svc.EditText(
		context.Background(), testUserID, uuid.New(), "trying to edit a PDF",
	); !errors.Is(err, ErrNotTextKind) {
		t.Errorf("err = %v, want ErrNotTextKind", err)
	}
	if len(store.deletedKeys) != 0 {
		t.Error("a rejected edit must leave the stored file alone")
	}
}

func TestEditText_MissingCoverLetterIsNotFound(t *testing.T) {
	svc := NewService(&mockRepo{}, &mockStorage{}) // default get returns ErrNotFound

	if _, err := svc.EditText(
		context.Background(), testUserID, uuid.New(), "text",
	); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestEditText_BlankBodyIsRejected(t *testing.T) {
	repo := &mockRepo{
		get: func(context.Context, uuid.UUID, uuid.UUID) (*CoverLetter, error) {
			t.Fatal("a blank body should be rejected before any lookup")
			return nil, nil
		},
	}
	svc := NewService(repo, &mockStorage{})

	if _, err := svc.EditText(
		context.Background(), testUserID, uuid.New(), "   ",
	); !errors.Is(err, ErrEmptyBody) {
		t.Errorf("err = %v, want ErrEmptyBody", err)
	}
}

// ---------------------------------------------------------------------------
// Get and DownloadURL
// ---------------------------------------------------------------------------

func TestGet_ReturnsEitherKind(t *testing.T) {
	appID := uuid.New()

	t.Run("text", func(t *testing.T) {
		repo := &mockRepo{
			get: func(context.Context, uuid.UUID, uuid.UUID) (*CoverLetter, error) {
				return &CoverLetter{ApplicationID: appID, Kind: KindText, BodyText: ptrString("Dear team,")}, nil
			},
		}
		got, err := NewService(repo, &mockStorage{}).Get(context.Background(), testUserID, appID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Kind != KindText || got.Text() != "Dear team," {
			t.Errorf("got %+v, want the typed body", got)
		}
		if got.Filename() != "" {
			t.Error("a text cover letter has no filename")
		}
	})

	t.Run("file", func(t *testing.T) {
		repo := &mockRepo{
			get: func(context.Context, uuid.UUID, uuid.UUID) (*CoverLetter, error) {
				return &CoverLetter{
					ApplicationID:    appID,
					Kind:             KindFile,
					S3Key:            ptrString("cover-letters/a/b/letter.docx"),
					OriginalFilename: ptrString("letter.docx"),
				}, nil
			},
		}
		got, err := NewService(repo, &mockStorage{}).Get(context.Background(), testUserID, appID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Kind != KindFile || got.Filename() != "letter.docx" {
			t.Errorf("got %+v, want the stored file reference", got)
		}
		if got.Text() != "" {
			t.Error("a file cover letter has no body text")
		}
	})
}

func TestGet_MissingCoverLetterIsNotFound(t *testing.T) {
	if _, err := NewService(&mockRepo{}, &mockStorage{}).Get(
		context.Background(), testUserID,
		uuid.New(),
	); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestDownloadURL_FileKindReturnsPresignedLink(t *testing.T) {
	repo := &mockRepo{
		get: func(context.Context, uuid.UUID, uuid.UUID) (*CoverLetter, error) {
			return &CoverLetter{
				Kind:             KindFile,
				S3Key:            ptrString("cover-letters/a/b/letter.pdf"),
				OriginalFilename: ptrString("letter.pdf"),
			}, nil
		},
	}
	svc := NewService(repo, &mockStorage{}, WithDownloadTTL(5*time.Minute))

	link, err := svc.DownloadURL(context.Background(), testUserID, uuid.New())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if link.URL != "https://s3.example/cover-letters/a/b/letter.pdf" {
		t.Errorf("url = %q", link.URL)
	}
	if link.Filename != "letter.pdf" {
		t.Errorf("filename = %q, want the original name for the browser to save under", link.Filename)
	}
	if link.ExpiresAt.Before(time.Now()) {
		t.Error("expiry must be in the future")
	}
}

func TestDownloadURL_TextKindHasNothingToDownload(t *testing.T) {
	repo := &mockRepo{
		get: func(context.Context, uuid.UUID, uuid.UUID) (*CoverLetter, error) {
			return &CoverLetter{Kind: KindText, BodyText: ptrString("Dear team,")}, nil
		},
	}
	if _, err := NewService(repo, &mockStorage{}).DownloadURL(
		context.Background(), testUserID,
		uuid.New(),
	); !errors.Is(err, ErrNotFileKind) {
		t.Errorf("err = %v, want ErrNotFileKind", err)
	}
}

func TestS3Key_SanitisesAndNamespaces(t *testing.T) {
	appID, clID := uuid.New(), uuid.New()

	key := s3Key(appID, clID, "../../etc/My Cover Letter (final).pdf")
	if !strings.HasPrefix(key, "cover-letters/"+appID.String()+"/"+clID.String()+"/") {
		t.Errorf("key %q is not namespaced correctly", key)
	}
	if strings.Contains(key, "..") || strings.Contains(key, " ") || strings.Contains(key, "(") {
		t.Errorf("key %q still contains characters that should have been replaced", key)
	}
}
