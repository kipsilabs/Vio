package downloads

import (
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/downloadprepare"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/remotestream"
	"github.com/Silo-Server/silo-server/internal/tonemap"
)

type stubVirtualResolver struct {
	inputPath string
	cleanup   func()
	err       error
	calls     int
}

func (s *stubVirtualResolver) ResolveVirtualDownloadInput(_ context.Context, _ *models.MediaFile) (string, func(), error) {
	s.calls++
	return s.inputPath, s.cleanup, s.err
}

func TestVirtualOriginalDelivery(t *testing.T) {
	const body = "original media bytes"
	const etag = `"source-version"`
	for _, testCase := range []struct {
		name, method, byteRange, ifRange, ifNoneMatch, ifMatch, ifUnmodifiedSince string
		wantStatus                                                                int
		wantBody                                                                  string
	}{
		{name: "original", method: http.MethodGet, wantStatus: http.StatusOK, wantBody: body},
		{name: "resume", method: http.MethodGet, byteRange: "bytes=2-5", ifRange: etag, wantStatus: http.StatusPartialContent, wantBody: "igin"},
		{name: "stale resume", method: http.MethodGet, byteRange: "bytes=2-5", ifRange: `"old"`, wantStatus: http.StatusOK, wantBody: body},
		{name: "head", method: http.MethodHead, wantStatus: http.StatusOK},
		{name: "not modified", method: http.MethodGet, ifNoneMatch: etag, wantStatus: http.StatusNotModified},
		{name: "source changed", method: http.MethodGet, ifMatch: `"old"`, wantStatus: http.StatusPreconditionFailed},
		{name: "source modified", method: http.MethodGet, ifUnmodifiedSince: time.Unix(1600000000, 0).UTC().Format(http.TimeFormat), wantStatus: http.StatusPreconditionFailed},
		{name: "invalid range", method: http.MethodGet, byteRange: "bytes=100-", wantStatus: http.StatusRequestedRangeNotSatisfiable},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			origin := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" {
					t.Error("request credentials reached the relay")
				}
				response.Header().Set("ETag", etag)
				response.Header().Set("Content-Type", "video/mp4")
				response.Header().Set("Set-Cookie", "provider-secret=value")
				response.Header().Set("Content-Disposition", `attachment; filename="private-source.mp4"`)
				http.ServeContent(response, request, "source.mp4", time.Unix(1700000000, 0), strings.NewReader(body))
			}))
			defer origin.Close()
			relay := remotestream.NewRelay()
			defer func() { _ = relay.Close(context.Background()) }()
			input, cleanup, err := relay.RegisterInsecure(context.Background(), origin.URL)
			if err != nil {
				t.Fatal(err)
			}
			cleanups := 0
			resolver := &stubVirtualResolver{inputPath: input, cleanup: func() { cleanups++; cleanup() }}
			file := &models.MediaFile{ID: 7, Container: "mp4", FilePath: "virtual://movie/tt1?result=a"}
			service := &Service{fileRepo: fakeFileResolver{file: file}}
			service.SetVirtualInputResolver(resolver)
			target, err := service.resolveDownloadBytesTarget(context.Background(), &Download{MediaFileID: file.ID, Format: FormatOriginal}, catalog.AccessFilter{}, true, true)
			if err != nil {
				t.Fatal(err)
			}
			if target.ProxyEligible || target.virtualFile != file || resolver.calls != 0 {
				t.Fatal("virtual original must stay on the API host and resolve only when served")
			}
			request := httptest.NewRequest(testCase.method, "/downloads/test/file", nil)
			request.Header.Set("Range", testCase.byteRange)
			request.Header.Set("If-Range", testCase.ifRange)
			request.Header.Set("If-None-Match", testCase.ifNoneMatch)
			request.Header.Set("If-Match", testCase.ifMatch)
			request.Header.Set("If-Unmodified-Since", testCase.ifUnmodifiedSince)
			request.Header.Set("Authorization", "Bearer client-secret")
			request.Header.Set("Cookie", "client-secret=value")
			response := httptest.NewRecorder()
			if err := service.serveFileTarget(request.Context(), response, request, target, 1); err != nil {
				t.Fatal(err)
			}
			if response.Code != testCase.wantStatus || response.Body.String() != testCase.wantBody {
				t.Fatalf("response = %d %q, want %d %q", response.Code, response.Body.String(), testCase.wantStatus, testCase.wantBody)
			}
			if response.Header().Get("Set-Cookie") != "" || strings.Contains(response.Header().Get("Content-Disposition"), "private-source") {
				t.Fatal("provider metadata leaked into response")
			}
			if response.Header().Get("Content-Disposition") != attachmentDisposition("download-7.mp4") || response.Header().Get("Content-Type") != "video/mp4" {
				t.Fatal("virtual download must carry a media filename and content type")
			}
			if testCase.wantStatus == http.StatusPartialContent && response.Header().Get("Content-Range") != "bytes 2-5/20" {
				t.Fatalf("Content-Range = %q", response.Header().Get("Content-Range"))
			}
			if resolver.calls != 1 || cleanups != 1 {
				t.Fatalf("resolve calls = %d, cleanup calls = %d", resolver.calls, cleanups)
			}
		})
	}
}

