package keyframes

import (
	"bytes"
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// ReadHTTP must produce the same keyframe index as the local reader while
// fetching only the small metadata ranges. The same source served by an origin
// that ignores Range must still work, by reading through the prefix.
func TestReadHTTPReadsTheMatroskaIndexOverRange(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "indexed.mkv")
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, ffmpeg, "-v", "error",
		"-f", "lavfi", "-i", "testsrc2=size=96x54:rate=25:duration=20",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "1000", "-bf", "2",
		"-sc_threshold", "0", "-force_key_frames", "0,2.2,5.2,7.2,12.6,15,18.5",
		source).CombinedOutput()
	if err != nil {
		t.Skipf("ffmpeg can't make the test file (%v): %s", err, out)
	}
	want, err := LoadVerified(ctx, source)
	if err != nil {
		t.Fatal(err)
	}

	serve := func(ignoreRange bool) *httptest.Server {
		content, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ignoreRange {
				r.Header.Del("Range")
			}
			http.ServeContent(w, r, "source.mkv", time.Time{}, bytes.NewReader(content))
		}))
	}

	for name, ignoreRange := range map[string]bool{"range": false, "origin ignores range": true} {
		t.Run(name, func(t *testing.T) {
			server := serve(ignoreRange)
			defer server.Close()
			got, err := ReadHTTP(ctx, server.Client(), server.URL+"/source.mkv")
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Keyframes) != len(want.Keyframes) {
				t.Fatalf("keyframes = %v, want %v", got.Keyframes, want.Keyframes)
			}
			for i := range want.Keyframes {
				if math.Abs(got.Keyframes[i]-want.Keyframes[i]) > 1e-6 {
					t.Fatalf("keyframe %d = %v, want %v", i, got.Keyframes[i], want.Keyframes[i])
				}
			}
			if math.Abs(got.Duration-want.Duration) > 1e-6 {
				t.Fatalf("duration = %v, want %v", got.Duration, want.Duration)
			}
		})
	}
}
