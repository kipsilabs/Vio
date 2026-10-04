package playback

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Silo-Server/silo-server/internal/models"
)

// Realtime message types exchanged over /playback/ws/{session_id}.
type RealtimeMessageType string

const (
	RealtimeMessageTypeCommand RealtimeMessageType = "command"
	RealtimeMessageTypeEvent   RealtimeMessageType = "event"
	RealtimeMessageTypeHello   RealtimeMessageType = "hello"
	RealtimeMessageTypeAck     RealtimeMessageType = "ack"
	RealtimeMessageTypeResult  RealtimeMessageType = "result"
)

// RealtimeEventName identifies a supported server-pushed event.
type RealtimeEventName string

const (
	RealtimeEventChapterThumbnailReady    RealtimeEventName = "chapter_thumbnail_ready"
	RealtimeEventMarkersUpdated           RealtimeEventName = "markers_updated"
	RealtimeEventSubtitleReady            RealtimeEventName = "subtitle_ready"
	RealtimeEventSubtitleTranslationStart RealtimeEventName = "subtitle_translation_started"
	RealtimeEventSubtitleTranslationCues  RealtimeEventName = "subtitle_translation_cues"
	RealtimeEventSubtitleTranslationDone  RealtimeEventName = "subtitle_translation_completed"
	RealtimeEventSubtitleTranslationFail  RealtimeEventName = "subtitle_translation_failed"
	RealtimeEventSubtitleTimingChanged    RealtimeEventName = "subtitle_timing_changed"
	// RealtimeEventSourceCommitted publishes the effective version a transport
	// has committed to — a fresh start or a serve-layer rotation — together
	// with that release's declared (not yet probed) track inventory. It is sent
	// the moment the commit happens so a playing client can re-key its version
	// menu and refresh its track list without waiting for a plan rebuild or the
	// background ffprobe. The existing inventory poll and provenance upgrade
	// then replace the declared list with probe evidence when it lands.
	RealtimeEventSourceCommitted RealtimeEventName = "source_committed"
	// RealtimeEventInventoryUpdated publishes the probe-verified audio and
	// subtitle inventory of a live session once the background probe has
	// persisted its evidence. source_committed carries the release's declared
	// snapshot the moment a transport commits; this event is the upgrade that
	// follows when the full probe lands, so a client can replace its declared
	// track menu without waiting for the inventory poll or a replan. It is
	// advisory menu data: it never carries an executable recipe and never
	// changes plan generation, transport choice, or the streamed bytes.
	RealtimeEventInventoryUpdated RealtimeEventName = "inventory_updated"
	// RealtimeEventDownloadProgress reports a provider-side cache fill for a
	// release a session is pinned to. It is best-effort telemetry: a client
	// shows a "preparing" state from it, but losing the event must never stop
	// playback. The cached copy becoming servable is what drives the in-session
	// handoff; the event only tells the client how far along the fill is.
	RealtimeEventDownloadProgress RealtimeEventName = "download.progress"
)

var supportedRealtimeEventNameSet = map[RealtimeEventName]struct{}{
	RealtimeEventChapterThumbnailReady:    {},
	RealtimeEventMarkersUpdated:           {},
	RealtimeEventSubtitleReady:            {},
	RealtimeEventSubtitleTranslationStart: {},
	RealtimeEventSubtitleTranslationCues:  {},
	RealtimeEventSubtitleTranslationDone:  {},
	RealtimeEventSubtitleTranslationFail:  {},
	RealtimeEventSubtitleTimingChanged:    {},
	RealtimeEventSourceCommitted:          {},
	RealtimeEventInventoryUpdated:         {},
	RealtimeEventDownloadProgress:         {},
}

// CommandName identifies a supported realtime command.
type CommandName string

