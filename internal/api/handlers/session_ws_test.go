package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"

	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/auth"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

func TestHandleSessionWebSocket_RequiresHelloBeforeRealtimeReady(t *testing.T) {
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", 100, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	handler := NewPlaybackHandler(sessionMgr)
	handler.RealtimeHub = playback.NewRealtimeHub()

	router := chi.NewRouter()
	router.Get("/playback/ws/{session_id}", func(w http.ResponseWriter, r *http.Request) {
		ctx := apimw.SetClaims(r.Context(), &auth.Claims{UserID: 1, Role: "user", TokenType: auth.TokenTypeAccess})
		handler.HandleSessionWebSocket(w, r.WithContext(ctx))
	})

	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/playback/ws/" + session.ID
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if resp != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if err != nil {
		t.Fatalf("Dial websocket: %v", err)
	}
	defer func() { _ = conn.Close() }()

	got, err := sessionMgr.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession before hello: %v", err)
	}
	if got.HasRealtimeConnection {
		t.Fatal("session should not be realtime-ready before hello")
	}
	if got.HasWebSocket {
		t.Fatal("session should not report playback control before hello")
	}

	if err := conn.WriteJSON(playback.HelloEnvelope{
		Type:      playback.RealtimeMessageTypeHello,
		SessionID: session.ID,
		Client: playback.HelloClientInfo{
			Name:    "ios",
			Version: "1.0.0",
		},
		Capabilities: playback.HelloCapabilities{
			Commands: []playback.CommandName{
				playback.CommandPause,
				playback.CommandUnpause,
				playback.CommandStop,
				playback.CommandTerminate,
			},
		},
	}); err != nil {
		t.Fatalf("WriteJSON hello: %v", err)
	}

	waitForPlaybackRealtimeState(t, sessionMgr, session.ID, true)

	if err := conn.Close(); err != nil {
		t.Fatalf("Close websocket: %v", err)
	}

	waitForPlaybackRealtimeState(t, sessionMgr, session.ID, false)
}

func waitForPlaybackRealtimeState(t *testing.T, sessionMgr *playback.SessionManager, sessionID string, want bool) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		session, err := sessionMgr.GetSession(sessionID)
		if err == nil && session != nil && session.HasRealtimeConnection == want && session.HasWebSocket == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	session, err := sessionMgr.GetSession(sessionID)
	if err != nil {
		t.Fatalf("GetSession after wait: %v", err)
	}
	t.Fatalf(
		"session realtime state = %v/%v, want %v/%v",
		session.HasRealtimeConnection,
		session.HasWebSocket,
		want,
		want,
	)
}

func TestHandleSessionWebSocket_PushesVerifiedInventoryOnHello(t *testing.T) {
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", 100, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	probedAt := time.Now()
	file := &models.MediaFile{
		ID:             100,
		FilePath:       "/media/test.mkv",
		ProbeUpdatedAt: &probedAt,
		AudioTracks: []models.AudioTrack{
			{Index: 1, Codec: "aac", Channels: 2, Language: "eng", Default: true},
		},
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 2, Codec: "subrip", Language: "eng", Default: true},
		},
	}

	handler := NewPlaybackHandler(sessionMgr, testPlaybackFileResolver{file: file})
	handler.RealtimeHub = playback.NewRealtimeHub()

	router := chi.NewRouter()
	router.Get("/playback/ws/{session_id}", func(w http.ResponseWriter, r *http.Request) {
		ctx := apimw.SetClaims(r.Context(), &auth.Claims{UserID: 1, Role: "user", TokenType: auth.TokenTypeAccess})
		handler.HandleSessionWebSocket(w, r.WithContext(ctx))
	})

	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/playback/ws/" + session.ID
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if resp != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if err != nil {
		t.Fatalf("Dial websocket: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.WriteJSON(playback.HelloEnvelope{
		Type:      playback.RealtimeMessageTypeHello,
		SessionID: session.ID,
		Client: playback.HelloClientInfo{
			Name:    "web",
			Version: "1.0.0",
		},
	}); err != nil {
		t.Fatalf("WriteJSON hello: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var event playback.EventEnvelope
	if err := conn.ReadJSON(&event); err != nil {
		t.Fatalf("ReadJSON inventory_updated: %v", err)
	}

	if event.Name != playback.RealtimeEventInventoryUpdated {
		t.Fatalf("event name = %q, want %q", event.Name, playback.RealtimeEventInventoryUpdated)
	}

	var payload playback.InventoryUpdatedPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	if payload.SessionID != session.ID {
		t.Fatalf("payload session_id = %q, want %q", payload.SessionID, session.ID)
	}
	if payload.InventoryStatus != "verified" {
		t.Fatalf("payload inventory_status = %q, want verified", payload.InventoryStatus)
	}
	if len(payload.SubtitleInventory) != 1 {
		t.Fatalf("len(payload.SubtitleInventory) = %d, want 1", len(payload.SubtitleInventory))
	}
}
