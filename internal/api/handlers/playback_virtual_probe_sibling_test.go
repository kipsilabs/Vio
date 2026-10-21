package handlers

import (
	"context"
	"testing"
	"time"

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
	probeSiblingPinnedURL      = "http://provider.example/pinned"
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
		{URI: probeSiblingPinnedPath, ProviderURL: probeSiblingPinnedURL, Visible: true, VisibilitySpecified: true, Resolution: "1080p"},
	}
}

// TestProbeChainPreparesPinnedRowWhenDefaultIsNeutral pins the behaviour the
// virtual catalog needs: the item's default is a neutral group row, which the
// listing names no release for, and the pinned release below it is prepared
// instead — exactly once.
func TestProbeChainPreparesPinnedRowWhenDefaultIsNeutral(t *testing.T) {
	reader, _, streams := probeSiblingFixture(t, nil)
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
	if got := prober.last(); got != probeSiblingPinnedURL {
		t.Fatalf("probed %q, want the listing's concrete URL for the pinned row %q", got, probeSiblingPinnedURL)
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
	if got := prober.last(); got != probeSiblingPinnedURL {
		t.Fatalf("probed %q, want the default row's listed URL %q", got, probeSiblingPinnedURL)
	}
}

// TestProbeChainNeverProbesAuthorizedButUnnamedSibling keeps the chain from
// guessing: a row the viewer may see is still not a probe target when the
// just-persisted listing does not name it. The fallback widens the search for
// a candidate, never the rule for what may be probed.
func TestProbeChainNeverProbesAuthorizedButUnnamedSibling(t *testing.T) {
	reader, _, _ := probeSiblingFixture(t, nil)
	prober := &recordingProber{}
	h := &PlaybackHandler{
		ServiceContext:              context.Background(),
		DefaultProbeTarget:          NewVirtualDefaultProbeTarget(reader, reader),
		VirtualPlaybackSourceProber: prober.probe,
	}
	// The listing names a release that is not a row of this item, so the
	// authorized pinned row below the neutral default stays unnamed.
	streams := []VirtualPlaybackStream{{URI: "virtual://movie/probe-chain?result=other", ProviderURL: "http://provider.example/other"}}

	h.ScheduleDefaultFileProbe(probeChainTestContent, probeChainSourceRow(), streams, catalog.AccessFilter{})
	h.probeChainWG.Wait()

	if got := prober.count(); got != 0 {
		t.Fatalf("probe count = %d, want no preparation of an authorized row the listing does not name", got)
	}
}

// TestProbeChainNeverProbesHiddenSiblingListedNowhere keeps the authorization
// boundary and the naming rule independent of each other: a row that is hidden
// from the viewer and named by no listing is never prepared, and a hidden row
// named by the listing is never prepared either.
func TestProbeChainNeverProbesHiddenSiblingListedNowhere(t *testing.T) {
	hidden := &models.MediaFile{
		ID: 343, ContentID: probeChainTestContent, FilePath: "virtual://movie/probe-chain?result=hidden",
		MediaFolderID: probeSiblingHiddenLibrary, VirtualOwnerInstallationID: 5, ProbeSource: "virtual",
	}
	reader, _, _ := probeSiblingFixture(t, []*models.MediaFile{hidden})
	prober := &recordingProber{}
	h := &PlaybackHandler{
		ServiceContext:              context.Background(),
		DefaultProbeTarget:          NewVirtualDefaultProbeTarget(reader, reader),
		VirtualPlaybackSourceProber: prober.probe,
	}
	// The listing names neither the hidden row nor the visible pinned row.
	streams := []VirtualPlaybackStream{{URI: "virtual://movie/probe-chain?result=other", ProviderURL: "http://provider.example/other"}}
	filter := catalog.AccessFilter{AllowedLibraryIDs: []int{probeSiblingVisibleLibrary}}

	h.ScheduleDefaultFileProbe(probeChainTestContent, probeChainSourceRow(), streams, filter)
	h.probeChainWG.Wait()

	if got := prober.count(); got != 0 {
		t.Fatalf("probe count = %d, want no preparation of a hidden row listed nowhere", got)
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
	hiddenURL := probeChainProviderURL(hidden.FilePath)
	streams = append(streams, VirtualPlaybackStream{URI: hidden.FilePath, ProviderURL: hiddenURL})
	filter := catalog.AccessFilter{AllowedLibraryIDs: []int{probeSiblingVisibleLibrary}}

	h.ScheduleDefaultFileProbe(probeChainTestContent, probeChainSourceRow(), streams, filter)
	h.probeChainWG.Wait()

	if got := prober.count(); got != 1 {
		t.Fatalf("probe count = %d, want only the visible pinned row prepared", got)
	}
	for _, url := range prober.urls {
		if url == hiddenURL {
			t.Fatalf("probed the hidden release %q, want no preparation outside the access filter", url)
		}
	}
	if got := prober.last(); got != probeSiblingPinnedURL {
		t.Fatalf("probed %q, want the visible pinned row %q", got, probeSiblingPinnedURL)
	}
}

// TestProbeChainMatchesReleaseAcrossOwnerInstallation reproduces the live
// listing shape that made the chain prepare nothing: the persisted row belongs
// to an installation (owner 1) while every listing entry is provider-side and
// carries owner 0. What the listing names is the release — the concrete
// ?result= candidate id — so the row must be prepared, and the probe must open
// the listing's concrete URL rather than the catalog's virtual:// path.
func TestProbeChainMatchesReleaseAcrossOwnerInstallation(t *testing.T) {
	reader, pinned, _ := probeSiblingFixture(t, nil)
	pinned.VirtualOwnerInstallationID = 1
	streams := []VirtualPlaybackStream{{
		URI: pinned.FilePath, OwnerInstallationID: 0, ProviderURL: "http://provider.example/live",
		Visible: true, VisibilitySpecified: true,
	}}
	prober := &recordingProber{}
	h := &PlaybackHandler{
		ServiceContext:              context.Background(),
		DefaultProbeTarget:          NewVirtualDefaultProbeTarget(reader, reader),
		VirtualPlaybackSourceProber: prober.probe,
	}

	h.ScheduleDefaultFileProbe(probeChainTestContent, probeChainSourceRow(), streams, catalog.AccessFilter{})
	h.probeChainWG.Wait()

	if got := prober.count(); got != 1 {
		t.Fatalf("probe count = %d, want the pinned release prepared despite the owner mismatch", got)
	}
	if got := prober.last(); got != "http://provider.example/live" {
		t.Fatalf("probed %q, want the listing's concrete URL", got)
	}
}

// TestProbeChainMatchesProfileVariantUnderNeutralListing keeps the profile a
// selection qualifier, not a release identity: the provider result id is what
// names the release, so a row pinned under a profile variant matches the
// neutral listing entry carrying the same result id.
func TestProbeChainMatchesProfileVariantUnderNeutralListing(t *testing.T) {
	neutral := &models.MediaFile{
		ID: 351, ContentID: probeChainTestContent, FilePath: probeSiblingNeutralPath,
		MediaFolderID: probeSiblingVisibleLibrary, VirtualOwnerInstallationID: 0, ProbeSource: "virtual_collection",
	}
	pinned := &models.MediaFile{
		ID: 350, ContentID: probeChainTestContent,
		FilePath:      "virtual://movie/probe-chain?profile=1080p&result=c5",
		MediaFolderID: probeSiblingVisibleLibrary, VirtualOwnerInstallationID: 1, ProbeSource: "virtual",
	}
	reader := &probeChainVersionReader{
		files: []*models.MediaFile{neutral, pinned},
		rows:  map[int]*models.MediaFile{neutral.ID: neutral, pinned.ID: pinned},
	}
	streams := []VirtualPlaybackStream{{
		URI: probeSiblingPinnedPath, ProviderURL: "http://provider.example/profile",
		Visible: true, VisibilitySpecified: true,
	}}
	prober := &recordingProber{}
	h := &PlaybackHandler{
		ServiceContext:              context.Background(),
		DefaultProbeTarget:          NewVirtualDefaultProbeTarget(reader, reader),
		VirtualPlaybackSourceProber: prober.probe,
	}

	h.ScheduleDefaultFileProbe(probeChainTestContent, probeChainSourceRow(), streams, catalog.AccessFilter{})
	h.probeChainWG.Wait()

	if got := prober.count(); got != 1 || prober.last() != "http://provider.example/profile" {
		t.Fatalf("probes = %d, last = %q; want the profile variant prepared through the neutral listing entry", got, prober.last())
	}
}

// TestProbeChainNeverProbesHiddenCandidate keeps a visibility-hidden candidate
// out of the chain even when the listing carries it and a row pins it.
func TestProbeChainNeverProbesHiddenCandidate(t *testing.T) {
	reader, pinned, _ := probeSiblingFixture(t, nil)
	streams := []VirtualPlaybackStream{{
		URI: pinned.FilePath, ProviderURL: probeSiblingPinnedURL,
		Visible: false, VisibilitySpecified: true,
	}}
	prober := &recordingProber{}
	h := &PlaybackHandler{
		ServiceContext:              context.Background(),
		DefaultProbeTarget:          NewVirtualDefaultProbeTarget(reader, reader),
		VirtualPlaybackSourceProber: prober.probe,
	}

	h.ScheduleDefaultFileProbe(probeChainTestContent, probeChainSourceRow(), streams, catalog.AccessFilter{})
	h.probeChainWG.Wait()

	if got := prober.count(); got != 0 {
		t.Fatalf("probe count = %d, want no preparation of a hidden candidate", got)
	}
}

// TestProbeChainNeverProbesExpiredCandidate keeps an expired provider URL out
// of the chain: the bytes it named are no longer the ones a play would open.
func TestProbeChainNeverProbesExpiredCandidate(t *testing.T) {
	reader, pinned, _ := probeSiblingFixture(t, nil)
	expired := time.Now().Add(-time.Minute)
	streams := []VirtualPlaybackStream{{
		URI: pinned.FilePath, ProviderURL: probeSiblingPinnedURL,
		Visible: true, VisibilitySpecified: true, ProviderExpiresAt: &expired,
	}}
	prober := &recordingProber{}
	h := &PlaybackHandler{
		ServiceContext:              context.Background(),
		DefaultProbeTarget:          NewVirtualDefaultProbeTarget(reader, reader),
		VirtualPlaybackSourceProber: prober.probe,
	}

	h.ScheduleDefaultFileProbe(probeChainTestContent, probeChainSourceRow(), streams, catalog.AccessFilter{})
	h.probeChainWG.Wait()

	if got := prober.count(); got != 0 {
		t.Fatalf("probe count = %d, want no preparation of an expired candidate", got)
	}
}

// TestProbeChainNeverProbesCandidateWithoutConcreteURL keeps the chain from
// attempting an enumeration it cannot run: the prober opens a real URL, so a
// listing entry with no concrete provider URL names no probed bytes.
func TestProbeChainNeverProbesCandidateWithoutConcreteURL(t *testing.T) {
	reader, pinned, _ := probeSiblingFixture(t, nil)
	streams := []VirtualPlaybackStream{{
		URI: pinned.FilePath, Visible: true, VisibilitySpecified: true,
	}}
	prober := &recordingProber{}
	h := &PlaybackHandler{
		ServiceContext:              context.Background(),
		DefaultProbeTarget:          NewVirtualDefaultProbeTarget(reader, reader),
		VirtualPlaybackSourceProber: prober.probe,
	}

	h.ScheduleDefaultFileProbe(probeChainTestContent, probeChainSourceRow(), streams, catalog.AccessFilter{})
	h.probeChainWG.Wait()

	if got := prober.count(); got != 0 {
		t.Fatalf("probe count = %d, want no preparation without a concrete provider URL", got)
	}
}
