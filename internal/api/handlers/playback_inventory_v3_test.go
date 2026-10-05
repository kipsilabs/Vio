package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/auth"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

func TestHandleGetPlaybackInventoryV3(t *testing.T) {
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", 100, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	probedAt := time.Now()
	file := &models.MediaFile{
		ID:             100,
		ContentID:      "movie-inv-test",
		FilePath:       "/media/test.mkv",
		ProbeUpdatedAt: &probedAt,
		AudioTracks: []models.AudioTrack{
			{Index: 1, Codec: "aac", Channels: 2, Language: "eng", Default: true},
			{Index: 2, Codec: "ac3", Channels: 6, Language: "fre"},
		},
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 3, Codec: "subrip", Language: "eng", Default: true},
		},
	}

	h := NewPlaybackHandler(sessionMgr, testPlaybackFileResolver{file: file})

	// Unauthenticated request -> 401
	unauthReq := httptest.NewRequest(http.MethodGet, "/api/v2/playback/"+session.ID+"/inventory", nil)
	rr := httptest.NewRecorder()
	h.HandleGetPlaybackInventoryV3(rr, withPlaybackRouteParam(unauthReq, "session_id", session.ID))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", rr.Code)
	}

	// Missing session -> 404
	authCtx := apimw.SetClaims(context.Background(), &auth.Claims{UserID: 1, Role: "user", TokenType: auth.TokenTypeAccess})
	authCtx = apimw.SetProfileID(authCtx, "profile-1")
	missingReq := httptest.NewRequest(http.MethodGet, "/api/v2/playback/missing-session/inventory", nil).WithContext(authCtx)
	rr = httptest.NewRecorder()
	h.HandleGetPlaybackInventoryV3(rr, withPlaybackRouteParam(missingReq, "session_id", "missing-session"))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("missing session status = %d, want 404", rr.Code)
	}

	// Authorized request -> 200 with ETag
	req := httptest.NewRequest(http.MethodGet, "/api/v2/playback/"+session.ID+"/inventory", nil).WithContext(authCtx)
	rr = httptest.NewRecorder()
	h.HandleGetPlaybackInventoryV3(rr, withPlaybackRouteParam(req, "session_id", session.ID))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	etag := rr.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag header")
	}

	var inv playback.PlaybackInventoryV3
	if err := json.Unmarshal(rr.Body.Bytes(), &inv); err != nil {
		t.Fatalf("unmarshal inventory: %v", err)
	}
	if inv.SessionID != session.ID {
		t.Errorf("session ID = %q, want %q", inv.SessionID, session.ID)
	}
	if inv.InventoryStatus != "verified" {
		t.Errorf("inventory status = %q, want verified", inv.InventoryStatus)
	}
	if len(inv.AudioTracks) != 2 {
		t.Fatalf("audio tracks = %d, want 2", len(inv.AudioTracks))
	}
	if len(inv.SubtitleInventory) != 1 {
		t.Fatalf("subtitle inventory = %d, want 1", len(inv.SubtitleInventory))
	}

	// Conditional request matching ETag -> 304 Not Modified
	condReq := httptest.NewRequest(http.MethodGet, "/api/v2/playback/"+session.ID+"/inventory", nil).WithContext(authCtx)
	condReq.Header.Set("If-None-Match", etag)
	rrCond := httptest.NewRecorder()
	h.HandleGetPlaybackInventoryV3(rrCond, withPlaybackRouteParam(condReq, "session_id", session.ID))
	if rrCond.Code != http.StatusNotModified {
		t.Fatalf("conditional status = %d, want 304", rrCond.Code)
	}
	if rrCond.Body.Len() != 0 {
		t.Fatalf("304 response body must be empty, got %d bytes", rrCond.Body.Len())
	}
}

