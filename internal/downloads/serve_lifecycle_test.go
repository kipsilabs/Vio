package downloads

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/remotestream"
)

// serveLifecycleFixture is one ephemeral original download row backed by a
// virtual file. The lifecycle decisions ServeFile makes are persistent, so the
// tests read the row back from the database instead of inferring them from the
// response.
type serveLifecycleFixture struct {
	pool      *pgxpool.Pool
	repo      *Repository
	userID    int
	fileID    int
	contentID string
	file      *models.MediaFile
}

func seedServeLifecycleFixture(t *testing.T) serveLifecycleFixture {
	t.Helper()
	ctx := context.Background()
	pool := newDownloadsTestPool(t)
	suffix := time.Now().UnixNano()

	var folderID int
	if err := pool.QueryRow(ctx,
		`INSERT INTO media_folders (type, name) VALUES ('movies', $1) RETURNING id`,
		fmt.Sprintf("Serve Lifecycle %d", suffix),
	).Scan(&folderID); err != nil {
		t.Fatalf("seed media folder: %v", err)
	}
	contentID := fmt.Sprintf("serve-lifecycle-content-%d", suffix)
	var fileID int
	if err := pool.QueryRow(ctx,
		`INSERT INTO media_files (content_id, media_folder_id, file_path, file_size, container)
		 VALUES ($1, $2, $3, 4096, 'mp4') RETURNING id`,
		contentID, folderID, "virtual://movie/tt1?result=a",
	).Scan(&fileID); err != nil {
		t.Fatalf("seed media file: %v", err)
	}
	var userID int
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (username, role, download_allowed) VALUES ($1, 'user', true) RETURNING id`,
		fmt.Sprintf("serve-lifecycle-%d", suffix),
	).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM downloads WHERE user_id = $1`, userID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_files WHERE id = $1`, fileID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id = $1`, folderID)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
	})

	return serveLifecycleFixture{
		pool:      pool,
		repo:      NewRepository(pool),
		userID:    userID,
		fileID:    fileID,
		contentID: contentID,
		file: &models.MediaFile{
			ID: fileID, ContentID: contentID, MediaFolderID: folderID,
			Container: "mp4", FilePath: "virtual://movie/tt1?result=a", FileSize: 4096,
		},
	}
}

