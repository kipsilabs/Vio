package handlers

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// buildSyntheticManifest returns a realistic full VOD playlist: every entry
// repeats the same long segment URI template, which is why a real master.m3u8
// is 140 KB+ and compresses heavily.
func buildSyntheticManifest(entries int) []byte {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:3\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-MAP:URI=\"init.mp4?token=eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.really.long.query.string\"\n")
	for range entries {
		b.WriteString("#EXTINF:2.000000,\n")
		b.WriteString("seg_00000.ts?token=eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.really.long.query.string\n")
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return []byte(b.String())
}

// TestTranscodeManifestResponseGzipsIdenticalBody pins the manifest
// compression: a request that accepts gzip gets the byte-identical playlist
// compressed, marked Content-Encoding: gzip and Vary: Accept-Encoding, while
// the no-store/Pragma cache semantics and Content-Type are unchanged. The
// compressed form must be dramatically smaller because the playlist repeats
// the same segment-URI template on every line.
func TestTranscodeManifestResponseGzipsIdenticalBody(t *testing.T) {
	manifest := buildSyntheticManifest(4000)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/playback/transcode/sess/master.m3u8", nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	writeTranscodeManifestResponse(rec, req, manifest)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/vnd.apple.mpegurl" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store, max-age=0" {
		t.Fatalf("Cache-Control = %q, want the no-store semantics preserved", got)
	}
	if got := rec.Header().Get("Pragma"); got != "no-cache" {
		t.Fatalf("Pragma = %q, want no-cache", got)
	}
	if got := rec.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Fatalf("Vary = %q, want Accept-Encoding", got)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}

	gz, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	decoded, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("gzip read: %v", err)
	}
	if !bytes.Equal(decoded, manifest) {
		t.Fatalf("decoded body differs from the source manifest (%d vs %d bytes)", len(decoded), len(manifest))
	}
	if len(rec.Body.Bytes())*5 > len(manifest) {
		t.Fatalf("compressed size %d is not >5x smaller than %d", len(rec.Body.Bytes()), len(manifest))
	}
}

// TestTranscodeManifestResponseIdentityWithoutGzip proves the encoding is
// negotiated, not unconditional: a client that does not accept gzip (or
// explicitly refuses it) gets the identical raw playlist and no
// Content-Encoding.
func TestTranscodeManifestResponseIdentityWithoutGzip(t *testing.T) {
	manifest := []byte("#EXTM3U\n#EXT-X-ENDLIST\n")
	for _, accept := range []string{"", "identity", "gzip;q=0", "br, deflate"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/playback/transcode/sess/master.m3u8", nil)
		if accept != "" {
			req.Header.Set("Accept-Encoding", accept)
		}
		writeTranscodeManifestResponse(rec, req, manifest)

		if rec.Code != http.StatusOK {
			t.Fatalf("accept=%q status = %d, want 200", accept, rec.Code)
		}
		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Fatalf("accept=%q Content-Encoding = %q, want identity", accept, got)
		}
		if !bytes.Equal(rec.Body.Bytes(), manifest) {
			t.Fatalf("accept=%q body changed; got %q", accept, rec.Body.String())
		}
		// Vary must be advertised on the identity variant too, so a cache never
		// keys the gzip and identity bodies together.
		if got := rec.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
			t.Fatalf("accept=%q Vary = %q, want Accept-Encoding", accept, got)
		}
	}
}

// TestAcceptsGzipEncoding covers the q-value negotiation directly, including
// the q=0 refusal that must win even when gzip is otherwise listed.
func TestAcceptsGzipEncoding(t *testing.T) {
	tests := []struct {
		header string
		want   bool
	}{
		{"", false},
		{"gzip", true},
		{"gzip, deflate", true},
		{"deflate, gzip;q=0.8", true},
		{"gzip;q=0", false},
		{"GZIP", true},
		{"GZIP;Q=0", false},
		{" gzip ; q=0 ", false},
		{"gzip;q=0.0", false},
		{"gzip;q=0.000 , deflate", false},
		{"br", false},
		{"identity", false},
	}
	for _, tt := range tests {
		if got := acceptsGzipEncoding(tt.header); got != tt.want {
			t.Errorf("acceptsGzipEncoding(%q) = %v, want %v", tt.header, got, tt.want)
		}
	}
}
