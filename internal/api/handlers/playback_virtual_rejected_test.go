package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/plugins"
)

type staticDeviceCapabilities struct {
	device plugins.DeviceCapabilities
}

func (s staticDeviceCapabilities) DeviceCapabilitiesFor(context.Context, string, string) (plugins.DeviceCapabilities, bool) {
	return s.device, true
}

// TestFinalizeVirtualCandidateOrderKeepsRejectedLastAfterDeviceRank proves the
// rejected-last partition is the final step: the rejected candidate is an exact
// device match and would win a pure device rank, but an accepted candidate must
// still come first. A rejected candidate remains last-resort selectable.
func TestFinalizeVirtualCandidateOrderKeepsRejectedLastAfterDeviceRank(t *testing.T) {
	rejected := VirtualPlaybackStream{
		URI: "virtual://movie/tt1?result=rejected", Resolution: "1080p",
		CodecVideo: "h264", CodecAudio: "aac", Container: "mp4", Rejected: true,
	}
	accepted := VirtualPlaybackStream{
		URI: "virtual://movie/tt1?result=accepted", Resolution: "720p",
		CodecVideo: "hevc", CodecAudio: "opus", Container: "mkv",
	}
	h := &PlaybackHandler{
		DeviceCapabilitySource: staticDeviceCapabilities{device: plugins.DeviceCapabilities{
			CodecsVideo:   []string{"h264"},
			CodecsAudio:   []string{"aac"},
			Containers:    []string{"mp4"},
			MaxResolution: "1080p",
		}},
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)

	out := h.finalizeVirtualCandidateOrder(request, []VirtualPlaybackStream{rejected, accepted}, "auto", 0)
	if len(out) != 2 {
		t.Fatalf("ordered streams = %d, want 2", len(out))
	}
	if out[0].Rejected {
		t.Fatalf("rejected candidate sorted first after device rank: %+v", out)
	}
	if !out[1].Rejected {
		t.Fatalf("accepted candidate was not first: %+v", out)
	}
}

// TestPartitionVirtualStreamsAcceptedFirstIsStable pins the partition's
// stability and its all-rejected/all-accepted no-op behavior.
func TestPartitionVirtualStreamsAcceptedFirstIsStable(t *testing.T) {
	all := []VirtualPlaybackStream{
		{URI: "a", Rejected: true},
		{URI: "b"},
		{URI: "c", Rejected: true},
		{URI: "d"},
	}
	ordered := partitionVirtualStreamsAcceptedFirst(all)
	want := []string{"b", "d", "a", "c"}
	for i, stream := range ordered {
		if stream.URI != want[i] {
			t.Fatalf("order = %v, want %v", ordered, want)
		}
	}

	allRejected := []VirtualPlaybackStream{{URI: "a", Rejected: true}, {URI: "b", Rejected: true}}
	if got := partitionVirtualStreamsAcceptedFirst(allRejected); len(got) != 2 || got[0].URI != "a" {
		t.Fatalf("all-rejected partition changed the order: %+v", got)
	}
}

// TestFinalizeVirtualCandidateOrderSingleRejectedIsAZeroAllocNoOp proves a lone
// rejected candidate is returned unchanged: last-resort selectable, not
// reordered behind a non-existent accepted stream.
func TestFinalizeVirtualCandidateOrderSingleRejectedIsAZeroAllocNoOp(t *testing.T) {
	h := &PlaybackHandler{}
	only := VirtualPlaybackStream{URI: "virtual://movie/tt1?result=only", Rejected: true}
	out := h.finalizeVirtualCandidateOrder(httptest.NewRequest(http.MethodGet, "/", nil), []VirtualPlaybackStream{only}, "auto", 0)
	if len(out) != 1 || out[0].URI != only.URI || !out[0].Rejected {
		t.Fatalf("single rejected candidate changed: %+v", out)
	}
}

func TestServerOrderBypassesHandlerPreferences(t *testing.T) {
	const neutral = "virtual://movie/tt-server-order"
	stub, probed := probedStartPreferenceRows(neutral, "stub", "probed")
	streams := []VirtualPlaybackStream{
		{URI: stub.FilePath, Resolution: "720p", CodecVideo: "hevc", PreserveProviderOrder: true},
		{URI: probed.FilePath, Resolution: "2160p", CodecVideo: "h264", PreserveProviderOrder: true},
	}
	h := &PlaybackHandler{
		DeviceCapabilitySource: staticDeviceCapabilities{device: plugins.DeviceCapabilities{CodecsVideo: []string{"h264"}, MaxResolution: "2160p"}},
		VirtualFileLookup: func(_ context.Context, path string) (*models.MediaFile, error) {
			if path == probed.FilePath {
				return probed, nil
			}
			return nil, nil
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ordered := h.finalizeVirtualCandidateOrder(req, streams, "2160p", 0)
	ordered = h.preferProbedVirtualCandidates(context.Background(), ordered, stub, 0)
	ordered = h.applyVirtualStickyPin("server-order", probed.FilePath, ordered)
	if ordered[0].URI != stub.FilePath {
		t.Fatal("handler preferences replaced the provider's first stream")
	}
}
