package keyframes

import (
	"bytes"
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// ReadHTTP must produce the same keyframe index as the local reader while
// fetching only the small metadata ranges. The same source served by an origin
// that ignores Range is declined: reading it through prefix discards would
// stream the file from zero for every metadata read.
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
				if r.Method == http.MethodHead {
					// Force the size probe through its Range request so this
					// test covers the probe's non-Range signal.
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				r.Header.Del("Range")
			}
			http.ServeContent(w, r, "source.mkv", time.Time{}, bytes.NewReader(content))
		}))
	}

	t.Run("range", func(t *testing.T) {
		server := serve(false)
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

	t.Run("origin ignores range", func(t *testing.T) {
		server := serve(true)
		defer server.Close()
		if _, err := ReadHTTP(ctx, server.Client(), server.URL+"/source.mkv"); !errors.Is(err, ErrNoIndex) {
			t.Fatalf("ReadHTTP on a range-ignoring origin = %v, want ErrNoIndex", err)
		} else if errors.Is(err, ErrTransient) {
			t.Fatal("a range-ignoring origin is a stable decline, not a retryable read failure")
		}
	})
}

// A read near EOF must not stream the prefix it would have to discard. Before
// the decline, an origin that ignores Range answered 200 and the reader copied
// `off` bytes (a cue read at the end of a multi-GB file costs the whole file),
// a cost the read budget never saw because it counts only the window.
func TestHTTPRangeReaderDeclinesRangeIgnoringOriginWithoutDownloadingPrefix(t *testing.T) {
	const size = 64 << 20
	var served atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		buf := make([]byte, 32<<10)
		for written := int64(0); written < size; written += int64(len(buf)) {
			n, err := w.Write(buf)
			served.Add(int64(n))
			if err != nil {
				return
			}
		}
	}))
	defer server.Close()

	reader := &httpRangeReaderAt{
		ctx:      t.Context(),
		client:   server.Client(),
		source:   server.URL,
		size:     size,
		maxBytes: httpRangeReadBytes,
		maxReads: httpRangeReadCount,
	}
	p := make([]byte, 4096)
	if n, err := reader.ReadAt(p, 60<<20); n != 0 || !errors.Is(err, ErrNoIndex) {
		t.Fatalf("ReadAt near EOF = %d, %v; want 0, ErrNoIndex", n, err)
	}
	if got := served.Load(); got >= 8<<20 {
		t.Fatalf("origin served %d bytes for one 4 KiB read; the discarded prefix was downloaded", got)
	}
}
