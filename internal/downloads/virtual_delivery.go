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

func (s *Service) resolveDownloadMetadata(ctx context.Context, file *models.MediaFile, caps playback.ClientCapabilities) (*models.MediaFile, error) {
	if !isVirtualMediaFile(file) || !hasCapabilities(caps) || (file.Container != "" && !strings.EqualFold(file.Container, "virtual")) {
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

// Media containers that map to a filename extension the download response
// advertises. Anything else falls back to .bin so the client never receives a
// provider-controlled name.
const (
	containerMatroska = "matroska"
	extensionMP4      = "mp4"
	extensionMKV      = "mkv"
	extensionAAC      = "aac"
)

// virtualDownloadTransport is the loopback-only client used for the API's own
// relay: no proxy, no compression (so byte counts stay exact), and a bounded
// wait for response headers. Redirects are refused by the client so a provider
// can never move the request off the relay.
func virtualDownloadTransport() *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{DisableCompression: true, ResponseHeaderTimeout: 30 * time.Second}
	}
	transport := base.Clone()
	transport.Proxy = nil
	transport.DisableCompression = true
	transport.ResponseHeaderTimeout = 30 * time.Second
	return transport
}

var virtualDownloadClient = &http.Client{
	Transport: virtualDownloadTransport(),
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func (s *Service) SetVirtualInputResolver(resolver VirtualInputResolver) {
	s.virtualInput = resolver
}

// virtualRelayInput resolves the download's authorized provider input and
// requires it to be a loopback URL. The client below only speaks to loopback,
// so anything else is refused before a request is built.
//
// Two separate guarantees meet here: the resolver wiring is trusted to hand
// back this host's own relay registration, and the loopback check below
// refuses any input that would leave the machine. The loopback check alone
// does not prove the URL belongs to this relay.
func (s *Service) virtualRelayInput(ctx context.Context, file *models.MediaFile) (string, func(), error) {
	if s.virtualInput == nil {
		return "", nil, ErrFormatUnavailable
	}
	input, cleanup, err := s.virtualInput.ResolveVirtualDownloadInput(ctx, file)
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		return "", nil, errors.New("virtual download source is unavailable")
	}
	if !isLoopbackPrepareInput(input) {
		if cleanup != nil {
			cleanup()
		}
		return "", nil, errors.New("virtual download source requires a local relay")
	}
	return input, cleanup, nil
}

func (s *Service) virtualOriginalSize(ctx context.Context, file *models.MediaFile) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	input, cleanup, err := s.virtualRelayInput(ctx, file)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, input, nil)
	if err != nil {
		return 0, errors.New("invalid virtual download relay request")
	}
	request.Header.Set("Accept-Encoding", "identity")
	//nolint:gosec // input is this host's own relay registration, trusted from the resolver wiring and confirmed to be loopback by virtualRelayInput
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
	return max(0, response.ContentLength), nil
}

func virtualDownloadFilename(file *models.MediaFile) string {
	extension := strings.ToLower(strings.TrimSpace(file.Container))
	switch extension {
	case containerMatroska:
		extension = extensionMKV
	case extensionMP4, extensionMKV, "webm", "mov", "avi", "m4v", "ts", "mpeg", "mp3", "m4a", "flac", "ogg", "opus", "wav", extensionAAC:
	default:
		extension = "bin"
	}
	return fmt.Sprintf("download-%d.%s", file.ID, extension)
}

func (s *Service) serveVirtualOriginal(ctx context.Context, w http.ResponseWriter, r *http.Request, file *models.MediaFile, userID int) error {
	input, cleanup, err := s.virtualRelayInput(ctx, file)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return err
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
	//nolint:gosec // input is this host's own relay registration, trusted from the resolver wiring and confirmed to be loopback by virtualRelayInput
	response, err := virtualDownloadClient.Do(request)
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