const (
	CommandPause              CommandName = "pause"
	CommandUnpause            CommandName = "unpause"
	CommandPlayPause          CommandName = "play_pause"
	CommandSeek               CommandName = "seek"
	CommandSetVolume          CommandName = "set_volume"
	CommandStop               CommandName = "stop"
	CommandTerminate          CommandName = "terminate"
	CommandDisplayMessage     CommandName = "display_message"
	CommandServerRestarting   CommandName = "server_restarting"
	CommandServerShuttingDown CommandName = "server_shutting_down"
	CommandPlayMedia          CommandName = "play_media"
	CommandSetAudioTrack      CommandName = "set_audio_track"
	CommandSetSubtitleTrack   CommandName = "set_subtitle_track"
	// CommandPlanInvalidated tells a playing client that the plan it is running
	// can no longer serve this source, so it must replan off that plan. It is
	// the first server-initiated control push in the v3 protocol, and it is only
	// sent to a session whose attempt negotiated FeaturePlanInvalidatedV3 and
	// which has a live realtime connection; every other session is stopped
	// instead. See CopySafetyNotifier.
	CommandPlanInvalidated CommandName = "plan_invalidated"
)

var supportedCommandNames = []CommandName{
	CommandPause,
	CommandUnpause,
	CommandPlayPause,
	CommandSeek,
	CommandSetVolume,
	CommandStop,
	CommandTerminate,
	CommandDisplayMessage,
	CommandServerRestarting,
	CommandServerShuttingDown,
	CommandPlayMedia,
	CommandSetAudioTrack,
	CommandSetSubtitleTrack,
	CommandPlanInvalidated,
}

var supportedCommandNameSet = func() map[CommandName]struct{} {
	names := map[CommandName]struct{}{}
	for _, name := range supportedCommandNames {
		names[name] = struct{}{}
	}
	return names
}()

// RealtimeAckStatus represents the client acknowledgement state.
type RealtimeAckStatus string

const (
	RealtimeAckStatusAccepted RealtimeAckStatus = "accepted"
)

// RealtimeResultStatus represents the client completion state.
type RealtimeResultStatus string

const (
	RealtimeResultStatusCompleted RealtimeResultStatus = "completed"
	RealtimeResultStatusRejected  RealtimeResultStatus = "rejected"
)

var (
	ErrUnsupportedCommandName = errors.New("unsupported command name")
	ErrUnsupportedEventName   = errors.New("unsupported realtime event name")
	ErrInvalidRealtimePayload = errors.New("invalid realtime payload")
)

// EventEnvelope is the server-to-client realtime event message.
type EventEnvelope struct {
	Type      RealtimeMessageType `json:"type"`
	SessionID string              `json:"session_id"`
	Name      RealtimeEventName   `json:"name"`
	Payload   json.RawMessage     `json:"payload,omitempty"`
}

// ChapterThumbnailReadyPayload describes one chapter thumbnail that became available.
type ChapterThumbnailReadyPayload struct {
	SessionID          string `json:"session_id"`
	FileID             int    `json:"file_id"`
	ChapterIndex       int    `json:"chapter_index"`
	ThumbnailURL       string `json:"thumbnail_url"`
	ThumbnailThumbhash string `json:"thumbnail_thumbhash,omitempty"`
}

type TimeRangePayload struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

type MarkersUpdatedPayload struct {
	SessionID      string                 `json:"session_id"`
	FileID         int                    `json:"file_id"`
	Intro          *TimeRangePayload      `json:"intro"`
	Credits        *TimeRangePayload      `json:"credits"`
	Recap          *TimeRangePayload      `json:"recap"`
	Preview        *TimeRangePayload      `json:"preview"`
	MarkerSegments []models.MarkerSegment `json:"marker_segments"`
}

// SubtitleReadyPayload announces that a newly generated subtitle track (AI
// translation, and later ASR) is available for the file, so the player can
// refresh its track list and optionally select it.
type SubtitleReadyPayload struct {
	SessionID  string `json:"session_id"`
	FileID     int    `json:"file_id"`
	SubtitleID int    `json:"subtitle_id"`
	Language   string `json:"language"`
	Label      string `json:"label,omitempty"`
	// Track carries the new track's frozen combined ordinal, identity, and
	// stream URL so the client can select it directly. Clients must not derive
	// the ordinal themselves. Absent only when the server could not resolve the
	// file's track inventory, in which case the client refetches the plan.
	Track *SubtitleInventoryItemV3 `json:"track,omitempty"`
}

