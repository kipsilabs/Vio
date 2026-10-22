package downloads

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/Silo-Server/silo-server/internal/downloadprepare"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

type virtualDownloadMetadataResolver interface {
	ResolveVirtualDownloadMetadata(context.Context, *models.MediaFile) (*models.MediaFile, error)
}

// Media-container names as the catalog stores them. A virtual download's
// filename extension comes from the persisted probe result, never from the
// provider URL.
const (
	containerMatroska = "matroska"
	containerMP4      = "mp4"
	containerMKV      = "mkv"
	containerAAC      = "aac"
)

// resolveDownloadMetadata returns file with the container, tracks, and size a
// virtual probe measured. It probes whenever the row's container is still
// unknown; device capabilities play no part, because every download route needs
// the resolved container for its filename and format decision, and the create
// body sends none. Non-virtual and already-probed rows pass through unchanged.
func (s *Service) resolveDownloadMetadata(ctx context.Context, file *models.MediaFile) (*models.MediaFile, error) {
	if !isVirtualMediaFile(file) || (file.Container != "" && !strings.EqualFold(file.Container, "virtual")) {
		return file, nil
	}
	resolver, ok := s.virtualInput.(virtualDownloadMetadataResolver)
	if !ok {
		return file, nil
	}
	resolved, err := resolver.ResolveVirtualDownloadMetadata(ctx, file)
	if err != nil || resolved == nil || resolved.ID != file.ID || resolved.FilePath != file.FilePath {
		return nil, ErrQualityUnavailable
	}
	return resolved, nil
}

// virtualDownloadClient opens only the loopback relay registration the service
// resolved from trusted server wiring (see isLoopbackPrepareInput): proxies are
// disabled and redirects are refused, so a relay response cannot steer it to
// another host.
var virtualDownloadClient = func() *http.Client {
	// A replaced http.DefaultTransport has no *http.Transport to clone, so
	// configure a fresh one instead of panicking at init.
	var transport *http.Transport
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = base.Clone()
	} else {
		transport = &http.Transport{}
	}
	transport.Proxy = nil
	transport.DisableCompression = true
	transport.ResponseHeaderTimeout = 30 * time.Second
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}()

func (s *Service) SetVirtualInputResolver(resolver VirtualInputResolver) {
	s.virtualInput = resolver
}

func (s *Service) virtualOriginalSize(ctx context.Context, file *models.MediaFile) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if s.virtualInput == nil {
		return 0, ErrFormatUnavailable
	}
	input, cleanup, err := s.virtualInput.ResolveVirtualDownloadInput(ctx, file)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil || !isLoopbackPrepareInput(input) {
		return 0, errors.New("virtual download source is unavailable")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, input, nil)
	if err != nil {
		return 0, errors.New("invalid virtual download relay request")
	}
	request.Header.Set("Accept-Encoding", "identity")
	response, err := virtualDownloadClient.Do(request)
	if err != nil {
		return 0, errors.New("virtual download relay is unavailable")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusMethodNotAllowed || response.StatusCode == http.StatusNotImplemented {
		return 0, nil
	}
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("virtual download relay returned %d", response.StatusCode)
	}
	// A playlist has no single-file length, so a size probe would report a
	// manifest's length as the download's. Refuse it the way delivery does.
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	if strings.Contains(contentType, "mpegurl") || strings.Contains(contentType, "dash+xml") {
		return 0, ErrFormatUnavailable
	}
	return max(0, response.ContentLength), nil
}

func virtualDownloadFilename(file *models.MediaFile) string {
	extension := strings.ToLower(strings.TrimSpace(file.Container))
	switch extension {
	case containerMatroska:
		extension = containerMKV
	case containerMP4, containerMKV, "webm", "mov", "avi", "m4v", "ts", "mpeg", "mp3", "m4a", "flac", "ogg", "opus", "wav", containerAAC:
	default:
		extension = "bin"
	}
	return fmt.Sprintf("download-%d.%s", file.ID, extension)
}

func (s *Service) serveVirtualOriginal(ctx context.Context, w http.ResponseWriter, r *http.Request, file *models.MediaFile, userID int) error {
	if s.virtualInput == nil {
		return ErrFormatUnavailable
	}
	input, cleanup, err := s.virtualInput.ResolveVirtualDownloadInput(ctx, file)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return errors.New("virtual download source is unavailable")
	}
	if !isLoopbackPrepareInput(input) {
		return errors.New("virtual download source requires a local relay")
	}
	request, err := http.NewRequestWithContext(ctx, r.Method, input, nil)
	if err != nil {
		return errors.New("invalid virtual download relay request")
	}
	for _, name := range []string{"Range", "If-Range", "If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since"} {
		if value := r.Header.Get(name); value != "" {
			request.Header.Set(name, value)
		}
	}
	request.Header.Set("Accept-Encoding", "identity")
	response, err := virtualDownloadClient.Do(request) //nolint:gosec // loopback relay URL from trusted wiring; proxies disabled and redirects refused
	if err != nil {
		return errors.New("virtual download relay is unavailable")
	}
	defer func() { _ = response.Body.Close() }()
	if r.Method == http.MethodHead && (response.StatusCode == http.StatusMethodNotAllowed || response.StatusCode == http.StatusNotImplemented) {
		filename := virtualDownloadFilename(file)
		w.Header().Set("Content-Disposition", attachmentDisposition(filename))
		w.Header().Set("Content-Type", playback.MimeFromExtension(filename))
		w.WriteHeader(http.StatusOK)
		return nil
	}
	if !downloadprepare.RelayStatusAllowed(response.StatusCode) {
		return fmt.Errorf("virtual download relay returned %d", response.StatusCode)
	}
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	if strings.Contains(contentType, "mpegurl") || strings.Contains(contentType, "dash+xml") {
		return ErrFormatUnavailable
	}
	downloadprepare.CopyResponseHeaders(w.Header(), response.Header)
	filename := virtualDownloadFilename(file)
	w.Header().Set("Content-Disposition", attachmentDisposition(filename))
	mediaType, params, parseErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode != http.StatusPartialContent || parseErr != nil || mediaType != "multipart/byteranges" || params["boundary"] == "" {
		w.Header().Set("Content-Type", playback.MimeFromExtension(filename))
	}
	if response.StatusCode == http.StatusPreconditionFailed || response.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(response.StatusCode)
		return nil
	}
	w.WriteHeader(response.StatusCode)
	if r.Method == http.MethodHead || response.StatusCode == http.StatusNotModified {
		return nil
	}
	var reader io.Reader = response.Body
	if s.bandwidth != nil {
		reader = s.bandwidth.ThrottledStreamReader(ctx, reader, userID)
	}
	written, err := io.Copy(w, reader)
	if err != nil || (response.ContentLength >= 0 && written != response.ContentLength) {
		return fmt.Errorf("%w: virtual download transfer interrupted", ErrResponseCommitted)
	}
	return nil
}
