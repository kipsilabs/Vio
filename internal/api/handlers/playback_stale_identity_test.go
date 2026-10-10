package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

func TestDropStaleAudioTrackIdentityV3(t *testing.T) {
	file := &models.MediaFile{ID: 6210092}

	tests := []struct {
		name    string
		trackID string
		want    bool
	}{
		{"matching identity", playback.TrackIDV3(6210092, "audio", 0), false},
		{"stale identity from rotated candidate", playback.TrackIDV3(6210091, "audio", 0), true},
		{"malformed identity", "not-a-track-id", false},
		{"subtitle kind on audio field", playback.TrackIDV3(6210091, "subtitle", 0), false},
		{"empty id", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dropStaleAudioTrackIdentityV3(context.Background(), file, tt.trackID); got != tt.want {
				t.Errorf("dropStaleAudioTrackIdentityV3(%q) = %v, want %v", tt.trackID, got, tt.want)
			}
		})
	}

	if dropStaleAudioTrackIdentityV3(context.Background(), nil, playback.TrackIDV3(1, "audio", 0)) {
		t.Fatal("nil file must not report staleness")
	}
}

// The friend's exact scenario: client replays file:6210091:audio:0 after the
// candidate row was replaced by 6210092. The stale ID is dropped, then the
// preference pipeline resolves track 0 of the new file.
func TestResolveV3AudioIndexAfterStaleDrop(t *testing.T) {
	newFile := &models.MediaFile{ID: 6210092, AudioTracks: []models.AudioTrack{
		{Codec: "truehd", Language: "en", Channels: 8},
		{Codec: "ac3", Language: "en", Channels: 6},
	}}

	stale := playback.TrackIDV3(6210091, "audio", 0)
	if !dropStaleAudioTrackIdentityV3(context.Background(), newFile, stale) {
		t.Fatal("expected staleness detection for rotated candidate")
	}

	index, _, err := resolveV3AudioIndex(newFile, "", nil)
	if err != nil {
		t.Fatalf("post-drop resolve err = %v", err)
	}
	if index != 0 {
		t.Fatalf("index = %d, want 0 (default first track)", index)
	}

	// A genuinely malformed identity still rejects.
	if _, _, err := resolveV3AudioIndex(newFile, "garbage", nil); err == nil {
		t.Fatal("malformed identity must still error")
	}
}

func TestRemapAudioSelectionV3_ToleratesStaleSourceIdentity(t *testing.T) {
	source := &models.MediaFile{ID: 6210091, AudioTracks: []models.AudioTrack{
		{Codec: "eac3", Language: "en", Channels: 6},
	}}
	target := &models.MediaFile{ID: 6210092, AudioTracks: []models.AudioTrack{
		{Codec: "truehd", Language: "en", Channels: 8},
		{Codec: "eac3", Language: "en", Channels: 6}, // same release layout as source
	}}

	req := &playback.StartRequestV3{}
	req.AudioTrackID = playback.TrackIDV3(99999, "audio", 0) // foreign source identity

	err := remapAudioSelectionV3(source, target, req)
	if err != nil {
		t.Fatalf("remapAudioSelectionV3 err = %v, want nil (stale identity tolerated)", err)
	}
	if req.AudioTrackIndex == nil || *req.AudioTrackIndex != 1 {
		t.Fatalf("audio index mismatch: got %+v", req.AudioTrackIndex)
	}
	if req.AudioTrackID != playback.TrackIDV3(target.ID, "audio", 1) {
		t.Fatalf("rewritten ID = %q", req.AudioTrackID)
	}

	// Malformed IDs still reject.
	bad := &playback.StartRequestV3{}
	bad.AudioTrackID = "junk"
	if err := remapAudioSelectionV3(source, target, bad); err == nil {
		t.Fatal("malformed audio identity must still error")
	}
}

func TestRemapSubtitleSelectionV3_ToleratesStaleSourceIdentity(t *testing.T) {
	source := &models.MediaFile{ID: 6210091, ExternalSubtitles: []models.ExternalSubtitle{
		{Language: "en", Format: "srt"},
	}}
	target := &models.MediaFile{ID: 6210092, ExternalSubtitles: []models.ExternalSubtitle{
		{Language: "en", Format: "srt"},
	}}

	req := &playback.StartRequestV3{}
	req.SubtitleTrackID = playback.TrackIDV3(424242, "subtitle", 0)

	_, err := (&PlaybackHandler{}).remapSubtitleSelectionV3(context.Background(), source, target, req)
	if err != nil {
		t.Fatalf("remapSubtitleSelectionV3 err = %v, want nil", err)
	}

	malformed := &playback.StartRequestV3{}
	malformed.SubtitleTrackID = "also-junk"
	if _, err := (&PlaybackHandler{}).remapSubtitleSelectionV3(context.Background(), source, target, malformed); err == nil {
		t.Fatal("malformed subtitle identity must still error")
	}
}

