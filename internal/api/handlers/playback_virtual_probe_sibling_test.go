package handlers

import (
	"context"
	"testing"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
)

// The pre-play chain must prepare a version the just-persisted listing names,
// even when the item's top version is a neutral group row. These tests drive
// the production target (the catalog's access-filtered version list) so the
// authorization rules are the real ones: a row the viewer may not see, or a
// row the listing does not name, is never probed.

const (
	probeSiblingVisibleLibrary = 77
	probeSiblingHiddenLibrary  = 78
	probeSiblingNeutralPath    = "virtual://movie/probe-chain"
	probeSiblingPinnedPath     = "virtual://movie/probe-chain?result=c5"
)

// probeSiblingFixture builds an item whose top version is the neutral group
// row and whose second version is a pinned release, plus the listing that
// names only the release.
func probeSiblingFixture(t *testing.T, extra []*models.MediaFile) (*probeChainVersionReader, *models.MediaFile, []VirtualPlaybackStream) {
	t.Helper()
	neutral := &models.MediaFile{
		ID: 340, ContentID: probeChainTestContent, FilePath: probeSiblingNeutralPath,
		MediaFolderID: probeSiblingVisibleLibrary, VirtualOwnerInstallationID: 5, ProbeSource: "virtual_collection",
	}
	pinned := &models.MediaFile{
		ID: 341, ContentID: probeChainTestContent, FilePath: probeSiblingPinnedPath,
		MediaFolderID: probeSiblingVisibleLibrary, VirtualOwnerInstallationID: 5, ProbeSource: "virtual",
	}
	files := append([]*models.MediaFile{neutral, pinned}, extra...)
	rows := map[int]*models.MediaFile{neutral.ID: neutral, pinned.ID: pinned}
	for _, file := range extra {
		rows[file.ID] = file
	}
	reader := &probeChainVersionReader{files: files, rows: rows}
	return reader, pinned, []VirtualPlaybackStream{
		{URI: probeSiblingPinnedPath, OwnerInstallationID: 5, Resolution: "1080p"},
	}
}

// TestProbeChainPreparesPinnedRowWhenDefaultIsNeutral pins the behaviour the
// virtual catalog needs: the item's default is a neutral group row, which the
// listing names no release for, and the pinned release below it is prepared
// instead — exactly once.
func TestProbeChainPreparesPinnedRowWhenDefaultIsNeutral(t *testing.T) {
	reader, pinned, streams := probeSiblingFixture(t, nil)
	prober := &recordingProber{}
	h := &PlaybackHandler{
		ServiceContext:              context.Background(),
		DefaultProbeTarget:          NewVirtualDefaultProbeTarget(reader, reader),
		VirtualPlaybackSourceProber: prober.probe,
	}
	h.ScheduleDefaultFileProbe(probeChainTestContent, probeChainSourceRow(), streams, catalog.AccessFilter{})
	h.probeChainWG.Wait()

	if got := prober.count(); got != 1 {
		t.Fatalf("probe count = %d, want exactly one preparation of the pinned row", got)
	}
	if got := prober.last(); got != pinned.FilePath {
		t.Fatalf("probed %q, want the pinned row %q the listing names", got, pinned.FilePath)
	}
}

// TestProbeChainProbesDefaultWhenDefaultIsProbeable keeps the existing
// behaviour: a default that the listing names is probed, and no other row is.
func TestProbeChainProbesDefaultWhenDefaultIsProbeable(t *testing.T) {
	reader, _, streams := probeSiblingFixture(t, nil)
	prober := &recordingProber{}
	h := &PlaybackHandler{
		ServiceContext:              context.Background(),
		DefaultProbeTarget:          NewVirtualDefaultProbeTarget(reader, reader),
		VirtualPlaybackSourceProber: prober.probe,
	}
	// The default is the pinned row here: the neutral group row sorts after it.
	reader.files = []*models.MediaFile{reader.rows[341], reader.rows[340]}

	h.ScheduleDefaultFileProbe(probeChainTestContent, probeChainSourceRow(), streams, catalog.AccessFilter{})
	h.probeChainWG.Wait()

	if got := prober.count(); got != 1 {
		t.Fatalf("probe count = %d, want one preparation of the default row", got)
	}
	if got := prober.last(); got != probeSiblingPinnedPath {
		t.Fatalf("probed %q, want the default row %q", got, probeSiblingPinnedPath)
	}
}

// TestProbeChainProbesNothingWhenListingNamesNoCandidate keeps the chain from
// guessing: a row the listing does not name is not a probe target, however
// authorized it is.
func TestProbeChainProbesNothingWhenListingNamesNoCandidate(t *testing.T) {
	reader, _, _ := probeSiblingFixture(t, nil)
	prober := &recordingProber{}
	h := &PlaybackHandler{
		ServiceContext:              context.Background(),
		DefaultProbeTarget:          NewVirtualDefaultProbeTarget(reader, reader),
		VirtualPlaybackSourceProber: prober.probe,
	}
	// The listing names a release that is not a row of this item.
	streams := []VirtualPlaybackStream{{URI: "virtual://movie/probe-chain?result=other", OwnerInstallationID: 5}}

	h.ScheduleDefaultFileProbe(probeChainTestContent, probeChainSourceRow(), streams, catalog.AccessFilter{})
	h.probeChainWG.Wait()

	if got := prober.count(); got != 0 {
		t.Fatalf("probe count = %d, want no preparation of a row the listing does not name", got)
	}
}

// TestProbeChainNeverProbesRowOutsideTheAccessFilter keeps the authorization
// boundary: a hidden library's release, even when the listing names it, is
// never prepared.
func TestProbeChainNeverProbesRowOutsideTheAccessFilter(t *testing.T) {
	hidden := &models.MediaFile{
		ID: 342, ContentID: probeChainTestContent, FilePath: "virtual://movie/probe-chain?result=hidden",
		MediaFolderID: probeSiblingHiddenLibrary, VirtualOwnerInstallationID: 5, ProbeSource: "virtual",
	}
	reader, _, streams := probeSiblingFixture(t, []*models.MediaFile{hidden})
	prober := &recordingProber{}
	h := &PlaybackHandler{
		ServiceContext:              context.Background(),
		DefaultProbeTarget:          NewVirtualDefaultProbeTarget(reader, reader),
		VirtualPlaybackSourceProber: prober.probe,
	}
	// The listing names the hidden release too, so only the access filter can
	// keep it out of the chain.
	streams = append(streams, VirtualPlaybackStream{URI: hidden.FilePath, OwnerInstallationID: 5})
	filter := catalog.AccessFilter{AllowedLibraryIDs: []int{probeSiblingVisibleLibrary}}

	h.ScheduleDefaultFileProbe(probeChainTestContent, probeChainSourceRow(), streams, filter)
	h.probeChainWG.Wait()

	for _, url := range prober.urls {
		if url == hidden.FilePath {
			t.Fatalf("probed the hidden row %q, want no preparation outside the access filter", url)
		}
	}
}