// TestPlaybackInventoryPublishesEffectiveVersionIdentity pins that the live
// inventory response names the effective version the transport is bound to, so
// a client polling it can re-key menus without waiting for a replan.
func TestPlaybackInventoryPublishesEffectiveVersionIdentity(t *testing.T) {
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", 100, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	probedAt := time.Now()
	file := &models.MediaFile{
		ID:             100,
		ContentID:      "movie-identity",
		FilePath:       "/media/identity.mkv",
		ProbeUpdatedAt: &probedAt,
		AudioTracks:    []models.AudioTrack{{Index: 1, Codec: "aac", Language: "eng"}},
	}
	h := NewPlaybackHandler(sessionMgr, testPlaybackFileResolver{file: file})

	authCtx := apimw.SetClaims(context.Background(), &auth.Claims{UserID: 1, Role: "user", TokenType: auth.TokenTypeAccess})
	authCtx = apimw.SetProfileID(authCtx, "profile-1")
	req := httptest.NewRequest(http.MethodGet, "/api/v2/playback/"+session.ID+"/inventory", nil).WithContext(authCtx)
	rr := httptest.NewRecorder()
	h.HandleGetPlaybackInventoryV3(rr, withPlaybackRouteParam(req, "session_id", session.ID))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	var inv playback.PlaybackInventoryV3
	if err := json.Unmarshal(rr.Body.Bytes(), &inv); err != nil {
		t.Fatalf("unmarshal inventory: %v", err)
	}
	if inv.EffectiveMediaFileID != file.ID {
		t.Fatalf("effective_media_file_id = %d, want %d", inv.EffectiveMediaFileID, file.ID)
	}
	if inv.EffectiveVirtualURI != "" {
		t.Fatalf("effective_virtual_uri = %q, want empty for a local file", inv.EffectiveVirtualURI)
	}
}

// TestPlaybackInventoryFollowsRotatedVirtualSource pins the failover case: after
// a serve-layer rotation the session's binding names release B while the row the
// session id was planned against is still release A. The response must name B
// and publish B's own declared inventory — never A's tracks — even though B has
// not been probed yet.
func TestPlaybackInventoryFollowsRotatedVirtualSource(t *testing.T) {
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", 100, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	probedAt := time.Now()
	releaseA := &models.MediaFile{
		ID:             100,
		ContentID:      "movie-rotation",
		FilePath:       "virtual://movie/rotation?result=A",
		ProbeUpdatedAt: &probedAt,
		AudioTracks:    []models.AudioTrack{{Index: 1, Codec: "aac", Language: "eng"}},
	}
	// Release B is unprobed: it carries declared provider metadata only.
	releaseB := &models.MediaFile{
		ID:           200,
		ContentID:    "movie-rotation",
		FilePath:     "virtual://movie/rotation?result=B",
		AudioTracks:  []models.AudioTrack{{Index: 1, Codec: "eac3", Language: "deu"}},
		VideoTracks:  []models.VideoTrack{{Codec: "hevc", Width: 1920, Height: 1080}},
		Resolution:   "1080p",
		CodecVideo:   "hevc",
		CodecAudio:   "eac3",
		Container:    "mkv",
		ProviderGUID: "guid-b",
	}
	h := NewPlaybackHandler(sessionMgr, testPlaybackFileResolver{file: releaseA})
	h.VirtualFileLookup = func(_ context.Context, path string) (*models.MediaFile, error) {
		if strings.TrimSpace(path) == releaseB.FilePath {
			return releaseB, nil
		}
		return nil, nil
	}
	if err := sessionMgr.SetVirtualSource(session.ID, releaseB.FilePath, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}

	authCtx := apimw.SetClaims(context.Background(), &auth.Claims{UserID: 1, Role: "user", TokenType: auth.TokenTypeAccess})
	authCtx = apimw.SetProfileID(authCtx, "profile-1")
	req := httptest.NewRequest(http.MethodGet, "/api/v2/playback/"+session.ID+"/inventory", nil).WithContext(authCtx)
	rr := httptest.NewRecorder()
	h.HandleGetPlaybackInventoryV3(rr, withPlaybackRouteParam(req, "session_id", session.ID))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	var inv playback.PlaybackInventoryV3
	if err := json.Unmarshal(rr.Body.Bytes(), &inv); err != nil {
		t.Fatalf("unmarshal inventory: %v", err)
	}
	if inv.EffectiveMediaFileID != releaseB.ID {
		t.Fatalf("effective_media_file_id = %d, want the rotated release %d", inv.EffectiveMediaFileID, releaseB.ID)
	}
	if inv.EffectiveVirtualURI != releaseB.FilePath {
		t.Fatalf("effective_virtual_uri = %q, want %q", inv.EffectiveVirtualURI, releaseB.FilePath)
	}
	if inv.InventoryStatus != "declared" {
		t.Fatalf("inventory_status = %q, want declared for an unprobed sibling", inv.InventoryStatus)
	}
	if len(inv.AudioTracks) != 1 || inv.AudioTracks[0].Language != "deu" {
		t.Fatalf("audio tracks = %#v, want release B's deu track only", inv.AudioTracks)
	}
	for _, track := range inv.AudioTracks {
		if track.Language == "eng" {
			t.Fatal("the previous release's audio track leaked into the rotated inventory")
		}
	}
}