// StreamCue is one translated subtitle cue pushed to the player during a live
// translation. Start/End are absolute media-time seconds; Text may contain
// embedded newlines for multi-line cues.
type StreamCue struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

// SubtitleTranslationStartedPayload tells the player a live translation has
// begun, so it can create a placeholder track, select it, and pause until the
// first cues near the playhead arrive. TrackKey identifies the live track for
// subsequent cue/completion events.
type SubtitleTranslationStartedPayload struct {
	SessionID string `json:"session_id"`
	FileID    int    `json:"file_id"`
	JobID     int64  `json:"job_id"`
	TrackKey  string `json:"track_key"`
	Language  string `json:"language"`
	Label     string `json:"label,omitempty"`
	TotalCues int    `json:"total_cues"`
}

// SubtitleTranslationCuesPayload delivers a batch of translated cues for a live
// track as it is produced. Done/Total track overall progress.
type SubtitleTranslationCuesPayload struct {
	SessionID string      `json:"session_id"`
	FileID    int         `json:"file_id"`
	JobID     int64       `json:"job_id"`
	TrackKey  string      `json:"track_key"`
	Cues      []StreamCue `json:"cues"`
	Done      int         `json:"done"`
	Total     int         `json:"total"`
}

// SubtitleTranslationCompletedPayload signals the live translation finished and
// the full track is persisted as a downloaded subtitle (SubtitleID).
type SubtitleTranslationCompletedPayload struct {
	SessionID  string `json:"session_id"`
	FileID     int    `json:"file_id"`
	JobID      int64  `json:"job_id"`
	TrackKey   string `json:"track_key"`
	SubtitleID int    `json:"subtitle_id"`
	Language   string `json:"language"`
	Label      string `json:"label,omitempty"`
	// Track carries the persisted track's frozen combined ordinal, identity,
	// and stream URL, replacing the placeholder the client created at
	// translation start. See SubtitleReadyPayload.Track.
	Track *SubtitleInventoryItemV3 `json:"track,omitempty"`
}

// SubtitleTranslationFailedPayload signals a live translation failed, so the
// player can drop the placeholder track and resume playback.
type SubtitleTranslationFailedPayload struct {
	SessionID string `json:"session_id"`
	FileID    int    `json:"file_id"`
	JobID     int64  `json:"job_id"`
	TrackKey  string `json:"track_key"`
	Message   string `json:"message,omitempty"`
}

// SourceCommittedPayload publishes the effective version a transport has
// committed to and its declared track inventory. It is sent at commit time,
// before the background probe has produced verified evidence: AudioTracks is
// provider-declared metadata (possibly empty), and InventoryStatus says so.
// Clients use it to follow a serve-layer rotation immediately; a later
// inventory poll or replan upgrades the list to probe evidence.
type SourceCommittedPayload struct {
	SessionID string `json:"session_id"`
	// EffectiveMediaFileID is the catalog row of the committed release.
	EffectiveMediaFileID int `json:"effective_media_file_id,omitempty"`
	// EffectiveVirtualURI is the provider-neutral candidate URI the session is
	// bound to, and is the identity the version menu keys on.
	EffectiveVirtualURI string `json:"effective_virtual_uri,omitempty"`
	// VirtualSourceRevision is the plan's opaque media-generation revision. It
	// is empty after a rotation (the sibling is not probed yet).
	VirtualSourceRevision string `json:"virtual_source_revision,omitempty"`
	// InventoryStatus is "declared" or "verified", matching the plan's field.
	InventoryStatus string `json:"inventory_status,omitempty"`
	// AudioTracks is the committed release's audio inventory. It is declared
	// metadata until a probe upgrades it. The tag deliberately has no
	// omitempty: an empty inventory must encode as [] so a client can tell
	// "this release declares no audio tracks" from a missing field.
	AudioTracks []AudioInventoryItemV3 `json:"audio_tracks"`
}