func TestVirtualOriginalAccessDeniedBeforeResolve(t *testing.T) {
	file := &models.MediaFile{ID: 7, Container: "mp4", FilePath: "virtual://movie/tt1?result=a", MediaFolderID: 2}
	resolver := &stubVirtualResolver{}
	service := &Service{fileRepo: fakeFileResolver{file: file}, virtualInput: resolver}
	_, err := service.resolveDownloadBytesTarget(context.Background(), &Download{MediaFileID: file.ID, Format: FormatOriginal}, catalog.AccessFilter{AllowedLibraryIDs: []int{1}}, true, true)
	if !errors.Is(err, catalog.ErrItemNotFound) || resolver.calls != 0 {
		t.Fatalf("access denied = %v, resolver calls = %d", err, resolver.calls)
	}
}

// A row registered before a probe committed carries no container, so the
// delivery filename would fall back to .bin. Resolving the bytes target probes
// it best effort and serves the bytes under the container the probe persisted.
func TestVirtualOriginalLegacyRowProbesForFilename(t *testing.T) {
	file := &models.MediaFile{ID: 7, FilePath: "virtual://movie/tt1?result=a"}
	probed := *file
	probed.Container = containerMKV
	resolver := &stubVirtualMetadataResolver{file: &probed}
	service := &Service{fileRepo: fakeFileResolver{file: file}, virtualInput: resolver}

	target, err := service.resolveDownloadBytesTarget(context.Background(), &Download{MediaFileID: file.ID, Format: FormatOriginal}, catalog.AccessFilter{}, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if target.virtualFile != &probed || resolver.metadataCalls != 1 {
		t.Fatalf("target file = %+v, probes = %d, want the probed row", target.virtualFile, resolver.metadataCalls)
	}
	if target.ProxyEligible {
		t.Fatal("virtual original must stay on the API host")
	}
	if filename := virtualDownloadFilename(target.virtualFile); filename != "download-7.mkv" {
		t.Fatalf("filename = %q, want the probed container", filename)
	}
}

// A failed probe must not fail a download whose bytes are still servable: the
// row keeps serving and only the filename falls back to .bin.
func TestVirtualOriginalLegacyRowProbeFailureKeepsServing(t *testing.T) {
	file := &models.MediaFile{ID: 7, FilePath: "virtual://movie/tt1?result=a"}
	service := &Service{fileRepo: fakeFileResolver{file: file}, virtualInput: &stubVirtualMetadataResolver{err: errors.New("provider unavailable")}}

	target, err := service.resolveDownloadBytesTarget(context.Background(), &Download{MediaFileID: file.ID, Format: FormatOriginal}, catalog.AccessFilter{}, false, false)
	if err != nil {
		t.Fatalf("bytes target error = %v, want the row still servable", err)
	}
	if target.virtualFile != file {
		t.Fatalf("target file = %+v, want the original row", target.virtualFile)
	}
	if filename := virtualDownloadFilename(target.virtualFile); filename != "download-7.bin" {
		t.Fatalf("filename = %q, want the .bin fallback", filename)
	}
}

func TestVirtualOriginalSize(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		status    int
		length    string
		want      int64
		wantError bool
	}{
		{name: "exact source length", status: http.StatusOK, length: "422458714", want: 422458714},
		{name: "unknown length", status: http.StatusOK},
		{name: "head unsupported", status: http.StatusMethodNotAllowed},
		{name: "head not implemented", status: http.StatusNotImplemented},
		{name: "provider unavailable", status: http.StatusBadGateway, wantError: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			origin := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodHead || request.Header.Get("Accept-Encoding") != "identity" {
					t.Error("size lookup must request identity HEAD metadata")
				}
				if testCase.length != "" {
					response.Header().Set("Content-Length", testCase.length)
				}
				response.WriteHeader(testCase.status)
			}))
			defer origin.Close()
			relay := remotestream.NewRelay()
			defer func() { _ = relay.Close(context.Background()) }()
			input, cleanup, err := relay.RegisterInsecure(context.Background(), origin.URL)
			if err != nil {
				t.Fatal(err)
			}
			cleanups := 0
			service := &Service{virtualInput: &stubVirtualResolver{inputPath: input, cleanup: func() { cleanups++; cleanup() }}}
			file := &models.MediaFile{ID: 7, FileSize: 536870000, FilePath: "virtual://movie/tt1?result=a"}
			size, err := service.virtualOriginalSize(context.Background(), file)
			if (err != nil) != testCase.wantError || size != testCase.want || cleanups != 1 {
				t.Fatalf("size = %d, error = %v, cleanups = %d", size, err, cleanups)
			}
			if file.FileSize != 536870000 {
				t.Fatal("size lookup must not mutate catalog metadata")
			}
		})
	}
}

