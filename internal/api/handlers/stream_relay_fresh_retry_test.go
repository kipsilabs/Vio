package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/remotestream"
)

// TestStreamResolveFreshRegistrationMintsNewRelayToken pins the serve-path
// retry contract for issue 1: a plain re-resolve of an unchanged provider URL
// reuses the live relay token, while the retry that sets the fresh-registration
// marker (withVirtualRelayFreshRegistration) mints a new one. Without the
// marker a 502 retry would replay the very registration whose upstream failed.
func TestStreamResolveFreshRegistrationMintsNewRelayToken(t *testing.T) {
	file := &models.MediaFile{
		ID:                         9,
		ContentID:                  "movie-fresh-relay",
		FilePath:                   "virtual://movie/movie-fresh-relay?result=cand-a",
		VirtualOwnerInstallationID: 7,
	}
	relay := remotestream.NewRelay()
	t.Cleanup(func() { _ = relay.Close(context.Background()) })
	h := &StreamHandler{
		RemoteStreamRelay:   relay,
		AllowPrivateStreams: func(int) bool { return true },
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			return ResolvedVirtualMedia{URL: "http://provider.local/video.mp4", URI: file.FilePath, CandidateID: "cand-a"}, nil
		}),
	}

	first, cleanupFirst, err := h.resolveVirtualInputURI(context.Background(), file, 1, "profile-1", false)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if cleanupFirst != nil {
		defer cleanupFirst()
	}
	second, cleanupSecond, err := h.resolveVirtualInputURI(context.Background(), file, 1, "profile-1", false)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if cleanupSecond != nil {
		defer cleanupSecond()
	}
	if first.URL != second.URL {
		t.Fatalf("plain re-registration minted a new token: first=%q second=%q", first.URL, second.URL)
	}

	fresh, cleanupFresh, err := h.resolveVirtualInputURI(withVirtualRelayFreshRegistration(context.Background()), file, 1, "profile-1", false)
	if err != nil {
		t.Fatalf("fresh resolve: %v", err)
	}
	if cleanupFresh != nil {
		defer cleanupFresh()
	}
	if fresh.URL == first.URL {
		t.Fatalf("fresh re-registration reused the live relay token %q", first.URL)
	}
}

func TestIsRelayTokenNotFoundError(t *testing.T) {
	relayErr := func() error {
		return errors.New(`ffmpeg subtitle stream failed: exit status 8 (stderr: [in#0] Error opening input: Server returned 404 Not Found
Error opening input file http://127.0.0.1:36367/source/abc/stream)`)
	}
	cases := []struct {
		name  string
		err   error
		input string
		want  bool
	}{
		{"nil", nil, "http://127.0.0.1:1/source/x/stream", false},
		{"relay 404", relayErr(), "http://127.0.0.1:36367/source/abc/stream", true},
		{"ipv6 relay 404", relayErr(), "http://[::1]:36367/source/abc/stream", true},
		{"non-relay 404 stays permanent", relayErr(), "https://provider.example/video.mkv", false},
		{
			"relay 500 is not token expiry",
			errors.New("ffmpeg subtitle stream failed: exit status 8 (stderr: Server returned 500)"),
			"http://127.0.0.1:1/source/x/stream",
			false,
		},
		{"local path never matches", relayErr(), "/media/movie.mkv", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRelayTokenNotFoundError(tc.err, tc.input); got != tc.want {
				t.Fatalf("isRelayTokenNotFoundError = %v, want %v", got, tc.want)
			}
		})
	}
}
