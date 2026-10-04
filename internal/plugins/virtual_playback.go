package plugins

import (
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

// VirtualPlaybackVariant is a provider-neutral profile placeholder returned
// by a virtual playback plugin. It is safe to request during collection sync:
// no upstream streaming provider is contacted until playback.
//
// Provenance is the explicit origin discriminator: "plugin" marks the plugin
// path, "core" marks core-stamped variants (Phase 4), and "" means
// legacy/unknown. The zero value stays fail-open on the legacy plugin path;
// the NEW core SSRF dispatch fails closed on unknown/inconsistent provenance.
// No DB column backs this in Phase 2 — struct-level only; the core Variants()
// path (Phase 4) stamps Provenance:"core".
type VirtualPlaybackVariant struct {
	VirtualURI          string                   `json:"virtual_uri"`
	Label               string                   `json:"label"`
	Resolution          string                   `json:"resolution,omitempty"`
	CodecVideo          string                   `json:"codec_video,omitempty"`
	CodecAudio          string                   `json:"codec_audio,omitempty"`
	HDR                 string                   `json:"hdr,omitempty"`
	OwnerInstallationID int                      `json:"-"`
	VirtualProvenance   models.VirtualProvenance `json:"-"`
}

// VirtualPlaybackStream is a provider result exposed as a temporary, stable
// virtual file. The provider URL is deliberately not returned or persisted;
// it is resolved again when the selected file is played.
type VirtualPlaybackStream struct {
	ID                  string            `json:"id"`
	Label               string            `json:"label"`
	URI                 string            `json:"uri"`
	Resolution          string            `json:"resolution,omitempty"`
	CodecVideo          string            `json:"codec_video,omitempty"`
	CodecAudio          string            `json:"codec_audio,omitempty"`
	HDR                 string            `json:"hdr,omitempty"`
	SourceType          string            `json:"source_type,omitempty"`
	FileSize            int64             `json:"file_size,omitempty"`
	Container           string            `json:"container,omitempty"`
	Bitrate             int               `json:"bitrate,omitempty"`
	FrameRate           string            `json:"frame_rate,omitempty"`
	AudioLanguages      []string          `json:"audio_languages,omitempty"`
	SubtitleLanguages   []string          `json:"subtitle_languages,omitempty"`
	HasAtmos            bool              `json:"has_atmos,omitempty"`
	QualityScore        int               `json:"quality_score,omitempty"`
	RequestHeaders      map[string]string `json:"-"`
	ExpiresAt           time.Time         `json:"-"`
	OwnerInstallationID int               `json:"-"`
	Visible             bool              `json:"-"`
	VisibilitySpecified bool              `json:"-"`
	// Rejected marks a candidate a configured custom format rejects. It is a
	// transient ranking signal (rank-last, last-resort selectable), never part
	// of the wire shape.
	Rejected bool `json:"-"`
}

// Get* accessors satisfy plugins.VirtualStreamMetadata so the shared device
// ranker can score both this type and the handler-layer candidate shape.
func (s VirtualPlaybackStream) GetCodecVideo() string { return s.CodecVideo }
func (s VirtualPlaybackStream) GetCodecAudio() string { return s.CodecAudio }
func (s VirtualPlaybackStream) GetHDR() string        { return s.HDR }
func (s VirtualPlaybackStream) GetContainer() string  { return s.Container }
func (s VirtualPlaybackStream) GetResolution() string { return s.Resolution }
func (s VirtualPlaybackStream) GetHasAtmos() bool     { return s.HasAtmos }
func (s VirtualPlaybackStream) GetQualityScore() int  { return s.QualityScore }

// ResolvedVirtualStream contains the complete outcome of resolving a virtual media stream,
// including the provider stream URL, canonical URI, request headers, candidate ID, and expiration.
type ResolvedVirtualStream struct {
	URL            string
	URI            string
	CandidateID    string
	RequestHeaders map[string]string
	ExpiresAt      time.Time
	// OwnerID is the plugin installation that actually served the candidate.
	// It is the runtime inheritance for a virtual file row whose stored owner
	// is 0: the catalog treats owner 0 as "inherit from the parent item", and
	// the provider that answers is that owner. Callers use it to make the
	// allow_private_streams decision for the provider that served the stream.
	OwnerID int
}