func TestVirtualOriginalSizeRejectsPlaylistContentTypes(t *testing.T) {
	for _, contentType := range []string{"application/vnd.apple.mpegurl", "application/dash+xml"} {
		t.Run(contentType, func(t *testing.T) {
			origin := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				response.Header().Set("Content-Type", contentType)
				response.Header().Set("Content-Length", "512")
				response.WriteHeader(http.StatusOK)
			}))
			defer origin.Close()
			cleanups := 0
			service := &Service{virtualInput: &stubVirtualResolver{inputPath: origin.URL, cleanup: func() { cleanups++ }}}
			size, err := service.virtualOriginalSize(context.Background(), &models.MediaFile{ID: 7, FilePath: "virtual://movie/tt1?result=a"})
			if !errors.Is(err, ErrFormatUnavailable) || size != 0 || cleanups != 1 {
				t.Fatalf("size = %d, error = %v, cleanups = %d, want a playlist refusal", size, err, cleanups)
			}
		})
	}
}

func TestVirtualOriginalSizeRefusesRedirect(t *testing.T) {
	followed := false
	target := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		followed = true
		response.Header().Set("Content-Length", "12")
		response.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, target.URL, http.StatusFound)
	}))
	defer origin.Close()

	service := &Service{virtualInput: &stubVirtualResolver{inputPath: origin.URL}}
	if _, err := service.virtualOriginalSize(context.Background(), &models.MediaFile{ID: 7}); err == nil {
		t.Fatal("redirected size probe error = nil, want the 302 reported as a failure")
	}
	if followed {
		t.Fatal("size probe followed a redirect")
	}
}

func TestVirtualOriginalSizeHonorsContextDeadline(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	defer origin.Close()

	service := &Service{virtualInput: &stubVirtualResolver{inputPath: origin.URL}}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := service.virtualOriginalSize(ctx, &models.MediaFile{ID: 7}); err == nil {
		t.Fatal("timed-out size probe error = nil")
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("size probe ignored its context deadline after %s", elapsed)
	}
}

func TestVirtualOriginalSizeReleasesSourceOnEveryPath(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		resolveErr error
		input      string
	}{
		{name: "resolve failure", resolveErr: errors.New("provider unavailable")},
		{name: "non-relay input", input: "https://provider.invalid/source?token=private"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			cleanups := 0
			service := &Service{virtualInput: &stubVirtualResolver{
				inputPath: testCase.input,
				err:       testCase.resolveErr,
				cleanup:   func() { cleanups++ },
			}}
			if _, err := service.virtualOriginalSize(context.Background(), &models.MediaFile{ID: 7}); err == nil {
				t.Fatal("size probe error = nil, want a refusal")
			}
			if cleanups != 1 {
				t.Fatalf("cleanup calls = %d, want 1", cleanups)
			}
		})
	}
}

