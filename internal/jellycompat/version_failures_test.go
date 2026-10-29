package jellycompat

import (
	"net/http"
	"testing"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/config"
)

// TestDetailMediaSourcesExcludeFailedVersions proves a durably failed row is
// absent from the detail version list, mirroring the native version picker.
func TestDetailMediaSourcesExcludeFailedVersions(t *testing.T) {
	m := newMapper(NewResourceIDCodec(), &config.Config{})
	detail := itemDetailToUpstream(&catalog.ItemDetail{
		ContentID: "movie-1",
		Type:      "movie",
		Versions: []catalog.FileVersion{
			{FileID: 42, Container: "mkv", Duration: 1200},
			{FileID: 43, Container: "mkv", Duration: 1200, Failed: true},
		},
	})

	dto := m.itemFromDetailWithFields(detail, false, nil, map[string]bool{"mediasources": true})
	if len(dto.MediaSources) != 1 {
		t.Fatalf("MediaSources = %d, want 1 after dropping the failed row", len(dto.MediaSources))
	}
	wantID := m.codec.EncodeIntID(EncodedIDMediaSource, 42)
	if !mediaSourceIDsEqual(dto.MediaSources[0].ID, wantID) {
		t.Fatalf("MediaSources[0].ID = %q, want %q", dto.MediaSources[0].ID, wantID)
	}
}

// TestDetailMediaSourcesKeepAllFailedList proves an all-failed item still lists
// its versions rather than returning an empty version menu.
func TestDetailMediaSourcesKeepAllFailedList(t *testing.T) {
	m := newMapper(NewResourceIDCodec(), &config.Config{})
	detail := itemDetailToUpstream(&catalog.ItemDetail{
		ContentID: "movie-1",
		Type:      "movie",
		Versions: []catalog.FileVersion{
			{FileID: 42, Container: "mkv", Duration: 1200, Failed: true},
			{FileID: 43, Container: "mkv", Duration: 1200, Failed: true},
		},
	})

	dto := m.itemFromDetailWithFields(detail, false, nil, map[string]bool{"mediasources": true})
	if len(dto.MediaSources) != 2 {
		t.Fatalf("MediaSources = %d, want the full all-failed list of 2", len(dto.MediaSources))
	}
}

// TestDetailMediaSourcesKeepSelectedFailedVersion proves the version a client
// addressed stays in the detail list even after a failed verdict.
func TestDetailMediaSourcesKeepSelectedFailedVersion(t *testing.T) {
	m := newMapper(NewResourceIDCodec(), &config.Config{})
	detail := itemDetailToUpstream(&catalog.ItemDetail{
		ContentID: "movie-1",
		Type:      "movie",
		Versions: []catalog.FileVersion{
			{FileID: 42, Container: "mkv", Duration: 1200},
			{FileID: 43, Container: "mkv", Duration: 1200, Failed: true},
		},
	})

	dto := m.itemFromDetailWithFieldsSelected(detail, 43, false, nil, map[string]bool{"mediasources": true})
	if len(dto.MediaSources) != 2 {
		t.Fatalf("MediaSources = %d, want the selected failed row kept alongside the live one", len(dto.MediaSources))
	}
}

// TestPlaybackInfoExcludesFailedVersion proves a dead row is not offered as a
// media source when the client has not selected one.
func TestPlaybackInfoExcludesFailedVersion(t *testing.T) {
	handler, content := newTwoVersionPlaybackInfoHandler(t)
	content.detail.Versions[1].Failed = true

	rec := servePlaybackInfo(handler, handler.codec.EncodeStringID(EncodedIDItem, "movie-1"), `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	resp := decodePlaybackInfo(t, rec.Body.Bytes())
	if len(resp.MediaSources) != 1 {
		t.Fatalf("MediaSources = %d, want 1 after dropping the failed row", len(resp.MediaSources))
	}
	wantID := handler.codec.EncodeIntID(EncodedIDMediaSource, 42)
	if !mediaSourceIDsEqual(resp.MediaSources[0].ID, wantID) {
		t.Fatalf("MediaSources[0].ID = %q, want %q", resp.MediaSources[0].ID, wantID)
	}
}

// TestPlaybackInfoKeepsSelectedFailedVersion proves the active version survives
// a failed verdict so playback never loses its row mid-session.
func TestPlaybackInfoKeepsSelectedFailedVersion(t *testing.T) {
	handler, content := newTwoVersionPlaybackInfoHandler(t)
	content.detail.Versions[1].Failed = true
	selectedID := handler.codec.EncodeIntID(EncodedIDMediaSource, 43)

	rec := servePlaybackInfo(handler, handler.codec.EncodeStringID(EncodedIDItem, "movie-1"), `{"MediaSourceId":"`+selectedID+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	resp := decodePlaybackInfo(t, rec.Body.Bytes())
	if len(resp.MediaSources) != 1 || !mediaSourceIDsEqual(resp.MediaSources[0].ID, selectedID) {
		t.Fatalf("MediaSources = %+v, want the selected failed source %s", resp.MediaSources, selectedID)
	}
}

// TestPlaybackInfoKeepsAllFailedList proves an all-failed item answers with the
// full source list instead of an empty PlaybackInfo.
func TestPlaybackInfoKeepsAllFailedList(t *testing.T) {
	handler, content := newTwoVersionPlaybackInfoHandler(t)
	content.detail.Versions[0].Failed = true
	content.detail.Versions[1].Failed = true

	rec := servePlaybackInfo(handler, handler.codec.EncodeStringID(EncodedIDItem, "movie-1"), `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	resp := decodePlaybackInfo(t, rec.Body.Bytes())
	if len(resp.MediaSources) != 2 {
		t.Fatalf("MediaSources = %d, want the full all-failed list of 2", len(resp.MediaSources))
	}
}