// TestPublishSourceCommittedEmitsEffectiveVersion proves the commit-time push
// names the effective version and its declared inventory on the session's
// realtime connection.
func TestPublishSourceCommittedEmitsEffectiveVersion(t *testing.T) {
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", 100, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	file := &models.MediaFile{
		ID:          100,
		ContentID:   "movie-commit",
		FilePath:    "virtual://movie/commit?result=A",
		AudioTracks: []models.AudioTrack{{Index: 1, Codec: "aac", Language: "eng"}},
	}
	h := NewPlaybackHandler(sessionMgr, testPlaybackFileResolver{file: file})
	h.RealtimeHub = playback.NewRealtimeHub()
	if err := sessionMgr.SetRealtimeConnection(session.ID, true); err != nil {
		t.Fatalf("SetRealtimeConnection: %v", err)
	}
	conn := &sourceCommittedTestConn{}
	registration := h.RealtimeHub.Register(session.ID, conn)
	if registration == nil {
		t.Fatal("expected a realtime registration")
	}
	defer h.RealtimeHub.Unregister(registration)

	h.PublishSourceCommitted(context.Background(), session.ID)

	if len(conn.messages) != 1 {
		t.Fatalf("delivered %d events, want 1", len(conn.messages))
	}
	event, ok := conn.messages[0].(playback.EventEnvelope)
	if !ok {
		t.Fatalf("message type = %T, want playback.EventEnvelope", conn.messages[0])
	}
	if event.Name != playback.RealtimeEventSourceCommitted {
		t.Fatalf("event name = %q, want %q", event.Name, playback.RealtimeEventSourceCommitted)
	}
	var payload playback.SourceCommittedPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.SessionID != session.ID || payload.EffectiveMediaFileID != file.ID {
		t.Fatalf("payload identity = %#v, want session %q file %d", payload, session.ID, file.ID)
	}
	if payload.EffectiveVirtualURI != file.FilePath {
		t.Fatalf("payload effective_virtual_uri = %q, want %q", payload.EffectiveVirtualURI, file.FilePath)
	}
	if len(payload.AudioTracks) != 1 || payload.AudioTracks[0].Language != "eng" {
		t.Fatalf("payload audio tracks = %#v, want the declared eng track", payload.AudioTracks)
	}
}