func TestVirtualOriginalHeadUnsupported(t *testing.T) {
	for _, status := range []int{http.StatusMethodNotAllowed, http.StatusNotImplemented} {
		origin := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			response.WriteHeader(status)
		}))
		relay := remotestream.NewRelay()
		input, cleanup, err := relay.RegisterInsecure(context.Background(), origin.URL)
		if err != nil {
			t.Fatal(err)
		}
		service := &Service{virtualInput: &stubVirtualResolver{inputPath: input, cleanup: cleanup}}
		request := httptest.NewRequest(http.MethodHead, "/downloads/test/file", nil)
		response := httptest.NewRecorder()
		err = service.serveVirtualOriginal(request.Context(), response, request, &models.MediaFile{ID: 7, Container: "mp4"}, 1)
		_ = relay.Close(context.Background())
		origin.Close()
		if err != nil || response.Code != http.StatusOK || response.Header().Get("Content-Length") != "" || response.Header().Get("Content-Disposition") != attachmentDisposition("download-7.mp4") {
			t.Fatalf("HEAD %d: status=%d headers=%v error=%v", status, response.Code, response.Header(), err)
		}
	}
}

func TestVirtualOriginalRejectsGenericMIMEPlaylist(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/octet-stream")
		_, _ = response.Write([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:10\n#EXTINF:10,\nsegment.ts\n#EXT-X-ENDLIST\n"))
	}))
	defer origin.Close()
	relay := remotestream.NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()
	input, cleanup, err := relay.RegisterInsecure(context.Background(), origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{virtualInput: &stubVirtualResolver{inputPath: input, cleanup: cleanup}}
	request := httptest.NewRequest(http.MethodGet, "/downloads/test/file", nil)
	response := httptest.NewRecorder()
	err = service.serveVirtualOriginal(request.Context(), response, request, &models.MediaFile{ID: 7, Container: "mp4"}, 1)
	if !errors.Is(err, ErrFormatUnavailable) || response.Body.Len() != 0 || response.Header().Get("Content-Disposition") != "" {
		t.Fatalf("playlist must fail before committing a download: error=%v headers=%v", err, response.Header())
	}
}

// A playlist is a manifest, not a downloadable original. Delivery and size
// lookup both refuse the provider content types, for GET and HEAD, before any
// response header or byte reaches the client.
func TestVirtualOriginalRejectsPlaylistContentTypes(t *testing.T) {
	for _, contentType := range []string{"application/vnd.apple.mpegurl", "application/dash+xml"} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			t.Run(method+" "+contentType, func(t *testing.T) {
				origin := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
					response.Header().Set("Content-Type", contentType)
					response.Header().Set("Content-Length", "512")
					if request.Method != http.MethodHead {
						_, _ = response.Write([]byte("#EXTM3U\n#EXT-X-ENDLIST\n"))
					}
				}))
				defer origin.Close()
				request := httptest.NewRequest(method, "/downloads/test/file", nil)
				response := httptest.NewRecorder()
				service := &Service{virtualInput: &stubVirtualResolver{inputPath: origin.URL}}
				err := service.serveVirtualOriginal(request.Context(), response, request, &models.MediaFile{ID: 7, Container: containerMP4}, 1)
				if !errors.Is(err, ErrFormatUnavailable) || response.Body.Len() != 0 || response.Header().Get("Content-Disposition") != "" {
					t.Fatalf("playlist delivery = %v, headers = %v, body = %q", err, response.Header(), response.Body.String())
				}
			})
		}
	}
}

func TestVirtualOriginalMultipartRanges(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "video/mp4")
		http.ServeContent(response, request, "source.mp4", time.Time{}, strings.NewReader("0123456789"))
	}))
	defer origin.Close()
	relay := remotestream.NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()
	input, cleanup, err := relay.RegisterInsecure(context.Background(), origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{virtualInput: &stubVirtualResolver{inputPath: input, cleanup: cleanup}}
	request := httptest.NewRequest(http.MethodGet, "/downloads/test/file", nil)
	request.Header.Set("Range", "bytes=0-1,4-5")
	response := httptest.NewRecorder()
	if err := service.serveVirtualOriginal(request.Context(), response, request, &models.MediaFile{ID: 7, Container: "mp4"}, 1); err != nil {
		t.Fatal(err)
	}
	mediaType, params, err := mime.ParseMediaType(response.Header().Get("Content-Type"))
	if err != nil || response.Code != http.StatusPartialContent || mediaType != "multipart/byteranges" || params["boundary"] == "" {
		t.Fatalf("invalid multipart response: status=%d type=%q error=%v", response.Code, response.Header().Get("Content-Type"), err)
	}
	reader := multipart.NewReader(response.Body, params["boundary"])
	for _, expected := range []string{"01", "45"} {
		part, err := reader.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(part)
		if err != nil || string(body) != expected {
			t.Fatalf("part = %q, error = %v", body, err)
		}
	}
	if _, err := reader.NextPart(); !errors.Is(err, io.EOF) {
		t.Fatalf("unexpected trailing part: %v", err)
	}
}