// createRow inserts one download row in the given state.
func (f serveLifecycleFixture) createRow(t *testing.T, status string) string {
	t.Helper()
	id := fmt.Sprintf("serve-lifecycle-%d", time.Now().UnixNano())
	now := time.Now()
	if err := f.repo.Create(context.Background(), &Download{
		ID: id, UserID: f.userID, MediaFileID: f.fileID, ContentID: f.contentID,
		Kind: KindQueued, Status: status, Format: FormatOriginal, FileSize: 4096,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create download row: %v", err)
	}
	return id
}

// rowState reads the row's status and completion timestamp.
func (f serveLifecycleFixture) rowState(t *testing.T, id string) (string, *time.Time) {
	t.Helper()
	var status string
	var completed *time.Time
	if err := f.pool.QueryRow(context.Background(),
		`SELECT status, completed_at FROM downloads WHERE id = $1`, id,
	).Scan(&status, &completed); err != nil {
		t.Fatalf("read download row: %v", err)
	}
	return status, completed
}

// service wires the service under test around the fixture.
func (f serveLifecycleFixture) service(inputPath string) *Service {
	return f.serviceWithFile(f.file, inputPath)
}

// serviceWithFile wires the service under test around a specific file, so the
// local-original cases can serve a path that is not a virtual source.
func (f serveLifecycleFixture) serviceWithFile(file *models.MediaFile, inputPath string) *Service {
	allowed := true
	return &Service{
		repo:        f.repo,
		fileRepo:    fakeFileResolver{file: file},
		itemAccess:  allowItemAccess{},
		userRepo:    stubUserRepo{user: &models.User{ID: f.userID, DownloadAllowed: &allowed}},
		cfg:         config.DownloadConfig{Enabled: true},
		cfgLoadedAt: time.Now(),
		virtualInput: &stubVirtualResolver{
			inputPath: inputPath,
			cleanup:   func() {},
		},
	}
}

// relayFor registers a loopback relay in front of the given origin handler, so
// the delivery path under test is the real relay and not a stub.
func relayFor(t *testing.T, origin http.Handler) (string, func()) {
	t.Helper()
	server := httptest.NewServer(origin)
	relay := remotestream.NewRelay()
	input, cleanup, err := relay.RegisterInsecure(context.Background(), server.URL)
	if err != nil {
		server.Close()
		t.Fatalf("register relay: %v", err)
	}
	return input, func() {
		cleanup()
		_ = relay.Close(context.Background())
		server.Close()
	}
}

// TestServeFileHeadLeavesEphemeralRowUnchanged covers the HEAD probe: it
// reports the file's metadata and must not move the row.
func TestServeFileHeadLeavesEphemeralRowUnchanged(t *testing.T) {
	f := seedServeLifecycleFixture(t)
	input, closeRelay := relayFor(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "video/mp4")
		response.Header().Set("Content-Length", "4096")
		response.WriteHeader(http.StatusOK)
	}))
	defer closeRelay()

	row := f.createRow(t, StatusQueued)
	request := httptest.NewRequest(http.MethodHead, "/downloads/"+row+"/file", nil)
	response := httptest.NewRecorder()
	if err := f.service(input).ServeFile(context.Background(), response, request, f.userID, "", "", row, catalog.AccessFilter{}); err != nil {
		t.Fatalf("serve head: %v", err)
	}
	if response.Code != http.StatusOK {
		t.Fatalf("head status = %d, want 200", response.Code)
	}
	status, completed := f.rowState(t, row)
	if status != StatusQueued || completed != nil {
		t.Fatalf("head changed the row: status=%q completed=%v", status, completed)
	}
}

// TestServeFileConditionalResponsesDoNotComplete covers the conditional and
// range-error replies: they carry no bytes, so they must not record a
// completed delivery.
func TestServeFileConditionalResponsesDoNotComplete(t *testing.T) {
	const body = "original media bytes"
	const etag = `"source-version"`
	for _, testCase := range []struct {
		name        string
		byteRange   string
		ifNoneMatch string
		ifMatch     string
		wantStatus  int
	}{
		{name: "not modified", ifNoneMatch: etag, wantStatus: http.StatusNotModified},
		{name: "precondition failed", ifMatch: `"old"`, wantStatus: http.StatusPreconditionFailed},
		{name: "range not satisfiable", byteRange: "bytes=9000-", wantStatus: http.StatusRequestedRangeNotSatisfiable},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			f := seedServeLifecycleFixture(t)
			input, closeRelay := relayFor(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				response.Header().Set("ETag", etag)
				response.Header().Set("Content-Type", "video/mp4")
				http.ServeContent(response, request, "source.mp4", time.Unix(1700000000, 0), strings.NewReader(body))
			}))
			defer closeRelay()

			row := f.createRow(t, StatusQueued)
			request := httptest.NewRequest(http.MethodGet, "/downloads/"+row+"/file", nil)
			request.Header.Set("Range", testCase.byteRange)
			request.Header.Set("If-None-Match", testCase.ifNoneMatch)
			request.Header.Set("If-Match", testCase.ifMatch)
			response := httptest.NewRecorder()
			if err := f.service(input).ServeFile(context.Background(), response, request, f.userID, "", "", row, catalog.AccessFilter{}); err != nil {
				t.Fatalf("serve conditional: %v", err)
			}
			if response.Code != testCase.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, testCase.wantStatus)
			}
			status, completed := f.rowState(t, row)
			if completed != nil {
				t.Fatalf("a %d reply recorded completion", testCase.wantStatus)
			}
			if status == StatusCompleted || status == StatusFailed {
				t.Fatalf("a %d reply moved the row to %q", testCase.wantStatus, status)
			}
		})
	}
}