func TestVirtualRuntimePlausible(t *testing.T) {
	tests := []struct {
		name           string
		probedSeconds  int
		runtimeMinutes int
		want           bool
	}{
		{"exact match (206min film)", 12374, 206, true},
		{"within +15%", 13380, 206, true}, // 206*60=12360; +15%=14214
		{"57-minute junk vs 206-min film", 3403, 206, false},
		{"118-minute rip vs 206-min film", 7070, 206, false},
		{"unknown probe passes", 0, 206, true},
		{"unknown runtime passes", 12374, 0, true},
		{"episode within tolerance", 2580, 42, true}, // 2520 expected
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := virtualRuntimePlausible(tt.probedSeconds, tt.runtimeMinutes); got != tt.want {
				t.Errorf("virtualRuntimePlausible(%d, %d) = %v, want %v", tt.probedSeconds, tt.runtimeMinutes, got, tt.want)
			}
		})
	}
}

func TestVirtualStickyPinLifecycle(t *testing.T) {
	h := &PlaybackHandler{}
	key := "content-key"

	if got := h.peekVirtualSticky(key); got != "" {
		t.Fatalf("initial peek = %q, want empty", got)
	}

	candidates := []VirtualPlaybackStream{
		{URI: "virtual://movie/tt5537002?result=aaa"},
		{URI: "virtual://movie/tt5537002?result=bbb"},
	}

	h.pinVirtualSticky(key, candidates[1].URI)
	if got := h.peekVirtualSticky(key); got != candidates[1].URI {
		t.Fatalf("peek = %q, want pinned uri", got)
	}

	reordered := h.applyVirtualStickyPin(key, candidates[1].URI, candidates)
	if reordered[0].URI != candidates[1].URI {
		t.Fatalf("pinned candidate not moved to front: %+v", reordered)
	}

	// Pinned URI no longer offered -> pin released.
	gone := []VirtualPlaybackStream{candidates[0]}
	out := h.applyVirtualStickyPin(key, candidates[1].URI, gone)
	if h.peekVirtualSticky(key) != "" {
		t.Fatal("pin should be released when candidate vanishes from offerings")
	}
	if len(out) != 1 || out[0].URI != candidates[0].URI {
		t.Fatalf("fallback list wrong: %+v", out)
	}

	// Explicit unpin on failure.
	h.pinVirtualSticky(key, candidates[0].URI)
	h.unpinVirtualSticky(key, candidates[0].URI)
	if h.peekVirtualSticky(key) != "" {
		t.Fatal("unpin should clear the pin")
	}
}

// A start that carries a selected identity minted against a rotated-out
// effective file must degrade to the default track pipeline instead of failing
// structural validation. The pre-drop runs before the file-bound id/index
// check; a genuinely mismatched pair on the request's own file is not stale and
// must still be rejected.
func TestHandleStartPlaybackV3StaleSelectedTrackIdentityDegrades(t *testing.T) {
	newHandler := func(t *testing.T) (*PlaybackHandler, *models.MediaFile) {
		t.Helper()
		file := v3HandlerFixtureFile(t)
		handler := NewPlaybackHandler(playback.NewSessionManager(0, 0), testPlaybackFileResolver{file: file})
		handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"allow_4k_transcode": "true"}}
		handler.ItemAccess = allowAllPlaybackItemAccess{}
		return handler, file
	}
	start := func(t *testing.T, handler *PlaybackHandler, req playback.StartRequestV3) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", strings.NewReader(marshalV3StartRequest(t, req))).WithContext(newAuthorizedPlaybackContext())
		rr := httptest.NewRecorder()
		handler.HandleStartPlayback(rr, r)
		return rr
	}

	t.Run("rotated-out audio identity degrades", func(t *testing.T) {
		handler, file := newHandler(t)
		index := 0
		req := v3HandlerStartRequest()
		req.AudioTrackID = playback.TrackIDV3(file.ID+9999, "audio", index)
		req.AudioTrackIndex = &index
		if rr := start(t, handler, req); rr.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body = %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("rotated-out subtitle identity degrades", func(t *testing.T) {
		handler, file := newHandler(t)
		index := 0
		req := v3HandlerStartRequest()
		req.SubtitleTrackID = playback.TrackIDV3(file.ID+9999, "subtitle", index)
		req.SubtitleTrackIndex = &index
		if rr := start(t, handler, req); rr.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body = %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("same-file audio id/index mismatch still rejected", func(t *testing.T) {
		handler, file := newHandler(t)
		index := 0
		req := v3HandlerStartRequest()
		req.AudioTrackID = playback.TrackIDV3(file.ID, "audio", index+1)
		req.AudioTrackIndex = &index
		if rr := start(t, handler, req); rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("same-file subtitle id/index mismatch still rejected", func(t *testing.T) {
		handler, file := newHandler(t)
		index := 0
		req := v3HandlerStartRequest()
		req.SubtitleTrackID = playback.TrackIDV3(file.ID, "subtitle", index+1)
		req.SubtitleTrackIndex = &index
		if rr := start(t, handler, req); rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
		}
	})
}