// TestPublishInventoryUpdatedEmitsVerifiedInventory proves the probe-persistence
// push delivers the session's verified audio and subtitle inventory, with the
// revision a client gates on, on the session's realtime connection.
func TestPublishInventoryUpdatedEmitsVerifiedInventory(t *testing.T) {
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", 100, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	probedAt := time.Now()
	file := &models.MediaFile{
		ID:             100,
		ContentID:      "movie-inventory-update",
		FilePath:       "/media/inventory-update.mkv",
		ProbeUpdatedAt: &probedAt,
		AudioTracks: []models.AudioTrack{
			{Index: 1, Codec: "aac", Channels: 2, Language: "eng", Default: true},
			{Index: 2, Codec: "ac3", Channels: 6, Language: "fre"},
		},
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 3, Codec: "subrip", Language: "eng", Default: true},
		},
	}
	h := NewPlaybackHandler(sessionMgr, testPlaybackFileResolver{file: file})
	h.RealtimeHub = playback.NewRealtimeHub()
	if err := sessionMgr.SetRealtimeConnection(session.ID, true); err != nil {
		t.Fatalf("SetRealtimeConnection: %v", err)
	}
	conn := &sourceCommittedTestConn{}
	registration := h.RealtimeHub.Register(session.ID, conn)
	if registration == nil {
		t.Fatal("expected a realtime registration")
	}
	defer h.RealtimeHub.Unregister(registration)

	h.PublishInventoryUpdated(context.Background(), file.ID)

	if len(conn.messages) != 1 {
		t.Fatalf("delivered %d events, want 1", len(conn.messages))
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
	if payload.SessionID != session.ID {
		t.Fatalf("payload session = %q, want %q", payload.SessionID, session.ID)
	}
	if payload.InventoryStatus != "verified" || payload.InventoryRevision == "" {
		t.Fatalf("payload status/revision = %q/%q, want verified and a non-empty revision", payload.InventoryStatus, payload.InventoryRevision)
	}
	if len(payload.AudioTracks) != 2 {
		t.Fatalf("payload audio tracks = %v, want 2", payload.AudioTracks)
	}
	if len(payload.SubtitleInventory) != 1 {
		t.Fatalf("payload subtitle inventory = %v, want 1", payload.SubtitleInventory)
	}
}

func TestColdCandidateSessionDoesNotFreezeEmptySubtitleEvidence(t *testing.T) {
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", 100, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	coldVirtualFile := &models.MediaFile{
		ID:             100,
		ContentID:      "movie-cold-virtual",
		FilePath:       "virtual://movie/cold-cand?result=cand-1",
		Container:      "virtual",
		ProbeUpdatedAt: nil,
	}

	h := NewPlaybackHandler(sessionMgr, testPlaybackFileResolver{file: coldVirtualFile})
	state := h.v3SessionStreamState(context.Background(), session, coldVirtualFile, playback.PlannerResultV3{}, preparedTransportV3{}, mediaAuthModeV3{})
	if state.VirtualSubtitleEvidenceSet {
		t.Fatal("cold candidate with nil ProbeUpdatedAt must not freeze VirtualSubtitleEvidenceSet to true")
	}

	// Apply stream state to the session manager, reflecting the live start decision
	if err := sessionMgr.UpdateStreamState(session.ID, state); err != nil {
		t.Fatalf("UpdateStreamState: %v", err)
	}

	// When probe completes, probe evidence arrives on the row with multi-audio and subtitles
	probedAt := time.Now()
	coldVirtualFile.ProbeUpdatedAt = &probedAt
	coldVirtualFile.AudioTracks = []models.AudioTrack{
		{Index: 1, Codec: "aac", Channels: 2, Language: "eng", Default: true},
		{Index: 2, Codec: "ac3", Channels: 6, Language: "fre"},
	}
	coldVirtualFile.SubtitleTracks = []models.SubtitleTrack{
		{Index: 3, Codec: "subrip", Language: "eng", Default: true},
		{Index: 4, Codec: "subrip", Language: "fre"},
	}

	h.RealtimeHub = playback.NewRealtimeHub()
	if err := sessionMgr.SetRealtimeConnection(session.ID, true); err != nil {
		t.Fatalf("SetRealtimeConnection: %v", err)
	}
	conn := &sourceCommittedTestConn{}
	registration := h.RealtimeHub.Register(session.ID, conn)
	if registration == nil {
		t.Fatal("expected a realtime registration")
	}
	defer h.RealtimeHub.Unregister(registration)

	h.PublishInventoryUpdated(context.Background(), coldVirtualFile.ID)

	if len(conn.messages) != 1 {
		t.Fatalf("delivered %d events, want 1", len(conn.messages))
	}
	event := conn.messages[0].(playback.EventEnvelope)
	if event.Name != playback.RealtimeEventInventoryUpdated {
		t.Fatalf("event name = %q, want %q", event.Name, playback.RealtimeEventInventoryUpdated)
	}
	var payload playback.InventoryUpdatedPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.SessionID != session.ID {
		t.Fatalf("payload session_id = %q, want %q", payload.SessionID, session.ID)
	}
	if payload.InventoryStatus != "verified" {
		t.Fatalf("payload inventory_status = %q, want verified", payload.InventoryStatus)
	}
	if len(payload.AudioTracks) != 2 {
		t.Fatalf("audio tracks len = %d, want 2 (multi-audio)", len(payload.AudioTracks))
	}
	if len(payload.SubtitleInventory) != 2 {
		t.Fatalf("subtitle inventory len = %d, want 2", len(payload.SubtitleInventory))
	}

	// Verify the active session remains healthy and usable in session manager
	active, err := sessionMgr.GetSession(session.ID)
	if err != nil || active == nil {
		t.Fatalf("session %s no longer active in manager: %v", session.ID, err)
	}
	if active.MediaFileID != coldVirtualFile.ID {
		t.Fatalf("active session file ID = %d, want %d", active.MediaFileID, coldVirtualFile.ID)
	}
}