// TestServeFileDeliveredBytesComplete covers the responses that carry the
// file: they record the completed delivery.
func TestServeFileDeliveredBytesComplete(t *testing.T) {
	const body = "original media bytes"
	for _, testCase := range []struct {
		name       string
		byteRange  string
		wantStatus int
	}{
		{name: "full body", wantStatus: http.StatusOK},
		{name: "requested range", byteRange: "bytes=0-5", wantStatus: http.StatusPartialContent},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			f := seedServeLifecycleFixture(t)
			input, closeRelay := relayFor(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				response.Header().Set("Content-Type", "video/mp4")
				http.ServeContent(response, request, "source.mp4", time.Unix(1700000000, 0), strings.NewReader(body))
			}))
			defer closeRelay()

			row := f.createRow(t, StatusQueued)
			request := httptest.NewRequest(http.MethodGet, "/downloads/"+row+"/file", nil)
			request.Header.Set("Range", testCase.byteRange)
			response := httptest.NewRecorder()
			if err := f.service(input).ServeFile(context.Background(), response, request, f.userID, "", "", row, catalog.AccessFilter{}); err != nil {
				t.Fatalf("serve: %v", err)
			}
			if response.Code != testCase.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, testCase.wantStatus)
			}
			status, completed := f.rowState(t, row)
			if status != StatusCompleted || completed == nil {
				t.Fatalf("delivered row: status=%q completed=%v, want completed", status, completed)
			}
		})
	}
}

// TestServeFileInterruptedTransferStaysResumable covers the interrupted
// delivery: the row must not be failed, so the client can ask for the
// remaining range and finish the download.
func TestServeFileInterruptedTransferStaysResumable(t *testing.T) {
	const body = "original media bytes"
	f := seedServeLifecycleFixture(t)
	truncate := true
	input, closeRelay := relayFor(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "video/mp4")
		if truncate {
			// Promise more bytes than are sent: the transfer breaks after the
			// response was committed.
			response.Header().Set("Content-Length", "4096")
			response.WriteHeader(http.StatusOK)
			_, _ = response.Write([]byte("orig"))
			return
		}
		http.ServeContent(response, request, "source.mp4", time.Unix(1700000000, 0), strings.NewReader(body))
	}))
	defer closeRelay()

	row := f.createRow(t, StatusQueued)
	request := httptest.NewRequest(http.MethodGet, "/downloads/"+row+"/file", nil)
	response := httptest.NewRecorder()
	err := f.service(input).ServeFile(context.Background(), response, request, f.userID, "", "", row, catalog.AccessFilter{})
	if !errors.Is(err, ErrResponseCommitted) {
		t.Fatalf("interrupted transfer error = %v, want ErrResponseCommitted", err)
	}
	status, completed := f.rowState(t, row)
	if status == StatusFailed {
		t.Fatal("an interrupted transfer failed the row for the rest of its life")
	}
	if completed != nil {
		t.Fatal("an interrupted transfer recorded completion")
	}

	// The client resumes: the row serves the rest and completes.
	truncate = false
	resume := httptest.NewRequest(http.MethodGet, "/downloads/"+row+"/file", nil)
	resume.Header.Set("Range", "bytes=4-")
	resumeResponse := httptest.NewRecorder()
	if err := f.service(input).ServeFile(context.Background(), resumeResponse, resume, f.userID, "", "", row, catalog.AccessFilter{}); err != nil {
		t.Fatalf("resume transfer: %v", err)
	}
	if resumeResponse.Code != http.StatusPartialContent {
		t.Fatalf("resume status = %d, want 206", resumeResponse.Code)
	}
	status, completed = f.rowState(t, row)
	if status != StatusCompleted || completed == nil {
		t.Fatalf("resumed row: status=%q completed=%v, want completed", status, completed)
	}
}