// InventoryUpdatedPayload is the probe-verified track inventory of a live
// playback session, pushed when the background probe persists evidence for the
// release that session is bound to.
//
// Its wire shape is exactly the GET /api/v2/playback/{session_id}/inventory
// body (PlaybackInventoryV3, embedded here so the two cannot drift apart). It
// carries the session id, the audio and subtitle lists, and the inventory
// status and revision. A client applies it with the same reducer it uses for an
// inventory poll and gates on InventoryRevision: a duplicate push, or one that
// names the revision the client already holds, is a no-op.
type InventoryUpdatedPayload struct {
	PlaybackInventoryV3
}

// NewEventEnvelope creates a validated realtime event envelope.
func NewEventEnvelope(sessionID string, name RealtimeEventName, payload json.RawMessage) (EventEnvelope, error) {
	normalizedPayload, err := normalizeJSONPayload(payload)
	if err != nil {
		return EventEnvelope{}, err
	}
	env := EventEnvelope{
		Type:      RealtimeMessageTypeEvent,
		SessionID: sessionID,
		Name:      name,
		Payload:   normalizedPayload,
	}
	if err := env.Validate(); err != nil {
		return EventEnvelope{}, err
	}
	return env, nil
}

// NewChapterThumbnailReadyEvent creates a validated chapter thumbnail event.
func NewChapterThumbnailReadyEvent(
	sessionID string,
	fileID int,
	chapterIndex int,
	thumbnailURL string,
	thumbnailThumbhash string,
) (EventEnvelope, error) {
	payload, err := json.Marshal(ChapterThumbnailReadyPayload{
		SessionID:          sessionID,
		FileID:             fileID,
		ChapterIndex:       chapterIndex,
		ThumbnailURL:       thumbnailURL,
		ThumbnailThumbhash: thumbnailThumbhash,
	})
	if err != nil {
		return EventEnvelope{}, err
	}
	return NewEventEnvelope(sessionID, RealtimeEventChapterThumbnailReady, payload)
}

func NewMarkersUpdatedEvent(
	sessionID string,
	fileID int,
	intro *TimeRangePayload,
	credits *TimeRangePayload,
	recap *TimeRangePayload,
	preview *TimeRangePayload,
	segments ...models.MarkerSegment,
) (EventEnvelope, error) {
	if segments == nil {
		segments = []models.MarkerSegment{}
	}
	payload, err := json.Marshal(MarkersUpdatedPayload{
		SessionID:      sessionID,
		FileID:         fileID,
		Intro:          intro,
		Credits:        credits,
		Recap:          recap,
		Preview:        preview,
		MarkerSegments: segments,
	})
	if err != nil {
		return EventEnvelope{}, err
	}
	return NewEventEnvelope(sessionID, RealtimeEventMarkersUpdated, payload)
}

// NewSubtitleReadyEvent creates a validated subtitle-ready event.
func NewSubtitleReadyEvent(
	sessionID string,
	fileID int,
	subtitleID int,
	language string,
	label string,
	track *SubtitleInventoryItemV3,
) (EventEnvelope, error) {
	payload, err := json.Marshal(SubtitleReadyPayload{
		SessionID:  sessionID,
		FileID:     fileID,
		SubtitleID: subtitleID,
		Language:   language,
		Label:      label,
		Track:      track,
	})
	if err != nil {
		return EventEnvelope{}, err
	}
	return NewEventEnvelope(sessionID, RealtimeEventSubtitleReady, payload)
}

// SubtitleTimingChangedPayload tells a player that a stored subtitle of its
// file was retimed (automatic sync or a manual adjustment). A player showing
// that track fetches it again; its stream URL already serves the new timing.
type SubtitleTimingChangedPayload struct {
	SessionID  string `json:"session_id"`
	FileID     int    `json:"file_id"`
	SubtitleID int    `json:"subtitle_id"`
	// Track identifies the retimed track in the session's inventory, as in
	// SubtitleReadyPayload. Absent when the inventory could not be resolved.
	Track *SubtitleInventoryItemV3 `json:"track,omitempty"`
}