type stubVirtualMetadataResolver struct {
	stubVirtualResolver
	file          *models.MediaFile
	err           error
	metadataCalls int
}

func (resolver *stubVirtualMetadataResolver) ResolveVirtualDownloadMetadata(context.Context, *models.MediaFile) (*models.MediaFile, error) {
	resolver.metadataCalls++
	if resolver.err != nil {
		return nil, resolver.err
	}
	return resolver.file, nil
}

// A virtual row whose container is still unknown is probed even though the
// request carries no device capabilities: the create body sends none, and both
// the download filename and the format decision need the resolved container.
func TestVirtualOriginalMetadataResolution(t *testing.T) {
	probedFile := func(file *models.MediaFile) *models.MediaFile {
		resolved := *file
		resolved.Container = containerMP4
		return &resolved
	}

	for _, testCase := range []struct {
		name   string
		file   *models.MediaFile
		probed bool
	}{
		{name: "placeholder container", file: &models.MediaFile{ID: 7, Container: "virtual", FilePath: "virtual://movie/tt1?result=a"}, probed: true},
		{name: "empty container", file: &models.MediaFile{ID: 8, FilePath: "virtual://movie/tt2?result=b"}, probed: true},
		{name: "known container", file: &models.MediaFile{ID: 9, Container: containerMKV, FilePath: "virtual://movie/tt3?result=c"}},
		{name: "local file", file: &models.MediaFile{ID: 10, Container: containerMKV, FilePath: "/media/movie.mkv"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			resolver := &stubVirtualMetadataResolver{file: probedFile(testCase.file)}
			service := &Service{virtualInput: resolver}

			got, err := service.resolveDownloadMetadata(context.Background(), testCase.file)
			if err != nil {
				t.Fatalf("metadata resolution error = %v", err)
			}
			if testCase.probed {
				if got != resolver.file || resolver.metadataCalls != 1 {
					t.Fatalf("metadata resolution = %+v, calls = %d, want the probed file once", got, resolver.metadataCalls)
				}
				return
			}
			if got != testCase.file || resolver.metadataCalls != 0 {
				t.Fatalf("metadata resolution = %+v, calls = %d, want the row unchanged and no probe", got, resolver.metadataCalls)
			}
		})
	}
}

func TestVirtualOriginalMetadataResolutionWithoutMetadataSupport(t *testing.T) {
	file := &models.MediaFile{ID: 7, Container: "virtual", FilePath: "virtual://movie/tt1?result=a"}
	resolver := &stubVirtualResolver{}
	service := &Service{virtualInput: resolver}

	got, err := service.resolveDownloadMetadata(context.Background(), file)
	if err != nil || got != file {
		t.Fatalf("metadata resolution = %+v, error = %v, want the row unchanged", got, err)
	}
	if resolver.calls != 0 {
		t.Fatalf("input resolver calls = %d, want 0", resolver.calls)
	}
}

func TestVirtualOriginalMetadataResolutionFailureRefuses(t *testing.T) {
	file := &models.MediaFile{ID: 7, Container: "virtual", FilePath: "virtual://movie/tt1?result=a"}
	otherFile := *file
	otherFile.ID = 8
	otherSource := *file
	otherSource.FilePath = "virtual://movie/tt1?result=other"

	for _, testCase := range []struct {
		name     string
		resolver *stubVirtualMetadataResolver
	}{
		{name: "probe error", resolver: &stubVirtualMetadataResolver{err: errors.New("provider unavailable")}},
		{name: "no metadata", resolver: &stubVirtualMetadataResolver{}},
		{name: "different file", resolver: &stubVirtualMetadataResolver{file: &otherFile}},
		{name: "different source", resolver: &stubVirtualMetadataResolver{file: &otherSource}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			service := &Service{virtualInput: testCase.resolver}
			got, err := service.resolveDownloadMetadata(context.Background(), file)
			if !errors.Is(err, ErrQualityUnavailable) || got != nil {
				t.Fatalf("metadata resolution = %+v, error = %v, want ErrQualityUnavailable", got, err)
			}
			if testCase.resolver.metadataCalls != 1 {
				t.Fatalf("metadata probes = %d, want 1", testCase.resolver.metadataCalls)
			}
		})
	}
}