type sourceCommittedTestConn struct {
	messages []any
}

func (c *sourceCommittedTestConn) WriteJSON(v any) error {
	c.messages = append(c.messages, v)
	return nil
}

// bindingMoveFileResolverV3 fires a source-binding move on the first file load,
// modeling a rotation that lands while an inventory build is resolving.
type bindingMoveFileResolverV3 struct {
	FilePathResolver
	move func()
	once sync.Once
}

func (r *bindingMoveFileResolverV3) GetByID(ctx context.Context, id int) (*models.MediaFile, error) {
	r.once.Do(func() {
		if r.move != nil {
			r.move()
		}
	})
	return r.FilePathResolver.GetByID(ctx, id)
}

// generationTearManager lands a binding move between the inventory publisher's
// generation read and its session re-read, reproducing the exact tear the
// atomic read closes: the generation is captured pre-move while the copy is
// post-move. A manager without the atomic capability would drop the fresh build.
type generationTearManager struct {
	*playback.SessionManager
	from string
	to   string
	once sync.Once
}

func (m *generationTearManager) GetSession(sessionID string) (*playback.Session, error) {
	m.once.Do(func() {
		if m.from != "" {
			_ = m.SetVirtualSource(sessionID, m.to, 5)
		}
	})
	return m.SessionManager.GetSession(sessionID)
}

// virtualProbedInventoryFile builds a probed virtual release with one audio
// track whose language identifies the release in the published payload.
func virtualProbedInventoryFile(uri, language string) *models.MediaFile {
	probedAt := time.Now()
	return &models.MediaFile{
		ID:             100,
		ContentID:      "movie-inventory-fence",
		FilePath:       uri,
		ProbeUpdatedAt: &probedAt,
		AudioTracks:    []models.AudioTrack{{Index: 1, Codec: "aac", Language: language, Default: true}},
	}
}

// TestPublishInventoryUpdatedReReadsLiveSourceBinding pins the pre-lock read
// fix: the caller's session snapshot is taken before the per-session lock, so a
// binding that moved in between must be published from the live session, not
// the stale snapshot.
func TestPublishInventoryUpdatedReReadsLiveSourceBinding(t *testing.T) {
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", 100, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	const releaseA = "virtual://movie/inventory-fence?result=A"
	releaseB := virtualProbedInventoryFile("virtual://movie/inventory-fence?result=B", "deu")
	if err := sessionMgr.SetVirtualSource(session.ID, releaseA, 5); err != nil {
		t.Fatalf("SetVirtualSource A: %v", err)
	}
	if err := sessionMgr.SetVirtualSource(session.ID, releaseB.FilePath, 5); err != nil {
		t.Fatalf("SetVirtualSource B: %v", err)
	}

	h := NewPlaybackHandler(sessionMgr, testPlaybackFileResolver{file: releaseB})
	h.RealtimeHub = playback.NewRealtimeHub()
	if err := sessionMgr.SetRealtimeConnection(session.ID, true); err != nil {
		t.Fatalf("SetRealtimeConnection: %v", err)
	}
	conn := &sourceCommittedTestConn{}
	registration := h.RealtimeHub.Register(session.ID, conn)
	if registration == nil {
		t.Fatal("expected a realtime registration")
	}
	defer h.RealtimeHub.Unregister(registration)

	// The snapshot the caller enumerated still names release A.
	stale := *session
	stale.VirtualSourceURI = releaseA
	h.publishInventoryUpdatedToSession(context.Background(), &stale, 0, "", nil)

	if len(conn.messages) != 1 {
		t.Fatalf("delivered %d events, want 1", len(conn.messages))
	}
	event, ok := conn.messages[0].(playback.EventEnvelope)
	if !ok {
		t.Fatalf("message type = %T, want playback.EventEnvelope", conn.messages[0])
	}
	var payload playback.InventoryUpdatedPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.EffectiveVirtualURI != releaseB.FilePath {
		t.Fatalf("published effective_virtual_uri = %q, want the live binding %q", payload.EffectiveVirtualURI, releaseB.FilePath)
	}
	if len(payload.AudioTracks) != 1 || payload.AudioTracks[0].Language != "deu" {
		t.Fatalf("published audio tracks = %#v, want the live release's deu track", payload.AudioTracks)
	}
}