// NewSubtitleTimingChangedEvent creates a validated subtitle-timing event.
func NewSubtitleTimingChangedEvent(sessionID string, fileID, subtitleID int, track *SubtitleInventoryItemV3) (EventEnvelope, error) {
	payload, err := json.Marshal(SubtitleTimingChangedPayload{
		SessionID: sessionID, FileID: fileID, SubtitleID: subtitleID, Track: track,
	})
	if err != nil {
		return EventEnvelope{}, err
	}
	return NewEventEnvelope(sessionID, RealtimeEventSubtitleTimingChanged, payload)
}

// NewSubtitleTranslationStartedEvent creates a validated translation-started event.
func NewSubtitleTranslationStartedEvent(sessionID string, fileID int, jobID int64, trackKey, language, label string, totalCues int) (EventEnvelope, error) {
	payload, err := json.Marshal(SubtitleTranslationStartedPayload{
		SessionID: sessionID, FileID: fileID, JobID: jobID,
		TrackKey: trackKey, Language: language, Label: label, TotalCues: totalCues,
	})
	if err != nil {
		return EventEnvelope{}, err
	}
	return NewEventEnvelope(sessionID, RealtimeEventSubtitleTranslationStart, payload)
}

// NewSubtitleTranslationCuesEvent creates a validated translation-cues event.
func NewSubtitleTranslationCuesEvent(sessionID string, fileID int, jobID int64, trackKey string, cues []StreamCue, done, total int) (EventEnvelope, error) {
	payload, err := json.Marshal(SubtitleTranslationCuesPayload{
		SessionID: sessionID, FileID: fileID, JobID: jobID,
		TrackKey: trackKey, Cues: cues, Done: done, Total: total,
	})
	if err != nil {
		return EventEnvelope{}, err
	}
	return NewEventEnvelope(sessionID, RealtimeEventSubtitleTranslationCues, payload)
}

// NewSubtitleTranslationCompletedEvent creates a validated translation-completed event.
func NewSubtitleTranslationCompletedEvent(sessionID string, fileID int, jobID int64, trackKey string, subtitleID int, language, label string, track *SubtitleInventoryItemV3) (EventEnvelope, error) {
	payload, err := json.Marshal(SubtitleTranslationCompletedPayload{
		SessionID: sessionID, FileID: fileID, JobID: jobID,
		TrackKey: trackKey, SubtitleID: subtitleID, Language: language, Label: label, Track: track,
	})
	if err != nil {
		return EventEnvelope{}, err
	}
	return NewEventEnvelope(sessionID, RealtimeEventSubtitleTranslationDone, payload)
}

// NewSubtitleTranslationFailedEvent creates a validated translation-failed event.
func NewSubtitleTranslationFailedEvent(sessionID string, fileID int, jobID int64, trackKey, message string) (EventEnvelope, error) {
	payload, err := json.Marshal(SubtitleTranslationFailedPayload{
		SessionID: sessionID, FileID: fileID, JobID: jobID, TrackKey: trackKey, Message: message,
	})
	if err != nil {
		return EventEnvelope{}, err
	}
	return NewEventEnvelope(sessionID, RealtimeEventSubtitleTranslationFail, payload)
}

// NewSourceCommittedEvent creates a validated effective-source event. The
// session id must be non-empty; a missing identity is allowed (the client then
// only refreshes its inventory) but a nil audio list is normalized to an empty
// one so the payload shape is stable.
func NewSourceCommittedEvent(sessionID string, payload SourceCommittedPayload) (EventEnvelope, error) {
	if sessionID == "" {
		return EventEnvelope{}, ErrInvalidRealtimePayload
	}
	payload.SessionID = sessionID
	if payload.AudioTracks == nil {
		payload.AudioTracks = []AudioInventoryItemV3{}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return EventEnvelope{}, err
	}
	return NewEventEnvelope(sessionID, RealtimeEventSourceCommitted, raw)
}