func TestVirtualOriginalDeliveryFailures(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		status     int
		truncate   bool
		resolveErr error
		input      string
	}{
		{name: "resolve failure", resolveErr: errors.New("private provider URL")},
		{name: "non-relay source", input: "https://provider.invalid/source?token=private"},
		{name: "upstream failure", status: http.StatusBadGateway},
		{name: "redirect refused", status: http.StatusFound},
		{name: "truncated response", status: http.StatusOK, truncate: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			origin := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if testCase.truncate {
					response.Header().Set("Content-Length", "50")
				}
				if testCase.status == http.StatusFound {
					response.Header().Set("Location", "https://provider.invalid/source?token=private")
				}
				response.WriteHeader(testCase.status)
				_, _ = response.Write([]byte("short"))
			}))
			defer origin.Close()
			input := origin.URL
			if testCase.input != "" {
				input = testCase.input
			}
			cleanups := 0
			service := &Service{virtualInput: &stubVirtualResolver{inputPath: input, err: testCase.resolveErr, cleanup: func() { cleanups++ }}}
			request := httptest.NewRequest(http.MethodGet, "/downloads/test/file", nil)
			response := httptest.NewRecorder()
			err := service.serveVirtualOriginal(request.Context(), response, request, &models.MediaFile{ID: 7}, 1)
			if err == nil || strings.Contains(err.Error(), "private") || cleanups != 1 {
				t.Fatalf("err = %v, cleanup calls = %d", err, cleanups)
			}
			if errors.Is(err, ErrResponseCommitted) != testCase.truncate {
				t.Fatalf("committed error = %v", err)
			}
		})
	}
}

func TestApplyVirtualInputRewritesPreparerInput(t *testing.T) {
	virtual := &models.MediaFile{ID: 7, Container: "virtual", FilePath: "virtual://movie/tt1?result=a"}
	released := false
	resolver := &stubVirtualResolver{
		inputPath: "http://127.0.0.1:4000/source/token/movie.mp4",
		cleanup:   func() { released = true },
	}
	m := &ArtifactManager{virtualInput: resolver}
	opts := playback.TranscodeOpts{InputPath: virtual.FilePath}

	cleanup, err := m.applyVirtualInput(context.Background(), virtual, &opts)
	if err != nil {
		t.Fatalf("applyVirtualInput error = %v", err)
	}
	if resolver.calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", resolver.calls)
	}
	if opts.InputPath != resolver.inputPath {
		t.Fatalf("InputPath = %q, want resolved relay %q", opts.InputPath, resolver.inputPath)
	}
	if opts.CanonicalInputPath != virtual.FilePath {
		t.Fatalf("CanonicalInputPath = %q, want %q", opts.CanonicalInputPath, virtual.FilePath)
	}
	if cleanup == nil {
		t.Fatal("cleanup = nil, want the relay release")
	}
	cleanup()
	if !released {
		t.Fatal("cleanup did not release the relay registration")
	}
}

func TestApplyVirtualInputNonVirtualIsNoop(t *testing.T) {
	local := &models.MediaFile{ID: 8, Container: "mkv", FilePath: "/media/movie.mkv"}
	resolver := &stubVirtualResolver{inputPath: "http://relay"}
	m := &ArtifactManager{virtualInput: resolver}
	opts := playback.TranscodeOpts{InputPath: local.FilePath}

	cleanup, err := m.applyVirtualInput(context.Background(), local, &opts)
	if err != nil || cleanup != nil {
		t.Fatalf("applyVirtualInput = (cleanup set: %t, err: %v), want no cleanup and no error", cleanup != nil, err)
	}
	if resolver.calls != 0 {
		t.Fatalf("resolver calls = %d, want 0", resolver.calls)
	}
	if opts.InputPath != local.FilePath {
		t.Fatalf("InputPath = %q, want unchanged %q", opts.InputPath, local.FilePath)
	}
}

