package playback

import (
	"strings"
)

// FailureClassificationAudioRendererErrorV3 is the client failure
// classification for an audio renderer that failed to open, initialize, or
// write. It is deliberately not a transport classification: the video route
// may be sound while the selected audio shape is not playable on the device.
const FailureClassificationAudioRendererErrorV3 = "audio_renderer_error"

// audioInitErrorCodesV3 are the Media3 PlaybackException codes a client reports
// when its audio sink cannot initialize or accept a track. They indict the
// audio output, not the bytes: the same file can play once its audio is
// re-shaped for the device.
var audioInitErrorCodesV3 = map[string]bool{
	"5001": true, // ERROR_CODE_AUDIO_TRACK_INIT_FAILED
	"5003": true, // ERROR_CODE_AUDIO_TRACK_OFFLOAD_WRITE_FAILED
	"5004": true, // ERROR_CODE_AUDIO_TRACK_OFFLOAD_INIT_FAILED
}

// The failure-diagnostics keys that can carry the player's audio-output
// evidence. They mirror the route-event allowlist so a client can send the same
// keys in either place.
const (
	diagnosticKeyErrorCodeV3     = "error_code"
	diagnosticKeyErrorCodeNameV3 = "error_code_name"
)

var (
	audioInitCodeKeysV3 = []string{diagnosticKeyErrorCodeV3, "media3_error_code", "code"}
	audioInitNameKeysV3 = []string{diagnosticKeyErrorCodeNameV3, "error_name", "error_cause"}
)

// audioInitNameFragmentsV3 are the Media3 audio-output failure names and the
// player messages that carry them, normalized by audioInitSearchKeyV3.
var audioInitNameFragmentsV3 = []string{
	"audio track init failed",
	"audiotrack init failed",
	"audio track offload init failed",
	"audiotrack offload init failed",
	"audio track offload write failed",
	"audiotrack offload write failed",
	"audio track write failed",
	"audiotrack write failed",
	"audio renderer init failed",
	"audio renderer error",
}

// audioInitSearchKeyV3 lowercases a diagnostic string and folds the separator
// variants Media3 and client wrappers use (AUDIO_TRACK_INIT_FAILED,
// AudioTrack init failed, audio-track-init-failed) into one searchable form.
func audioInitSearchKeyV3(value string) string {
	replacer := strings.NewReplacer("_", " ", "-", " ", ".", " ", ":", " ")
	return strings.Join(strings.Fields(strings.ToLower(replacer.Replace(value))), " ")
}

// AudioInitFailureDetailV3 returns a short diagnostic detail naming an audio
// output initialization failure, or "" when the failure carries no such
// evidence. Structured diagnostics win over free-text message parsing because
// they carry the player's own code and name; the message path keeps a client
// that only reports the exception text working.
//
// A bare audio_renderer_error classification is accepted as family evidence:
// it names the audio output without a code, which is enough to try a different
// audio shape but not enough to indict the delivery on its own.
func AudioInitFailureDetailV3(f FailureV3) string {
	for _, key := range audioInitCodeKeysV3 {
		code := strings.TrimSpace(f.Diagnostics[key])
		if audioInitErrorCodesV3[code] {
			name := strings.TrimSpace(f.Diagnostics[diagnosticKeyErrorCodeNameV3])
			if name == "" {
				name = strings.TrimSpace(f.Diagnostics["error_name"])
			}
			if name != "" {
				return "media3 code " + code + " " + name
			}
			return "media3 code " + code
		}
	}
	for _, key := range audioInitNameKeysV3 {
		if detail := audioInitTextDetailV3(f.Diagnostics[key]); detail != "" {
			return detail
		}
	}
	if detail := audioInitTextDetailV3(f.Message); detail != "" {
		return detail
	}
	if strings.EqualFold(strings.TrimSpace(f.Classification), FailureClassificationAudioRendererErrorV3) {
		return FailureClassificationAudioRendererErrorV3
	}
	return ""
}

func audioInitTextDetailV3(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	key := audioInitSearchKeyV3(trimmed)
	for _, fragment := range audioInitNameFragmentsV3 {
		if strings.Contains(key, fragment) {
			if len(trimmed) > 160 {
				trimmed = trimmed[:160]
			}
			return "player reported: " + trimmed
		}
	}
	return ""
}
