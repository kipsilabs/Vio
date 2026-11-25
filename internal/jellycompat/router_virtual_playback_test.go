package jellycompat

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/userstore"
)

// The virtual playback handler is constructed inside NewRouter, so a dropped
// assignment is invisible to tests that build PlaybackHandler directly. These
// sentinels make every virtual dependency observable after NewRouter returns.

type sentinelVirtualMediaResolver struct{}

func (*sentinelVirtualMediaResolver) ResolveVirtualMedia(context.Context, string, int, int, string) (string, error) {
	return "sentinel-resolver", nil
}

type sentinelVirtualMediaRefreshResolver struct{}

func (*sentinelVirtualMediaRefreshResolver) RefreshVirtualMedia(context.Context, string, int, int, string) (string, error) {
	return "sentinel-refresh", nil
}

type sentinelVirtualMediaDetailedResolver struct{}

func (*sentinelVirtualMediaDetailedResolver) ResolveVirtualMediaDetailed(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
	return ResolvedVirtualMedia{URL: "sentinel-detailed"}, nil
}

type sentinelVirtualPlaybackStreamLister struct{}

func (*sentinelVirtualPlaybackStreamLister) ListVirtualPlaybackStreams(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
	return nil, nil
}

type sentinelRemoteStreamRelay struct{}

func (*sentinelRemoteStreamRelay) Proxy(http.ResponseWriter, *http.Request, string) error {
	return nil
}

func (*sentinelRemoteStreamRelay) ProxyInsecure(http.ResponseWriter, *http.Request, string) error {
	return nil
}

func (*sentinelRemoteStreamRelay) Register(context.Context, string) (string, func(), error) {
	return "sentinel-relay", func() {}, nil
}

func sameFuncValue(a, b any) bool {
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}

func TestRouterWiresVirtualPlaybackDependencies(t *testing.T) {
	resolver := &sentinelVirtualMediaResolver{}
	refreshResolver := &sentinelVirtualMediaRefreshResolver{}
	detailedResolver := &sentinelVirtualMediaDetailedResolver{}
	streamLister := &sentinelVirtualPlaybackStreamLister{}
	relay := &sentinelRemoteStreamRelay{}
	prober := VirtualSourceProber(func(context.Context, string, *models.MediaFile) (*models.MediaFile, error) {
		return nil, nil
	})
	proberWithHeaders := VirtualSourceProberWithHeaders(func(context.Context, string, *models.MediaFile, map[string]string) (*models.MediaFile, error) {
		return nil, nil
	})
	fileSaver := VirtualFileSaver(func(context.Context, models.VirtualFilePersistArgs) (int64, error) {
		return 1, nil
	})
	metadataSaver := VirtualFileMetadataSaver(func(context.Context, models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
		return VirtualFileMetadataUpdateResult{}, nil
	})
	candidateLookup := VirtualCandidateFileLookup(func(context.Context, string, string, string, int) (*models.MediaFile, error) {
		return nil, nil
	})
	allowInsecure := func(int) bool { return true }
	allowPrivate := func(int) bool { return false }

	deps := Dependencies{
		Config:                         &config.Config{},
		VirtualMediaResolver:           resolver,
		VirtualMediaRefreshResolver:    refreshResolver,
		VirtualMediaDetailedResolver:   detailedResolver,
		VirtualPlaybackStreamLister:    streamLister,
		VirtualSourceProber:            prober,
		VirtualSourceProberWithHeaders: proberWithHeaders,
		VirtualFileSaver:               fileSaver,
		VirtualFileMetadataSaver:       metadataSaver,
		VirtualCandidateFileLookup:     candidateLookup,
		RemoteStreamRelay:              relay,
		AllowInsecureVirtual:           allowInsecure,
		AllowPrivateStreams:            allowPrivate,
	}

	original := newPlaybackHandler
	var captured *PlaybackHandler
	newPlaybackHandler = func(
		cfg *config.Config,
		content ContentService,
		codec *ResourceIDCodec,
		deviceProfiles *DeviceProfileStore,
		playbackStore CompatPlaybackStore,
		sessionMgr SessionManagerInterface,
		fileResolver FilePathResolver,
		storeProvider userstore.UserStoreProvider,
	) *PlaybackHandler {
		captured = NewPlaybackHandler(cfg, content, codec, deviceProfiles, playbackStore, sessionMgr, fileResolver, storeProvider)
		return captured
	}
	t.Cleanup(func() { newPlaybackHandler = original })

	NewRouter(deps)

	if captured == nil {
		t.Fatal("NewRouter did not construct a playback handler")
	}
	if captured.VirtualMediaResolver != resolver {
		t.Error("VirtualMediaResolver was not propagated to the playback handler")
	}
	if captured.VirtualMediaRefreshResolver != refreshResolver {
		t.Error("VirtualMediaRefreshResolver was not propagated to the playback handler")
	}
	if captured.VirtualMediaDetailedResolver != detailedResolver {
		t.Error("VirtualMediaDetailedResolver was not propagated to the playback handler")
	}
	if captured.VirtualPlaybackStreamLister != streamLister {
		t.Error("VirtualPlaybackStreamLister was not propagated to the playback handler")
	}
	if !sameFuncValue(captured.VirtualSourceProber, prober) {
		t.Error("VirtualSourceProber was not propagated to the playback handler")
	}
	if !sameFuncValue(captured.VirtualSourceProberWithHeaders, proberWithHeaders) {
		t.Error("VirtualSourceProberWithHeaders was not propagated to the playback handler")
	}
	if !sameFuncValue(captured.VirtualFileSaver, fileSaver) {
		t.Error("VirtualFileSaver was not propagated to the playback handler")
	}
	if !sameFuncValue(captured.VirtualFileMetadataSaver, metadataSaver) {
		t.Error("VirtualFileMetadataSaver was not propagated to the playback handler")
	}
	if !sameFuncValue(captured.VirtualCandidateFileLookup, candidateLookup) {
		t.Error("VirtualCandidateFileLookup was not propagated to the playback handler")
	}
	if captured.RemoteStreamRelay != relay {
		t.Error("RemoteStreamRelay was not propagated to the playback handler")
	}
	if !sameFuncValue(captured.AllowInsecureVirtual, allowInsecure) {
		t.Error("AllowInsecureVirtual was not propagated to the playback handler")
	}
	if !sameFuncValue(captured.AllowPrivateStreams, allowPrivate) {
		t.Error("AllowPrivateStreams was not propagated to the playback handler")
	}
}
