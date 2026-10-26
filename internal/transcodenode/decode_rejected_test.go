package transcodenode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/playback"
)

// writeDecodeRejectingFFmpeg writes a fake ffmpeg that emits repeated
// invalid-bitstream decoder failure lines from the first frame and then idles,
// so a real TranscodeSession records the source rejection without producing any
// media.
func writeDecodeRejectingFFmpeg(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-ffmpeg-decode-reject.sh")
	script := "#!/bin/sh\n" +
		"i=0\n" +
		"while [ $i -lt 12 ]; do echo \"[hevc @ 0x1] Error submitting packet to decoder: Invalid data found when processing input\" >&2; i=$((i+1)); done\n" +
		"sleep 30\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}
	return path
}

// TestNodeMediaRoutesReportDecoderRejectedSource proves the transcode-node
// serve path answers the same permanent decode verdict as the API server, so a
// clustered deployment's proxy surfaces it to the player rather than letting it
// retry a stream this node cannot decode.
func TestNodeMediaRoutesReportDecoderRejectedSource(t *testing.T) {
	const sessionID = "decode-rejected-session"
	server := newTestServer(t)
	session, err := playback.StartTranscode(context.Background(), playback.TranscodeOpts{
		SessionID:               sessionID,
		OutputDir:               t.TempDir(),
		FFmpegPath:              writeDecodeRejectingFFmpeg(t),
		TargetCodecVideo:        "h264",
		TargetCodecAudio:        "aac",
		SegmentDuration:         2,
		SegmentRetentionSeconds: 600,
	})
	if err != nil {
		t.Fatalf("StartTranscode: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	server.sessions[sessionID] = session
	server.lastAccess[sessionID] = time.Now()

	deadline := time.Now().Add(5 * time.Second)
	for !session.IsSourceRejected() {
		if time.Now().After(deadline) {
			t.Fatal("the decoder rejection was never observed on the node session")
		}
		time.Sleep(5 * time.Millisecond)
	}

	for _, tc := range []struct {
		name   string
		handle func(http.ResponseWriter, *http.Request)
		params map[string]string
		target string
	}{
		{
			name:   "manifest",
			handle: server.handleManifest,
			params: map[string]string{"session_id": sessionID},
			target: "/transcode/" + sessionID + "/master.m3u8",
		},
		{
			name:   "segment",
			handle: server.handleSegment,
			params: map[string]string{"session_id": sessionID, "name": "seg_0.ts"},
			target: "/transcode/" + sessionID + "/segment/seg_0.ts",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			tc.handle(rr, withNodeRouteParams(httptest.NewRequest(http.MethodGet, tc.target, nil), tc.params))
			if rr.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want %d; body = %q", rr.Code, http.StatusUnprocessableEntity, rr.Body.String())
			}
			if got := rr.Header().Get(playback.DecodeErrorHeader); got != playback.DecodeErrorSourceRejectedCode {
				t.Fatalf("%s = %q, want %q", playback.DecodeErrorHeader, got, playback.DecodeErrorSourceRejectedCode)
			}
		})
	}
}

// TestNodeSegmentLatchedDecodeRejectionAnswersUnprocessableEntity proves a
// latched async-restart decode failure surfaces the permanent decode verdict
// (422) on the segment path instead of a 404 that would invite the player to
// retry a source this node cannot decode.
func TestNodeSegmentLatchedDecodeRejectionAnswersUnprocessableEntity(t *testing.T) {
	const sessionID = "decode-rejected-latched-session"
	server := newTestServer(t)
	session, err := playback.StartTranscode(context.Background(), playback.TranscodeOpts{
		SessionID:               sessionID,
		OutputDir:               t.TempDir(),
		FFmpegPath:              writeNodeFarSeekAnyStartFFmpeg(t),
		TargetCodecVideo:        "h264",
		TargetCodecAudio:        "aac",
		SegmentDuration:         2,
		SegmentRetentionSeconds: 600,
	})
	if err != nil {
		t.Fatalf("StartTranscode: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	server.sessions[sessionID] = session
	server.lastAccess[sessionID] = time.Now()

	session.RecordAsyncRestartFailure(playback.ErrSourceDecodeRejected)

	target := "/transcode/" + sessionID + "/segment/seg_0.ts"
	rr := httptest.NewRecorder()
	server.handleSegment(rr, withNodeRouteParams(httptest.NewRequest(http.MethodGet, target, nil), map[string]string{"session_id": sessionID, "name": "seg_0.ts"}))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d; body = %q", rr.Code, http.StatusUnprocessableEntity, rr.Body.String())
	}
	if got := rr.Header().Get(playback.DecodeErrorHeader); got != playback.DecodeErrorSourceRejectedCode {
		t.Fatalf("%s = %q, want %q", playback.DecodeErrorHeader, got, playback.DecodeErrorSourceRejectedCode)
	}
}