func TestApplyVirtualInputResolverFailureReleasesCleanup(t *testing.T) {
	virtual := &models.MediaFile{ID: 9, Container: "virtual", FilePath: "virtual://movie/tt2?result=b"}
	released := false
	resolver := &stubVirtualResolver{
		err:     errors.New("provider unavailable"),
		cleanup: func() { released = true },
	}
	m := &ArtifactManager{virtualInput: resolver}
	opts := playback.TranscodeOpts{InputPath: virtual.FilePath}

	if _, err := m.applyVirtualInput(context.Background(), virtual, &opts); err == nil {
		t.Fatal("applyVirtualInput error = nil, want resolver failure")
	}
	if !released {
		t.Fatal("failed resolve did not release the partial relay registration")
	}
}

func TestApplyVirtualInputEmptyResolveRefused(t *testing.T) {
	virtual := &models.MediaFile{ID: 10, Container: "virtual", FilePath: "virtual://movie/tt3?result=c"}
	m := &ArtifactManager{virtualInput: &stubVirtualResolver{}}
	opts := playback.TranscodeOpts{InputPath: virtual.FilePath}

	if _, err := m.applyVirtualInput(context.Background(), virtual, &opts); err == nil {
		t.Fatal("applyVirtualInput error = nil, want empty resolve refused")
	}
}

func TestApplyVirtualInputWithoutResolverDefers(t *testing.T) {
	virtual := &models.MediaFile{ID: 11, Container: "virtual", FilePath: "virtual://movie/tt4?result=d"}
	m := &ArtifactManager{}
	opts := playback.TranscodeOpts{InputPath: virtual.FilePath}

	_, err := m.applyVirtualInput(context.Background(), virtual, &opts)
	if !errors.Is(err, errVirtualInputResolverPending) {
		t.Fatalf("applyVirtualInput error = %v, want errVirtualInputResolverPending", err)
	}
}

// The frozen execution fingerprint of a virtual artifact is derived from the
// durable canonical source identity, so a per-attempt relay URL rotation never
// invalidates a healthy artifact while a real source change still does. This is
// the invariant the hourly integrity probe and retries rely on.
func TestArtifactExecutionFingerprintUsesCanonicalPathForVirtual(t *testing.T) {
	canonical := "virtual://movie/tt1?result=a"
	frozen := playback.TranscodeOpts{
		InputPath: canonical, ToneMapPolicy: tonemap.PolicySoftwareOnly,
		ToneMapMode: tonemap.ModeSoftware, ToneMapSourceKind: tonemap.SourcePQ,
		ToneMapRecipeVersion:  playback.TransformationHDRToSDRToneMapRecipeVersionV3,
		ToneMapSourceRevision: tonemap.SourceRevision{MediaFileID: 42, FileSize: 100},
		TargetCodecVideo:      "h264", TargetCodecAudio: "aac",
	}
	a := &Artifact{ID: "art-fingerprint", ToneMapMode: tonemap.ModeSoftware}
	a.ParamsHash = downloadprepare.NewRequest(a.ID, frozen).ExecutionFingerprint()
	if a.ParamsHash == "" {
		t.Fatal("frozen virtual fingerprint is empty")
	}

	// The resolver rewrites InputPath to a rotating relay URL but preserves the
	// canonical identity; the frozen fingerprint must still match.
	resolved := frozen
	resolved.InputPath = "http://relay/attempt-2/token"
	resolved.CanonicalInputPath = canonical
	if !artifactExecutionFingerprintMatches(a, resolved) {
		t.Fatal("relay rotation invalidated the frozen virtual fingerprint")
	}

	// A genuine source change must not match.
	changed := resolved
	changed.CanonicalInputPath = "virtual://movie/tt1?result=other-release"
	if artifactExecutionFingerprintMatches(a, changed) {
		t.Fatal("a different canonical source matched the frozen fingerprint")
	}
}

func TestCancelAbandonedPrepareIgnoresEmptyArtifact(t *testing.T) {
	// No repository is touched for an empty id, so this is safe on a zero-value
	// manager with no database.
	m := &ArtifactManager{}
	m.CancelAbandonedPrepare(context.Background(), "")
	m.SetVirtualInputResolver(nil)
}