// TestPublishInventoryUpdatedDropsBuildRacedByBindingMove pins the generation
// fence: when a source-binding move lands while the inventory is being built,
// the older build must be dropped rather than delivered after the newer
// release's inventory.
func TestPublishInventoryUpdatedDropsBuildRacedByBindingMove(t *testing.T) {
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", 100, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	releaseA := virtualProbedInventoryFile("virtual://movie/inventory-fence?result=A", "eng")
	if err := sessionMgr.SetVirtualSource(session.ID, releaseA.FilePath, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}

	h := NewPlaybackHandler(sessionMgr, testPlaybackFileResolver{file: releaseA})
	h.RealtimeHub = playback.NewRealtimeHub()
	if err := sessionMgr.SetRealtimeConnection(session.ID, true); err != nil {
		t.Fatalf("SetRealtimeConnection: %v", err)
	}
	conn := &sourceCommittedTestConn{}
	registration := h.RealtimeHub.Register(session.ID, conn)
	if registration == nil {
		t.Fatal("expected a realtime registration")
	}
	defer h.RealtimeHub.Unregister(registration)

	// The binding moves to release B while release A's inventory is resolved.
	h.fileResolver = &bindingMoveFileResolverV3{
		FilePathResolver: h.fileResolver,
		move: func() {
			if moveErr := sessionMgr.SetVirtualSource(session.ID, "virtual://movie/inventory-fence?result=B", 5); moveErr != nil {
				t.Errorf("move binding: %v", moveErr)
			}
		},
	}

	h.PublishInventoryUpdated(context.Background(), releaseA.ID)

	if len(conn.messages) != 0 {
		t.Fatalf("delivered %d events, want 0: the build raced a source-binding move", len(conn.messages))
	}
}

