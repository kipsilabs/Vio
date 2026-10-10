package handlers

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// TestOfferedReleaseInventoryPopulatesMenusWithoutMovingTransport pins the
// mid-session churn fix. A collection-owned row is pinned to a candidate the
// provider later drops; the transport keeps playing the already-open relay. The
// row cannot adopt the current listing's different release, so the evidence
// write is (correctly) refused, but the live session must still receive the
// listing's probed tracks as a display-only inventory_updated. Its effective
// identity is the row the session is bound to, never the offered release, and
// the session binding is untouched.
//
// Red before the fix: the refusal returned without publishing anything, so the
// menus stayed empty until a stop/resume.
func TestOfferedReleaseInventoryPopulatesMenusWithoutMovingTransport(t *testing.T) {
	const (
		neutral    = "virtual://movie/tt-offered-release"
		pinnedURI  = neutral + "?result=8376"
		offeredURI = neutral + "?result=7398"
		content    = "movie-offered-release"
		rowID      = 941
	)
	pinnedRow := &models.MediaFile{
		ID: rowID, ContentID: content, FilePath: pinnedURI,
		MediaFolderID: 9, VirtualOwnerInstallationID: 5,
		ProbeSource: virtualCollectionProbeSource,
	}
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", rowID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := sessionMgr.SetVirtualSource(session.ID, pinnedURI, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	if err := sessionMgr.SetRealtimeConnection(session.ID, true); err != nil {
		t.Fatalf("SetRealtimeConnection: %v", err)
	}

	h := NewPlaybackHandler(sessionMgr, pathLookupFileResolver{files: map[int]*models.MediaFile{rowID: pinnedRow}})
	h.RealtimeHub = playback.NewRealtimeHub()
	conn := &sourceCommittedTestConn{}
	registration := h.RealtimeHub.Register(session.ID, conn)
	if registration == nil {
		t.Fatal("expected a realtime registration")
	}
	defer h.RealtimeHub.Unregister(registration)
	saver := &persistRotationEvidenceSaver{result: VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}}
	h.VirtualFileMetadataSaver = saver.save
	h.VirtualFileSaver = func(context.Context, models.VirtualFilePersistArgs) (int64, error) { return 1, nil }

	h.persistVirtualProbeEvidence(context.Background(), pinnedRow, offeredURI, probedDualAudioFile(), true, false)
	h.stopVirtualEvidence()

	if calls := saver.recorded(); len(calls) != 0 {
		t.Fatalf("collection cross-release evidence still wrote %+v, want no catalog write", calls)
	}
	if len(conn.messages) != 1 {
		t.Fatalf("delivered %d events, want 1 display-only offered-release inventory_updated", len(conn.messages))
	}
	event, ok := conn.messages[0].(playback.EventEnvelope)
	if !ok {
		t.Fatalf("message type = %T, want playback.EventEnvelope", conn.messages[0])
	}
	if event.Name != playback.RealtimeEventInventoryUpdated {
		t.Fatalf("event name = %q, want %q", event.Name, playback.RealtimeEventInventoryUpdated)
	}
	var payload playback.InventoryUpdatedPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.OfferedVirtualURI != offeredURI {
		t.Fatalf("offered uri = %q, want the current listing's %q", payload.OfferedVirtualURI, offeredURI)
	}
	if payload.EffectiveVirtualURI != "" {
		t.Fatalf("effective uri = %q, want empty: the offer must never name the bound candidate", payload.EffectiveVirtualURI)
	}
	if payload.EffectiveMediaFileID != rowID {
		t.Fatalf("effective media file = %d, want the row the session is bound to %d", payload.EffectiveMediaFileID, rowID)
	}
	if payload.InventoryStatus != string(ProbeProvenanceVerified) {
		t.Fatalf("inventory status = %q, want verified", payload.InventoryStatus)
	}
	if len(payload.AudioTracks) != 2 {
		t.Fatalf("payload audio tracks = %#v, want the offered listing's probed tracks", payload.AudioTracks)
	}
	if len(payload.SubtitleInventory) == 0 {
		t.Fatalf("payload subtitle inventory = %#v, want the offered listing's probed subtitle", payload.SubtitleInventory)
	}

	// The transport never moves: the session is still bound to the vanished pin.
	live, err := sessionMgr.GetSession(session.ID)
	if err != nil || live == nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualSourceURI != pinnedURI {
		t.Fatalf("session binding = %q, want the untouched pinned candidate %q", live.VirtualSourceURI, pinnedURI)
	}
	if live.MediaFileID != rowID {
		t.Fatalf("session media file = %d, want the bound row %d", live.MediaFileID, rowID)
	}
}