// TestServeFileLocalOriginalReadFailureStaysResumable covers a local original
// whose read fails mid-transfer: the row stays resumable instead of being
// failed for good.
func TestServeFileLocalOriginalReadFailureStaysResumable(t *testing.T) {
	f := seedServeLifecycleFixture(t)
	// A directory cannot be read as a file, so the transfer breaks after the
	// response was committed.
	unreadable := t.TempDir()
	local := *f.file
	local.Container = "mp4"
	local.FilePath = unreadable
	local.FileSize = 4096

	row := f.createRow(t, StatusQueued)
	request := httptest.NewRequest(http.MethodGet, "/downloads/"+row+"/file", nil)
	response := httptest.NewRecorder()
	err := f.serviceWithFile(&local, "").ServeFile(context.Background(), response, request, f.userID, "", "", row, catalog.AccessFilter{})
	if err == nil {
		t.Fatal("unreadable local file served without an error")
	}
	status, completed := f.rowState(t, row)
	if status == StatusFailed {
		t.Fatal("a failed read failed the row for the rest of its life")
	}
	if completed != nil {
		t.Fatal("a failed read recorded completion")
	}
}

// TestServeFileLocalOriginalVanishedFileFailsRow covers the refusal that can
// never be retried: the file is gone, so the row is failed.
func TestServeFileLocalOriginalVanishedFileFailsRow(t *testing.T) {
	f := seedServeLifecycleFixture(t)
	local := *f.file
	local.Container = "mp4"
	local.FilePath = filepath.Join(t.TempDir(), "gone.mp4")

	row := f.createRow(t, StatusQueued)
	request := httptest.NewRequest(http.MethodGet, "/downloads/"+row+"/file", nil)
	response := httptest.NewRecorder()
	err := f.serviceWithFile(&local, "").ServeFile(context.Background(), response, request, f.userID, "", "", row, catalog.AccessFilter{})
	if !errors.Is(err, catalog.ErrItemNotFound) {
		t.Fatalf("vanished file error = %v, want catalog.ErrItemNotFound", err)
	}
	status, _ := f.rowState(t, row)
	if status != StatusFailed {
		t.Fatalf("vanished file left the row in %q, want failed", status)
	}
}

// TestServeFileManagedRowKeepsEphemeralLifecycle covers the managed path: a
// managed entry is authorized on (user, profile, device) and must not pick up
// the ephemeral lifecycle transitions.
func TestServeFileManagedRowKeepsEphemeralLifecycle(t *testing.T) {
	f := seedServeLifecycleFixture(t)
	input, closeRelay := relayFor(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "video/mp4")
		_, _ = response.Write([]byte("original media bytes"))
	}))
	defer closeRelay()

	row := f.createRow(t, StatusQueued)
	profileID := fmt.Sprintf("serve-lifecycle-profile-%d", time.Now().UnixNano())
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO user_profiles (id, user_id, name) VALUES ($1, $2, 'Phone')`, profileID, f.userID,
	); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	deviceID := fmt.Sprintf("serve-lifecycle-device-%d", time.Now().UnixNano())
	if err := f.repo.EnsureDevice(context.Background(), f.userID, profileID, deviceID, "Phone", "android"); err != nil {
		t.Fatalf("ensure device: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM user_devices WHERE user_id = $1`, f.userID)
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM user_profiles WHERE user_id = $1`, f.userID)
	})
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE downloads SET profile_id=$2, device_id=$3 WHERE id=$1`, row, profileID, deviceID,
	); err != nil {
		t.Fatalf("mark row managed: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/downloads/"+row+"/file", nil)
	response := httptest.NewRecorder()
	if err := f.service(input).ServeFile(context.Background(), response, request, f.userID, profileID, deviceID, row, catalog.AccessFilter{}); err != nil {
		t.Fatalf("serve managed row: %v", err)
	}
	if response.Code != http.StatusOK {
		t.Fatalf("managed status = %d, want 200", response.Code)
	}
	status, completed := f.rowState(t, row)
	if status != StatusQueued || completed != nil {
		t.Fatalf("managed row picked up the ephemeral lifecycle: status=%q completed=%v", status, completed)
	}
}