// NewInventoryUpdatedEvent creates a validated inventory_updated event. The
// session id must be non-empty; nil audio and subtitle slices are normalized to
// empty arrays so the payload matches the inventory endpoint's shape and a
// client can tell "this release has no such tracks" from a missing field.
func NewInventoryUpdatedEvent(sessionID string, inventory PlaybackInventoryV3) (EventEnvelope, error) {
	if sessionID == "" {
		return EventEnvelope{}, ErrInvalidRealtimePayload
	}
	inventory.SessionID = sessionID
	if inventory.AudioTracks == nil {
		inventory.AudioTracks = []AudioInventoryItemV3{}
	}
	if inventory.SubtitleInventory == nil {
		inventory.SubtitleInventory = []SubtitleInventoryItemV3{}
	}
	raw, err := json.Marshal(InventoryUpdatedPayload{PlaybackInventoryV3: inventory})
	if err != nil {
		return EventEnvelope{}, err
	}
	return NewEventEnvelope(sessionID, RealtimeEventInventoryUpdated, raw)
}

// DownloadProgressState is the lifecycle stage a download.progress event
// reports for one cache fill.
type DownloadProgressState string

const (
	// DownloadProgressStateQueued means the provider accepted the fill but has
	// not reported bytes yet.
	DownloadProgressStateQueued DownloadProgressState = "queued"
	// DownloadProgressStateDownloading means the provider is fetching the
	// release and reported byte progress.
	DownloadProgressStateDownloading DownloadProgressState = "downloading"
	// DownloadProgressStateCompleted means the provider finished the fill and
	// the cached copy is servable.
	DownloadProgressStateCompleted DownloadProgressState = "completed"
	// DownloadProgressStateFailed means the fill could not start or aborted.
	// It is retryable: the session keeps streaming the remote source.
	DownloadProgressStateFailed DownloadProgressState = "failed"
)

// DownloadProgressPayload describes the cache-fill progress of one release a
// playback session is pinned to. Bytes, TotalBytes and Percent are the
// provider's reported counts; Percent is clamped to [0,100] and derived from
// the byte counts when the caller leaves it zero. Message is an operator-safe
// reason on a failed fill and never carries a provider URL.
type DownloadProgressPayload struct {
	SessionID  string                `json:"session_id"`
	FileID     int                   `json:"file_id,omitempty"`
	ReleaseID  string                `json:"release_id,omitempty"`
	State      DownloadProgressState `json:"state"`
	Percent    float64               `json:"percent"`
	Bytes      int64                 `json:"bytes,omitempty"`
	TotalBytes int64                 `json:"total_bytes,omitempty"`
	Message    string                `json:"message,omitempty"`
}

// NewDownloadProgressEvent creates a validated download.progress event. An
// empty state defaults to queued; a negative or over-100 percent is clamped,
// and a zero percent with a known total is derived from the byte counts so
// clients never have to duplicate the arithmetic.
func NewDownloadProgressEvent(sessionID string, payload DownloadProgressPayload) (EventEnvelope, error) {
	if sessionID == "" {
		return EventEnvelope{}, ErrInvalidRealtimePayload
	}
	payload.SessionID = sessionID
	if payload.State == "" {
		payload.State = DownloadProgressStateQueued
	}
	if payload.Percent == 0 && payload.TotalBytes > 0 && payload.Bytes > 0 {
		payload.Percent = float64(payload.Bytes) / float64(payload.TotalBytes) * 100
	}
	if payload.Percent < 0 {
		payload.Percent = 0
	} else if payload.Percent > 100 {
		payload.Percent = 100
	}
	if payload.State == DownloadProgressStateCompleted {
		payload.Percent = 100
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return EventEnvelope{}, err
	}
	return NewEventEnvelope(sessionID, RealtimeEventDownloadProgress, raw)
}

// ParseEventEnvelope decodes and validates a realtime event envelope.
func ParseEventEnvelope(data []byte) (EventEnvelope, error) {
	var env EventEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return EventEnvelope{}, err
	}
	normalizedPayload, err := normalizeJSONPayload(env.Payload)
	if err != nil {
		return EventEnvelope{}, err
	}
	env.Payload = normalizedPayload
	if err := env.Validate(); err != nil {
		return EventEnvelope{}, err
	}
	return env, nil
}

