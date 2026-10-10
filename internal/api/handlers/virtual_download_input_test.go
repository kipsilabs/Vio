package handlers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/remotestream"
)

// A provider-backed media file resolved for download preparation must reuse the
// playback virtual-transport resolver and yield a playable (non-virtual) input,
// mirroring the movie and episode shapes the picker serves.
func TestResolveVirtualDownloadInputReusesPlaybackResolver(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		owner    int
		resolved string
	}{
		{name: "movie", path: "virtual://movie/tt1?result=a", resolved: "http://altmount:8080/stremio/test/play?url=http%3A%2F%2Fprowlarr%3A9696%2F19%2Fdownload"},
		{name: "episode", path: "virtual://series/tt1/1/1?result=b", owner: 5, resolved: "http://altmount:8080/stremio/test/play?url=http%3A%2F%2Fprowlarr%3A9696%2F42%2Fdownload"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			relay := remotestream.NewRelay()
			defer func() { _ = relay.Close(context.Background()) }()

			calls := 0
			h := &PlaybackHandler{
				RemoteStreamRelay:   relay,
				AllowPrivateStreams: func(int) bool { return true },
				VirtualMediaResolver: VirtualMediaResolverFunc(func(context.Context, string, int, int, string) (string, error) {
					calls++
					return tc.resolved, nil
				}),
			}
			file := &models.MediaFile{ID: 7, Container: "virtual", FilePath: tc.path, VirtualOwnerInstallationID: tc.owner}

			input, cleanup, err := h.ResolveVirtualDownloadInput(context.Background(), file, 1, "profile")
			if err != nil {
				t.Fatalf("ResolveVirtualDownloadInput error = %v", err)
			}
			if cleanup == nil {
				t.Fatal("cleanup = nil, want a relay release")
			}
			defer cleanup()
			if calls != 1 {
				t.Fatalf("resolver calls = %d, want 1", calls)
			}
			if strings.TrimSpace(input) == "" || strings.HasPrefix(strings.ToLower(input), "virtual://") {
				t.Fatalf("resolved input = %q, want a playable non-virtual input", input)
			}
		})
	}
}

// An unresolvable provider row must still be refused, not handed to the
// executor as a bare virtual:// path.
func TestResolveVirtualDownloadInputRefusesUnresolvedSource(t *testing.T) {
	relay := remotestream.NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()

	h := &PlaybackHandler{
		RemoteStreamRelay: relay,
		VirtualMediaResolver: VirtualMediaResolverFunc(func(context.Context, string, int, int, string) (string, error) {
			return "", errors.New("no streams available from provider")
		}),
	}
	file := &models.MediaFile{ID: 8, Container: "virtual", FilePath: "virtual://movie/tt404?result=dead"}

	if _, _, err := h.ResolveVirtualDownloadInput(context.Background(), file, 1, "profile"); err == nil {
		t.Fatal("ResolveVirtualDownloadInput error = nil, want refusal")
	}
}

func TestResolveVirtualDownloadInputRejectsMissingFile(t *testing.T) {
	h := &PlaybackHandler{}
	if _, _, err := h.ResolveVirtualDownloadInput(context.Background(), nil, 1, "profile"); err == nil {
		t.Fatal("ResolveVirtualDownloadInput(nil) error = nil, want refusal")
	}
}