// TestPublishInventoryUpdatedKeepsFreshBuildWhenReadGenerationLags pins finding
// 4: the fence must pair the generation with the live copy the build actually
// uses. A move that lands between a separately read generation and the session
// re-read leaves the build using the current source; the fence must not drop
// that payload on the strength of the older generation. The manager below
// performs exactly that move when the generation is read.
func TestPublishInventoryUpdatedKeepsFreshBuildWhenReadGenerationLags(t *testing.T) {
	base := playback.NewSessionManager(0, 0)
	session, err := base.StartSession(1, "profile-1", 100, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	releaseB := virtualProbedInventoryFile("virtual://movie/inventory-fence?result=B", "deu")
	if err := base.SetVirtualSource(session.ID, "virtual://movie/inventory-fence?result=A", 5); err != nil {
		t.Fatalf("SetVirtualSource A: %v", err)
	}

	// The manager moves the binding to the verified release B just before the
	// session re-read, so a separately captured generation still names A's
	// binding while the copy the build uses is B. The fresh build from B must
	// not be dropped, and the atomic read that pairs copy and generation must
	// see this tear and still deliver.
	manager := &generationTearManager{SessionManager: base, from: "virtual://movie/inventory-fence?result=A", to: releaseB.FilePath}

	h := NewPlaybackHandler(manager, testPlaybackFileResolver{file: releaseB})
	h.RealtimeHub = playback.NewRealtimeHub()
	if err := base.SetRealtimeConnection(session.ID, true); err != nil {
		t.Fatalf("SetRealtimeConnection: %v", err)
	}
	conn := &sourceCommittedTestConn{}
	registration := h.RealtimeHub.Register(session.ID, conn)
	if registration == nil {
		t.Fatal("expected a realtime registration")
	}
	defer h.RealtimeHub.Unregister(registration)

	// A stale snapshot from before the binding moved, as PublishInventoryUpdated
	// enumerates before taking the per-session lock.
	stale := *session
	stale.VirtualSourceURI = "virtual://movie/inventory-fence?result=A"

	h.publishInventoryUpdatedToSession(context.Background(), &stale, 0, "", nil)

	if len(conn.messages) != 1 {
		t.Fatalf("delivered %d events, want 1: a payload built from the live binding must not be dropped by a lagging generation read", len(conn.messages))
	}
}

func TestComputeInventoryRevisionDeterministic(t *testing.T) {
	audio := []playback.AudioInventoryItemV3{
		{Index: 1, Codec: "aac", Channels: 2, Language: "eng", Default: true},
	}
	subs := []playback.SubtitleInventoryItemV3{
		{TrackID: "sub-1", Codec: "subrip", Language: "eng", Default: true},
	}

	r1 := playback.ComputeInventoryRevisionV3("verified", audio, subs)
	r2 := playback.ComputeInventoryRevisionV3("verified", audio, subs)
	if r1 != r2 {
		t.Fatalf("revision not deterministic: %q != %q", r1, r2)
	}

	rDeclared := playback.ComputeInventoryRevisionV3("declared", audio, subs)
	if rDeclared == r1 {
		t.Fatal("revision must differ when status changes from declared to verified")
	}

	audioUpdated := append(audio, playback.AudioInventoryItemV3{Index: 2, Codec: "ac3", Channels: 6, Language: "fre"})
	rUpdated := playback.ComputeInventoryRevisionV3("verified", audioUpdated, subs)
	if rUpdated == r1 {
		t.Fatal("revision must differ when audio tracks change")
	}

	// Delimiter collision test: titles containing colons must not produce identical digests
	audioCol1 := []playback.AudioInventoryItemV3{
		{Title: "a:b", EmbeddedTitle: "c", TrackID: "1"},
	}
	audioCol2 := []playback.AudioInventoryItemV3{
		{Title: "a", EmbeddedTitle: "b:c", TrackID: "1"},
	}
	rCol1 := playback.ComputeInventoryRevisionV3("verified", audioCol1, subs)
	rCol2 := playback.ComputeInventoryRevisionV3("verified", audioCol2, subs)
	if rCol1 == rCol2 {
		t.Fatalf("delimiter collision: %q == %q", rCol1, rCol2)
	}

	// The effective source is part of the digest, so a rotation to a sibling
	// with an identical inventory still changes the ETag. The zero identity
	// keeps the historical inventory-only digest.
	rReleaseA := playback.ComputeInventoryRevisionV3("verified", audio, subs, playback.InventorySourceIdentityV3{
		EffectiveMediaFileID:  100,
		EffectiveVirtualURI:   "virtual://movie/x?result=A",
		VirtualSourceRevision: "rev-a",
	})
	rReleaseB := playback.ComputeInventoryRevisionV3("verified", audio, subs, playback.InventorySourceIdentityV3{
		EffectiveMediaFileID:  200,
		EffectiveVirtualURI:   "virtual://movie/x?result=B",
		VirtualSourceRevision: "",
	})
	if rReleaseA == rReleaseB {
		t.Fatal("revision must differ when only the effective source changes")
	}
	if rReleaseA == r1 {
		t.Fatal("revision must differ when a source identity is supplied")
	}
	if got := playback.ComputeInventoryRevisionV3("verified", audio, subs, playback.InventorySourceIdentityV3{}); got != r1 {
		t.Fatalf("zero identity changed the historical digest: %q != %q", got, r1)
	}
}
