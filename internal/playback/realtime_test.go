package playback

import (
	"encoding/json"
	"testing"
)

func TestNewChapterThumbnailReadyEvent(t *testing.T) {
	event, err := NewChapterThumbnailReadyEvent(
		"session-1",
		42,
		3,
		"https://example.com/thumb.jpg",
		"thumbhash",
	)
	if err != nil {
		t.Fatalf("NewChapterThumbnailReadyEvent() error = %v", err)
	}

	if event.Type != RealtimeMessageTypeEvent {
		t.Fatalf("event.Type = %q, want %q", event.Type, RealtimeMessageTypeEvent)
	}
	if event.Name != RealtimeEventChapterThumbnailReady {
		t.Fatalf("event.Name = %q, want %q", event.Name, RealtimeEventChapterThumbnailReady)
	}

	var payload ChapterThumbnailReadyPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("json.Unmarshal(payload): %v", err)
	}
	if payload.SessionID != "session-1" || payload.FileID != 42 || payload.ChapterIndex != 3 {
		t.Fatalf("payload = %#v, want session/file/chapter identifiers", payload)
	}
	if payload.ThumbnailURL != "https://example.com/thumb.jpg" {
		t.Fatalf("payload.ThumbnailURL = %q, want thumbnail URL", payload.ThumbnailURL)
	}
}

// TestNewSourceCommittedEventEncodesEmptyAudioTracksAsArray pins that a
// committed release which declares no audio tracks still serializes the field
// as [], so a client can tell "none declared" from "field absent".
func TestNewSourceCommittedEventEncodesEmptyAudioTracksAsArray(t *testing.T) {
	event, err := NewSourceCommittedEvent("session-1", SourceCommittedPayload{
		EffectiveMediaFileID: 200,
		EffectiveVirtualURI:  "virtual://movie/x?result=B",
	})
	if err != nil {
		t.Fatalf("NewSourceCommittedEvent() error = %v", err)
	}
	if event.Name != RealtimeEventSourceCommitted {
		t.Fatalf("event.Name = %q, want %q", event.Name, RealtimeEventSourceCommitted)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(event.Payload, &raw); err != nil {
		t.Fatalf("json.Unmarshal(payload): %v", err)
	}
	tracks, ok := raw["audio_tracks"]
	if !ok {
		t.Fatal("audio_tracks is absent; want an explicit empty array")
	}
	if string(tracks) != "[]" {
		t.Fatalf("audio_tracks = %s, want []", tracks)
	}
}

// TestNewInventoryUpdatedEventMirrorsInventoryShape pins that the
// inventory_updated payload serializes the full live inventory (audio,
// subtitles, status and revision) and normalizes empty lists to arrays, so a
// client can apply it with the same reducer as the inventory endpoint.
func TestNewInventoryUpdatedEventMirrorsInventoryShape(t *testing.T) {
	event, err := NewInventoryUpdatedEvent("session-1", PlaybackInventoryV3{
		InventoryStatus:   "verified",
		InventoryRevision: "inv:abc",
	})
	if err != nil {
		t.Fatalf("NewInventoryUpdatedEvent() error = %v", err)
	}
	if event.Name != RealtimeEventInventoryUpdated {
		t.Fatalf("event.Name = %q, want %q", event.Name, RealtimeEventInventoryUpdated)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(event.Payload, &raw); err != nil {
		t.Fatalf("json.Unmarshal(payload): %v", err)
	}
	for _, field := range []string{"audio_tracks", "subtitle_inventory"} {
		value, ok := raw[field]
		if !ok {
			t.Fatalf("%s is absent; want an explicit empty array", field)
		}
		if string(value) != "[]" {
			t.Fatalf("%s = %s, want []", field, value)
		}
	}
	var payload InventoryUpdatedPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("json.Unmarshal(payload): %v", err)
	}
	if payload.SessionID != "session-1" {
		t.Fatalf("payload.SessionID = %q, want session-1", payload.SessionID)
	}
	if payload.InventoryStatus != "verified" || payload.InventoryRevision != "inv:abc" {
		t.Fatalf("payload status/revision = %q/%q, want verified/inv:abc", payload.InventoryStatus, payload.InventoryRevision)
	}
}

func TestNewMarkersUpdatedEvent(t *testing.T) {
	event, err := NewMarkersUpdatedEvent(
		"session-1",
		42,
		&TimeRangePayload{Start: 12, End: 75},
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("NewMarkersUpdatedEvent() error = %v", err)
	}
	if event.Name != RealtimeEventMarkersUpdated {
		t.Fatalf("event.Name = %q, want %q", event.Name, RealtimeEventMarkersUpdated)
	}

	var payload MarkersUpdatedPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("json.Unmarshal(payload): %v", err)
	}
	if payload.SessionID != "session-1" || payload.FileID != 42 {
		t.Fatalf("payload = %#v, want session/file identifiers", payload)
	}
	if payload.Intro == nil || payload.Intro.Start != 12 || payload.Intro.End != 75 {
		t.Fatalf("payload.Intro = %#v, want intro range", payload.Intro)
	}
	if payload.Credits != nil {
		t.Fatalf("payload.Credits = %#v, want nil", payload.Credits)
	}
}

func TestParseCommandEnvelopeStillWorks(t *testing.T) {
	command, err := ParseCommandEnvelope([]byte(`{
		"type":"command",
		"command_id":"cmd-1",
		"session_id":"session-1",
		"name":"pause",
		"payload":{"reason":"test"}
	}`))
	if err != nil {
		t.Fatalf("ParseCommandEnvelope() error = %v", err)
	}
	if command.Type != RealtimeMessageTypeCommand {
		t.Fatalf("command.Type = %q, want %q", command.Type, RealtimeMessageTypeCommand)
	}
	if command.Name != CommandPause {
		t.Fatalf("command.Name = %q, want %q", command.Name, CommandPause)
	}
}

// The plan id is what lets a client ignore a command for a plan it has already
// replanned past, so an envelope without one must never be built.
func TestNewPlanInvalidatedCommandRequiresPlanAndReason(t *testing.T) {
	if _, err := NewPlanInvalidatedCommand("session-1", "cmd-1", "", PlanInvalidatedVideoCopyUnsafe); err == nil {
		t.Fatal("NewPlanInvalidatedCommand() with no plan id = nil error, want a rejection")
	}
	if _, err := NewPlanInvalidatedCommand("session-1", "cmd-1", "plan-1", ""); err == nil {
		t.Fatal("NewPlanInvalidatedCommand() with no reason = nil error, want a rejection")
	}
}

// A client advertising the command in its hello must validate: the closed
// command enum is the negotiation surface for the realtime channel.
func TestHelloAcceptsPlanInvalidatedCapability(t *testing.T) {
	hello := HelloEnvelope{
		Type:         RealtimeMessageTypeHello,
		SessionID:    "session-1",
		Client:       HelloClientInfo{Name: "silo-web", Version: "1.0.0"},
		Capabilities: HelloCapabilities{Commands: []CommandName{CommandPause, CommandPlanInvalidated}},
	}
	if err := hello.Validate(); err != nil {
		t.Fatalf("hello.Validate() = %v, want the plan_invalidated capability accepted", err)
	}
}
