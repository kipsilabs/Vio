package api

import (
	"time"

	"github.com/Silo-Server/silo-server/internal/api/handlers"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

// virtualPlaybackStreamsFromCore converts the core virtual library's candidate
// records into the playback handler's port shape. It is shared by the cached
// playback lister and the explicit "refresh list" lister so the two cannot
// drift on which provider fields cross the boundary.
func virtualPlaybackStreamsFromCore(streams []virtuallibrary.PlaybackStream) []handlers.VirtualPlaybackStream {
	out := make([]handlers.VirtualPlaybackStream, 0, len(streams))
	for _, stream := range streams {
		var providerExpiresAt *time.Time
		if !stream.ExpiresAt.IsZero() {
			expiresAt := stream.ExpiresAt
			providerExpiresAt = &expiresAt
		}
		out = append(out, handlers.VirtualPlaybackStream{
			ID: stream.ID, Label: stream.Label, URI: stream.URI, Resolution: stream.Resolution,
			CodecVideo: stream.CodecVideo, CodecAudio: stream.CodecAudio, HasAtmos: stream.HasAtmos,
			QualityScore: stream.QualityScore, RequestHeaders: stream.RequestHeaders,
			HDR: stream.HDR, SourceType: stream.SourceType, FileSize: stream.FileSize, Container: stream.Container,
			Bitrate: stream.Bitrate, FrameRate: stream.FrameRate, AudioLanguages: stream.AudioLanguages,
			SubtitleLanguages: stream.SubtitleLanguages, AudioChannels: stream.AudioChannels,
			Is10Bit: stream.Is10Bit, IsMultiAudio: stream.IsMultiAudio,
			VisualTags: stream.VisualTags, AudioTags: stream.AudioTags,
			OwnerInstallationID: stream.OwnerInstallationID,
			Visible:             stream.Visible, VisibilitySpecified: stream.VisibilitySpecified,
			Rejected:              stream.Rejected,
			PreserveProviderOrder: stream.PreserveProviderOrder,

			ProviderURL:         stream.ProviderURL,
			ProviderVideoHash:   stream.ProviderVideoHash,
			ProviderGUID:        stream.ProviderGUID,
			ProviderReleaseName: stream.ProviderReleaseName,
			ProviderExpiresAt:   providerExpiresAt,
		})
	}
	return out
}