// Validate checks the event envelope shape.
func (e *EventEnvelope) Validate() error {
	if e == nil {
		return ErrInvalidRealtimePayload
	}
	if e.Type != RealtimeMessageTypeEvent {
		return fmt.Errorf("event envelope type must be %q", RealtimeMessageTypeEvent)
	}
	if e.SessionID == "" {
		return ErrInvalidRealtimePayload
	}
	if len(e.Payload) == 0 {
		e.Payload = json.RawMessage(`{}`)
	} else if !json.Valid(e.Payload) {
		return ErrInvalidRealtimePayload
	}
	if _, ok := supportedRealtimeEventNameSet[e.Name]; !ok {
		return ErrUnsupportedEventName
	}
	return nil
}

// CommandEnvelope is the server-to-client command message.
type CommandEnvelope struct {
	Type       RealtimeMessageType `json:"type"`
	CommandID  string              `json:"command_id"`
	SessionID  string              `json:"session_id"`
	Name       CommandName         `json:"name"`
	Reason     string              `json:"reason,omitempty"`
	IssuedBy   *CommandIssuedBy    `json:"issued_by,omitempty"`
	DeadlineMS int                 `json:"deadline_ms,omitempty"`
	Payload    json.RawMessage     `json:"payload,omitempty"`
}

// CommandIssuedBy identifies the source of a command.
type CommandIssuedBy struct {
	Kind string `json:"kind"`
}

// PlanInvalidationReason values a plan_invalidated command can carry.
const (
	// PlanInvalidatedVideoCopyUnsafe means the asynchronous H.264 copy-safety
	// scan came back multi-PPS after the plan was already playing, so the video
	// stream-copy route it named cannot serve this source.
	PlanInvalidatedVideoCopyUnsafe = "video_copy_unsafe"
)

// PlanInvalidatedPayload names the plan the server withdrew and why.
//
// PlanID is required: the client compares it against the plan it is running and
// does nothing when it has already replanned past it, so a late command can
// never evict a route the server never complained about.
type PlanInvalidatedPayload struct {
	Reason string `json:"reason"`
	PlanID string `json:"plan_id"`
}

// NewPlanInvalidatedCommand builds a validated plan_invalidated command.
func NewPlanInvalidatedCommand(sessionID, commandID, planID, reason string) (CommandEnvelope, error) {
	if planID == "" || reason == "" {
		return CommandEnvelope{}, ErrInvalidRealtimePayload
	}
	payload, err := json.Marshal(PlanInvalidatedPayload{Reason: reason, PlanID: planID})
	if err != nil {
		return CommandEnvelope{}, err
	}
	return NewCommandEnvelope(sessionID, commandID, CommandPlanInvalidated, payload)
}

// NewCommandEnvelope creates a validated command envelope.
func NewCommandEnvelope(sessionID, commandID string, name CommandName, payload json.RawMessage) (CommandEnvelope, error) {
	normalizedPayload, err := normalizeJSONPayload(payload)
	if err != nil {
		return CommandEnvelope{}, err
	}
	env := CommandEnvelope{
		Type:      RealtimeMessageTypeCommand,
		CommandID: commandID,
		SessionID: sessionID,
		Name:      name,
		Payload:   normalizedPayload,
	}
	if err := env.Validate(); err != nil {
		return CommandEnvelope{}, err
	}
	return env, nil
}

// ParseCommandEnvelope decodes and validates a command envelope.
func ParseCommandEnvelope(data []byte) (CommandEnvelope, error) {
	var env CommandEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return CommandEnvelope{}, err
	}
	normalizedPayload, err := normalizeJSONPayload(env.Payload)
	if err != nil {
		return CommandEnvelope{}, err
	}
	env.Payload = normalizedPayload
	if err := env.Validate(); err != nil {
		return CommandEnvelope{}, err
	}
	return env, nil
}

