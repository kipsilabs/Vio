package handlers

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/remotestream"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

const virtualPreparationBudget = 90 * time.Second

type VirtualPlaybackPreparationService struct {
	Detail       VirtualCandidatesDetailReader
	FileByID     func(context.Context, int) (*models.MediaFile, error)
	List         VirtualPlaybackStreamLister
	Enabled      func(context.Context) bool
	AllowPrivate func(context.Context) bool
	Claim        func(context.Context, string, time.Duration) (bool, error)
	Context      context.Context

	once       sync.Once
	active     chan struct{}
	mu         sync.Mutex
	recent     map[string]time.Time
	publicHTTP *http.Client
	privateHTTP *http.Client
}

func (s *VirtualPlaybackPreparationService) PrepareVirtualPlayback(ctx context.Context, userID int, profileID, contentID string, fileID int, filter catalog.AccessFilter) error {
	if s == nil || s.Enabled == nil || !s.Enabled(ctx) || s.Detail == nil || s.FileByID == nil || s.List == nil {
		return apiError(http.StatusServiceUnavailable, "unavailable", "Early stream preparation is unavailable.")
	}
	detail, err := s.Detail.WatchDetail(ctx, userID, profileID, contentID, filter)
	if err != nil {
		return err
	}
	if detail == nil || (detail.Type != itemTypeMovie && detail.Type != itemTypeEpisode) {
		return apiError(http.StatusUnprocessableEntity, "validation_failed", "Select a movie or an individual episode.")
	}
	selectedPath := ""
	for _, version := range detail.Versions {
		if version.FileID == fileID {
			selectedPath = version.FilePath
			break
		}
	}
	if selectedPath == "" {
		return apiError(http.StatusNotFound, "not_found", "The selected version is unavailable.")
	}
	file, err := s.FileByID(ctx, fileID)
	if err != nil {
		return apiError(http.StatusInternalServerError, "internal_error", "Failed to load the selected version.")
	}
	if file == nil || file.FilePath != selectedPath {
		return apiError(http.StatusNotFound, "not_found", "The selected version is unavailable.")
	}
	parsed, err := url.Parse(selectedPath)
	if err != nil || parsed.Scheme != "virtual" || parsed.Query().Get("result") == "" || file.FailedAt != nil {
		return nil
	}
	s.once.Do(func() {
		s.active = make(chan struct{}, 4)
		s.recent = make(map[string]time.Time)
		s.publicHTTP = remotestream.NewSafeClient()
		s.privateHTTP = remotestream.NewInsecureClient()
		for _, client := range []*http.Client{s.publicHTTP, s.privateHTTP} {
			client.Timeout = virtualPreparationBudget
			if transport, ok := client.Transport.(*http.Transport); ok {
				transport.ResponseHeaderTimeout = virtualPreparationBudget
			}
		}
	})
	select {
	case s.active <- struct{}{}:
	default:
		return apiError(http.StatusServiceUnavailable, "unavailable", "Early stream preparation is busy.")
	}
	key := fmt.Sprintf("virtual-preparation:%x", sha256.Sum256([]byte(fmt.Sprintf("%d:%s", file.VirtualOwnerInstallationID, file.FilePath))))
	claimed, err := s.claim(ctx, key)
	if err != nil || !claimed {
		<-s.active
		if err != nil {
			return apiError(http.StatusServiceUnavailable, "unavailable", "Early stream preparation is unavailable.")
		}
		return nil
	}
	workCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), virtualPreparationBudget)
	serviceContext := s.Context
	if serviceContext == nil {
		serviceContext = context.Background()
	}
	stop := context.AfterFunc(serviceContext, cancel)
	selected := *file
	go func() {
		defer func() { <-s.active }()
		defer cancel()
		defer stop()
		s.prepare(workCtx, &selected, userID, profileID)
	}()
	return nil
}

func (s *VirtualPlaybackPreparationService) claim(ctx context.Context, key string) (bool, error) {
	const cooldown = 2 * time.Minute
	if s.Claim != nil {
		return s.Claim(ctx, key, cooldown)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for previous, expires := range s.recent {
		if !now.Before(expires) {
			delete(s.recent, previous)
		}
	}
	if _, exists := s.recent[key]; exists || len(s.recent) >= 1024 {
		return false, nil
	}
	s.recent[key] = now.Add(cooldown)
	return true, nil
}

func (s *VirtualPlaybackPreparationService) prepare(ctx context.Context, file *models.MediaFile, userID int, profileID string) {
	if ctx.Err() != nil || !s.Enabled(ctx) {
		return
	}
	streams, err := s.List.ListVirtualPlaybackStreams(ctx, virtualPlaybackNeutralKey(file.FilePath), userID, profileID, file.VirtualOwnerInstallationID)
	if err != nil {
		return
	}
	candidate, ok := selectedVirtualPreparationCandidate(file, streams)
	if !ok || ctx.Err() != nil || !s.Enabled(ctx) {
		return
	}
	allowPrivate := s.AllowPrivate != nil && s.AllowPrivate(ctx)
	providerURL, err := virtuallibrary.ValidateProviderStreamURL(ctx, candidate.ProviderURL, allowPrivate)
	if err != nil {
		return
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, providerURL, nil)
	if err != nil {
		return
	}
	for name, value := range candidate.RequestHeaders {
		request.Header.Set(name, value)
	}
	request.Header.Set("Range", "bytes=0-0")
	client := s.publicHTTP
	if allowPrivate {
		client = s.privateHTTP
	}
	response, err := client.Do(request)
	if err != nil {
		slog.DebugContext(ctx, "early stream preparation did not complete", "component", "api", "file_id", file.ID)
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent {
		return
	}
	if _, err := io.CopyN(io.Discard, response.Body, 1); err != nil {
		return
	}
	slog.DebugContext(ctx, "early stream preparation received a byte", "component", "api", "file_id", file.ID)
}

func selectedVirtualPreparationCandidate(file *models.MediaFile, streams []VirtualPlaybackStream) (VirtualPlaybackStream, bool) {
	if file == nil {
		return VirtualPlaybackStream{}, false
	}
	parsed, err := url.Parse(file.FilePath)
	if err != nil || parsed.Scheme != "virtual" || parsed.Query().Get("result") == "" {
		return VirtualPlaybackStream{}, false
	}
	for _, candidate := range streams {
		if candidate.URI == file.FilePath && candidate.SourceType == "usenet" &&
			!candidate.Rejected && (!candidate.VisibilitySpecified || candidate.Visible) &&
			(candidate.ProviderExpiresAt == nil || candidate.ProviderExpiresAt.After(time.Now())) &&
			strings.TrimSpace(candidate.ProviderURL) != "" {
			return candidate, true
		}
	}
	return VirtualPlaybackStream{}, false
}