package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

// probedStartPreferenceRows builds the two catalog rows of one release: a bare
// size-unknown placeholder carrying a persisted ?result= pick and a fully
// probed copy. They share the provider-neutral path (same content, result
// stripped) and belong to the same profile/owner, exactly the prod shape where
// a start bound the size-0 stub and a later replan had to rotate.
func probedStartPreferenceRows(neutralURI, placeholderID, probedID string) (*models.MediaFile, *models.MediaFile) {
	placeholderURI := neutralURI + "?result=" + placeholderID
	probedURI := neutralURI + "?result=" + probedID
	placeholder := &models.MediaFile{
		ID:                         901,
		ContentID:                  "movie-start-prefer",
		FilePath:                   placeholderURI,
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
	}
	stamp := time.Now().Add(-time.Hour)
	probed := &models.MediaFile{
		ID:                         902,
		ContentID:                  "movie-start-prefer",
		FilePath:                   probedURI,
		Container:                  "mkv",
		CodecVideo:                 "h264",
		CodecAudio:                 "aac",
		Resolution:                 "1080p",
		Bitrate:                    8_000,
		ProbeUpdatedAt:             &stamp,
		VirtualOwnerInstallationID: 5,
		VideoTracks:                []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080, FrameRate: "24000/1001", BitDepth: 8, Bitrate: 8_000}},
		AudioTracks:                []models.AudioTrack{{Codec: "aac", Channels: 2, Language: "eng", Default: true}},
	}
	return placeholder, probed
}

// TestResolveVirtualStartPrefersProbedCopyOverPersistedPlaceholder pins issue
// #244 item 1. A start whose requested row is an unprobed placeholder that still
// carries a persisted ?result= pick must not bind the size-0 stub when a fully
// probed copy of the same profile/content exists: the placeholder sits at index
// 0 (it is the requested row), yet the probed sibling must be promoted ahead of
// it before the candidate cap, so the session binds known-good bytes instead of
// guaranteeing a later rotation. Version-list display ordering is a catalog
// concern and is untouched by this handler-side preference.
func TestResolveVirtualStartPrefersProbedCopyOverPersistedPlaceholder(t *testing.T) {
	const neutralURI = "virtual://movie/tt-start-prefer"
	placeholder, probed := probedStartPreferenceRows(neutralURI, "stub", "probed")
	placeholderURI := placeholder.FilePath
	probedURI := probed.FilePath

	var listed []string
	h := &PlaybackHandler{
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(
			func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
				listed = append(listed, path)
				return "https://93.184.216.34/stream?path=" + path, nil
			}),
		// The provider lists the placeholder first, then the probed copy. Both
		// share the neutral path, so filtering keeps the probed sibling while the
		// requested placeholder is matched at index 0.
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(
			func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
				return []VirtualPlaybackStream{
					{ID: "stub", URI: placeholderURI, Resolution: "2160p", CodecVideo: "hevc", CodecAudio: "eac3", Container: "mkv"},
					{ID: "probed", URI: probedURI, Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac", Container: "mkv"},
				}, nil
			}),
		// Only the probed row carries a probe stamp plus planner-grade evidence;
		// the placeholder has no catalog row at all.
		VirtualFileLookup: func(_ context.Context, path string) (*models.MediaFile, error) {
			if path == probedURI {
				row := *probed
				return &row, nil
			}
			return nil, nil
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	resolved, err := h.resolveVirtualPlaybackSource(req, placeholder, "profile-1", true, nil, "", "auto", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if resolved.URI != probedURI {
		t.Fatalf("resolved candidate = %q, want the probed copy %q (placeholder must not bind first)", resolved.URI, probedURI)
	}
	if got := virtualResultCandidateID(resolved.URI); got != "probed" {
		t.Fatalf("resolved candidate id = %q, want %q", got, "probed")
	}
}

// TestPreferProbedVirtualCandidatesKeepsRejectedBehindAccepted pins the
// compatibility half of item 1: the preference only reorders within each
// accepted/rejected group. A rejected probed row must never jump an accepted
// unprobed stub, so custom-format compatibility still wins the top slot.
func TestPreferProbedVirtualCandidatesOrderingRespectsGroups(t *testing.T) {
	const neutralURI = "virtual://movie/tt-prefer-order"
	placeholder, probed := probedStartPreferenceRows(neutralURI, "stub", "probed")
	acceptedStub := VirtualPlaybackStream{ID: "stub-a", URI: placeholder.FilePath}
	acceptedProbed := VirtualPlaybackStream{ID: "probed-b", URI: probed.FilePath}
	rejectedProbed := VirtualPlaybackStream{ID: "probed-c", URI: neutralURI + "?result=probed-c", Rejected: true}
	rejectedStub := VirtualPlaybackStream{ID: "stub-d", URI: neutralURI + "?result=stub-d", Rejected: true}

	h := &PlaybackHandler{
		VirtualFileLookup: func(_ context.Context, path string) (*models.MediaFile, error) {
			// Every concrete probed-* candidate is a probed copy; the stub-*
			// candidates have no probed row.
			if virtualResultCandidateID(path) == "probed" || virtualResultCandidateID(path) == "probed-b" || virtualResultCandidateID(path) == "probed-c" {
				row := *probed
				row.FilePath = path
				return &row, nil
			}
			return nil, nil
		},
	}
	ordered := h.preferProbedVirtualCandidates(context.Background(),
		[]VirtualPlaybackStream{acceptedStub, acceptedProbed, rejectedProbed, rejectedStub},
		placeholder, placeholder.VirtualOwnerInstallationID)

	want := []string{"probed-b", "stub-a", "probed-c", "stub-d"}
	if len(ordered) != len(want) {
		t.Fatalf("ordered %d candidates, want %d: %#v", len(ordered), len(want), ordered)
	}
	for i, id := range want {
		if ordered[i].ID != id {
			t.Fatalf("ordered[%d] = %q, want %q (got order %#v)", i, ordered[i].ID, id, ordered)
		}
	}
}