// Validate checks the envelope for required fields and supported command names.
func (e *CommandEnvelope) Validate() error {
	if e == nil {
		return ErrInvalidRealtimePayload
	}
	if e.Type != RealtimeMessageTypeCommand {
		return fmt.Errorf("command envelope type must be %q", RealtimeMessageTypeCommand)
	}
	if e.CommandID == "" || e.SessionID == "" {
		return ErrInvalidRealtimePayload
	}
	if len(e.Payload) == 0 {
		e.Payload = json.RawMessage(`{}`)
	} else if !json.Valid(e.Payload) {
		return ErrInvalidRealtimePayload
	}
	if err := ValidateCommandName(e.Name); err != nil {
		return err
	}
	return nil
}

// ValidateCommandName reports whether a command is supported.
func ValidateCommandName(name CommandName) error {
	if _, ok := supportedCommandNameSet[name]; !ok {
		return ErrUnsupportedCommandName
	}
	return nil
}

// SupportedCommandNames returns a copy of the supported command names.
func SupportedCommandNames() []CommandName {
	out := make([]CommandName, len(supportedCommandNames))
	copy(out, supportedCommandNames)
	return out
}

// HelloEnvelope is the client hello message.
type HelloEnvelope struct {
	Type         RealtimeMessageType `json:"type"`
	SessionID    string              `json:"session_id"`
	Client       HelloClientInfo     `json:"client"`
	Capabilities HelloCapabilities   `json:"capabilities"`
}

// HelloClientInfo identifies the client implementation.
type HelloClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// HelloCapabilities lists supported commands.
type HelloCapabilities struct {
	Commands []CommandName `json:"commands"`
}

// Validate checks the hello envelope shape and capability names.
func (e HelloEnvelope) Validate() error {
	if e.Type != RealtimeMessageTypeHello {
		return fmt.Errorf("hello envelope type must be %q", RealtimeMessageTypeHello)
	}
	if e.SessionID == "" || e.Client.Name == "" || e.Client.Version == "" {
		return ErrInvalidRealtimePayload
	}
	for _, name := range e.Capabilities.Commands {
		if err := ValidateCommandName(name); err != nil {
			return err
		}
	}
	return nil
}

// AckEnvelope is the client acknowledgement message.
type AckEnvelope struct {
	Type      RealtimeMessageType `json:"type"`
	CommandID string              `json:"command_id"`
	SessionID string              `json:"session_id"`
	Status    RealtimeAckStatus   `json:"status"`
}

// Validate checks the ack envelope shape.
func (e AckEnvelope) Validate() error {
	if e.Type != RealtimeMessageTypeAck {
		return fmt.Errorf("ack envelope type must be %q", RealtimeMessageTypeAck)
	}
	if e.CommandID == "" || e.SessionID == "" || e.Status == "" {
		return ErrInvalidRealtimePayload
	}
	if e.Status != RealtimeAckStatusAccepted {
		return ErrInvalidRealtimePayload
	}
	return nil
}

// ResultEnvelope is the client completion message.
type ResultEnvelope struct {
	Type      RealtimeMessageType  `json:"type"`
	CommandID string               `json:"command_id"`
	SessionID string               `json:"session_id"`
	Status    RealtimeResultStatus `json:"status"`
	Error     string               `json:"error,omitempty"`
}

// Validate checks the result envelope shape.
func (e ResultEnvelope) Validate() error {
	if e.Type != RealtimeMessageTypeResult {
		return fmt.Errorf("result envelope type must be %q", RealtimeMessageTypeResult)
	}
	if e.CommandID == "" || e.SessionID == "" || e.Status == "" {
		return ErrInvalidRealtimePayload
	}
	switch e.Status {
	case RealtimeResultStatusCompleted, RealtimeResultStatusRejected:
		return nil
	default:
		return ErrInvalidRealtimePayload
	}
}

func normalizeJSONPayload(payload json.RawMessage) (json.RawMessage, error) {
	if len(payload) == 0 {
		return json.RawMessage(`{}`), nil
	}
	if !json.Valid(payload) {
		return nil, ErrInvalidRealtimePayload
	}
	return payload, nil
}
