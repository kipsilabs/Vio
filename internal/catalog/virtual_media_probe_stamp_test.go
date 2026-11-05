package catalog

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestVirtualMediaRegistrationDoesNotStampProbe requires a migrated database
// via SILO_TEST_DATABASE_URL. Registration stores declared display hints
// (provider-declared languages) but it is not an ffprobe: it must leave
// probe_updated_at NULL on a new row and preserve whatever a real probe already
// recorded when a rotated result hash adopts the row. Stamping now() made the
// row look probed while its track inventory was still only a language hint, so
// the stream was never repaired with real ffprobe data.
func TestVirtualMediaRegistrationDoesNotStampProbe(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(999,'TestVirtualProbe','movies',true)`); err != nil {
		t.Fatalf("seed virtual media folder: %v", err)
	}
	reg := newReleasedVirtualMediaRegistrar(pool)

	movie := VirtualMedia{
		LibraryID:      "999",
		MediaType:      "movie",
		Title:          "Probe Stamp Film",
		IMDbID:         "tt70000001",
		TMDBID:         "70000001",
		Source:         "provider-probe-stamp",
		RuntimeMinutes: 100,
		VirtualURI:     "virtual://movie/tt70000001?result=hash-one",
		Container:      "mkv",
		Resolution:     "1080p",
		CodecVideo:     "h264",
		AudioLanguages: []string{"eng", "fra"},
	}

	// A fresh registration inserts the row with no probe timestamp.
	res, err := reg.UpsertVirtualMedia(ctx, 11, movie)
	if err != nil {
		t.Fatalf("register virtual media: %v", err)
	}
	probeSource, probeUpdatedAt, audioTracks := loadVirtualProbeState(t, ctx, pool, res.MediaID, movie.VirtualURI)
	if probeSource != "virtual" {
		t.Fatalf("probe_source = %q, want virtual", probeSource)
	}
	if probeUpdatedAt != nil {
		t.Fatalf("fresh registration stamped probe_updated_at = %v, want NULL", probeUpdatedAt)
	}
	// Declared languages remain display hints stored in audio_tracks; only the
	// probe stamping changes.
	var declared []struct {
		Language string `json:"language"`
	}
	if err := json.Unmarshal([]byte(audioTracks), &declared); err != nil {
		t.Fatalf("decode declared audio tracks %q: %v", audioTracks, err)
	}
	if len(declared) != 2 || declared[0].Language != "eng" || declared[1].Language != "fra" {
		t.Fatalf("declared audio languages = %s, want eng+fra", audioTracks)
	}

	// Rotating to a new result hash adopts the unprobed row; it must stay NULL.
	movie.VirtualURI = "virtual://movie/tt70000001?result=hash-two"
	if _, err := reg.UpsertVirtualMedia(ctx, 11, movie); err != nil {
		t.Fatalf("rotate unprobed virtual media: %v", err)
	}
	if _, probeUpdatedAt, _ = loadVirtualProbeState(t, ctx, pool, res.MediaID, movie.VirtualURI); probeUpdatedAt != nil {
		t.Fatalf("adoption of an unprobed row stamped probe_updated_at = %v, want NULL", probeUpdatedAt)
	}

	// A row that a real probe already stamped must lose that stamp on adoption.
	// Registration overwrites the probed inventory with provider-declared hints,
	// so the row's evidence is no longer verified and the next play must
	// re-probe instead of trusting a stale stamp.
	probedAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx,
		`UPDATE media_files SET probe_source='virtual', probe_updated_at=$1 WHERE content_id=$2 AND file_path=$3`,
		probedAt, res.MediaID, movie.VirtualURI); err != nil {
		t.Fatalf("simulate real probe: %v", err)
	}
	movie.VirtualURI = "virtual://movie/tt70000001?result=hash-three"
	if _, err := reg.UpsertVirtualMedia(ctx, 11, movie); err != nil {
		t.Fatalf("rotate probed virtual media: %v", err)
	}
	if _, probeUpdatedAt, _ = loadVirtualProbeState(t, ctx, pool, res.MediaID, movie.VirtualURI); probeUpdatedAt != nil {
		t.Fatalf("adoption of a probed row kept probe_updated_at = %v, want NULL", probeUpdatedAt)
	}
}

// TestVirtualMediaVariantRegistrationDoesNotStampProbe covers the variant insert
// path, which shares the same registration semantics as the base file path.
func TestVirtualMediaVariantRegistrationDoesNotStampProbe(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(999,'TestVirtualProbe','movies',true)`); err != nil {
		t.Fatalf("seed virtual media folder: %v", err)
	}
	reg := newReleasedVirtualMediaRegistrar(pool)

	uri := "virtual://movie/tt70000002?profile=1080p&result=hash-one"
	in := VirtualMedia{
		LibraryID:      "999",
		MediaType:      "movie",
		Title:          "Probe Stamp Variant Film",
		IMDbID:         "tt70000002",
		TMDBID:         "70000002",
		Source:         "provider-probe-stamp",
		RuntimeMinutes: 100,
		Variants: []VirtualMediaVariant{{
			VirtualURI:     uri,
			Label:          "1080p",
			Resolution:     "1080p",
			CodecVideo:     "h264",
			AudioLanguages: []string{"eng"},
		}},
	}

	res, err := reg.UpsertVirtualMedia(ctx, 11, in)
	if err != nil {
		t.Fatalf("register virtual media variant: %v", err)
	}
	if _, probeUpdatedAt, _ := loadVirtualProbeState(t, ctx, pool, res.MediaID, uri); probeUpdatedAt != nil {
		t.Fatalf("variant registration stamped probe_updated_at = %v, want NULL", probeUpdatedAt)
	}

	in.Variants[0].VirtualURI = "virtual://movie/tt70000002?profile=1080p&result=hash-two"
	if _, err := reg.UpsertVirtualMedia(ctx, 11, in); err != nil {
		t.Fatalf("rotate virtual media variant: %v", err)
	}
	if _, probeUpdatedAt, _ := loadVirtualProbeState(t, ctx, pool, res.MediaID, in.Variants[0].VirtualURI); probeUpdatedAt != nil {
		t.Fatalf("variant adoption stamped probe_updated_at = %v, want NULL", probeUpdatedAt)
	}
}

func loadVirtualProbeState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, contentID, filePath string) (string, *time.Time, string) {
	t.Helper()
	var (
		probeSource    string
		probeUpdatedAt *time.Time
		audioTracks    string
	)
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(probe_source,''), probe_updated_at, audio_tracks::text FROM media_files WHERE content_id=$1 AND file_path=$2`,
		contentID, filePath).Scan(&probeSource, &probeUpdatedAt, &audioTracks); err != nil {
		t.Fatalf("load probe state for %s: %v", filePath, err)
	}
	return probeSource, probeUpdatedAt, audioTracks
}

// seedProbedVirtualRow makes an existing virtual row look like ffprobe already
// stamped it: a probe timestamp, a real container, rich probe tracks, and (when
// edition is non-empty) a foreign edition_raw that makes adoptVirtualFileRow
// miss so the INSERT ON CONFLICT branch runs.
func seedProbedVirtualRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, contentID, filePath, edition string, probedAt time.Time) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		UPDATE media_files SET
			probe_source='virtual', probe_updated_at=$1, last_delivered_at=$1,
			resolution='2160p', codec_video='hevc', codec_audio='eac3', container='mkv',
			hdr=true, bitrate=5000, edition_raw=$2,
			video_tracks='[{"codec":"hevc","width":3840,"height":2160,"frame_rate":"23.976"}]'::jsonb,
			audio_tracks='[{"codec":"eac3","channels":6,"language":"jpn"}]'::jsonb,
			subtitle_tracks='[{"codec":"srt","language":"jpn"}]'::jsonb
		WHERE content_id=$3 AND file_path=$4`, probedAt, edition, contentID, filePath); err != nil {
		t.Fatalf("seed probed virtual row %s: %v", filePath, err)
	}
}

// assertVirtualRegistrationClearedProbe proves registration reset the probe
// stamp on a previously probed row and wrote the provider-declared hints.
func assertVirtualRegistrationClearedProbe(t *testing.T, ctx context.Context, pool *pgxpool.Pool, contentID, filePath, wantResolution string) {
	t.Helper()
	var (
		probeUpdatedAt *time.Time
		probeSource    *string
		resolution     *string
		codecVideo     *string
		audioTracks    string
	)
	if err := pool.QueryRow(ctx, `
		SELECT probe_updated_at, probe_source, resolution, codec_video, audio_tracks::text
		FROM media_files WHERE content_id=$1 AND file_path=$2`, contentID, filePath).
		Scan(&probeUpdatedAt, &probeSource, &resolution, &codecVideo, &audioTracks); err != nil {
		t.Fatalf("load probe state for %s: %v", filePath, err)
	}
	if probeUpdatedAt != nil {
		t.Fatalf("probe_updated_at = %v, want NULL: registration overwrote the probed inventory", probeUpdatedAt)
	}
	if probeSource != nil {
		t.Fatalf("probe_source = %v, want NULL", *probeSource)
	}
	if resolution == nil || *resolution != wantResolution {
		t.Fatalf("resolution = %v, want registration hint %q", resolution, wantResolution)
	}
	if codecVideo == nil || *codecVideo != "h264" {
		t.Fatalf("codec_video = %v, want registration hint h264", codecVideo)
	}
	if !strings.Contains(audioTracks, "eng") {
		t.Fatalf("audio_tracks = %s, want registration language hint eng", audioTracks)
	}
}

// Base registration rotation adopts an existing probed row and must clear its
// probe stamp because the provider hints replace the probed inventory.
func TestVirtualMediaRegistrationClearsProbeOnAdoptedRow(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(999,'TestVirtualProbe','movies',true)`); err != nil {
		t.Fatalf("seed virtual media folder: %v", err)
	}
	reg := newReleasedVirtualMediaRegistrar(pool)

	movie := VirtualMedia{
		LibraryID:      "999",
		MediaType:      "movie",
		Title:          "Adopt Clear Film",
		IMDbID:         "tt70000201",
		TMDBID:         "70000201",
		Source:         "provider-probe-clear",
		RuntimeMinutes: 100,
		VirtualURI:     "virtual://movie/tt70000201?result=hash-one",
		Container:      "mkv",
		Resolution:     "1080p",
		CodecVideo:     "h264",
		CodecAudio:     "aac",
		AudioLanguages: []string{"eng"},
	}
	res, err := reg.UpsertVirtualMedia(ctx, 11, movie)
	if err != nil {
		t.Fatalf("register virtual media: %v", err)
	}
	probedAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	seedProbedVirtualRow(t, ctx, pool, res.MediaID, movie.VirtualURI, "", probedAt)

	movie.VirtualURI = "virtual://movie/tt70000201?result=hash-two"
	if _, err := reg.UpsertVirtualMedia(ctx, 11, movie); err != nil {
		t.Fatalf("rotate probed virtual media: %v", err)
	}
	assertVirtualRegistrationClearedProbe(t, ctx, pool, res.MediaID, movie.VirtualURI, "1080p")
}

// Base registration's INSERT ON CONFLICT branch must clear the stamp as well.
// A foreign edition_raw makes the adopt lookup miss without changing the unique
// (file_path, owner, folder) tuple.
func TestVirtualMediaRegistrationClearsProbeOnConflictRow(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(999,'TestVirtualProbe','movies',true)`); err != nil {
		t.Fatalf("seed virtual media folder: %v", err)
	}
	reg := newReleasedVirtualMediaRegistrar(pool)

	movie := VirtualMedia{
		LibraryID:      "999",
		MediaType:      "movie",
		Title:          "Conflict Clear Film",
		IMDbID:         "tt70000211",
		TMDBID:         "70000211",
		Source:         "provider-probe-clear",
		RuntimeMinutes: 100,
		VirtualURI:     "virtual://movie/tt70000211?result=hash-one",
		Container:      "mkv",
		Resolution:     "1080p",
		CodecVideo:     "h264",
		CodecAudio:     "aac",
		AudioLanguages: []string{"eng"},
	}
	res, err := reg.UpsertVirtualMedia(ctx, 11, movie)
	if err != nil {
		t.Fatalf("register virtual media: %v", err)
	}
	probedAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	seedProbedVirtualRow(t, ctx, pool, res.MediaID, movie.VirtualURI, "legacy-edition", probedAt)

	if _, err := reg.UpsertVirtualMedia(ctx, 11, movie); err != nil {
		t.Fatalf("re-register conflicting virtual media: %v", err)
	}
	assertVirtualRegistrationClearedProbe(t, ctx, pool, res.MediaID, movie.VirtualURI, "1080p")
}

// Variant registration rotation adopts a probed variant row and must clear its
// probe stamp.
func TestVirtualMediaVariantRegistrationClearsProbeOnAdoptedRow(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(999,'TestVirtualProbe','movies',true)`); err != nil {
		t.Fatalf("seed virtual media folder: %v", err)
	}
	reg := newReleasedVirtualMediaRegistrar(pool)

	in := VirtualMedia{
		LibraryID:      "999",
		MediaType:      "movie",
		Title:          "Variant Adopt Clear Film",
		IMDbID:         "tt70000221",
		TMDBID:         "70000221",
		Source:         "provider-probe-clear",
		RuntimeMinutes: 100,
		Variants: []VirtualMediaVariant{{
			VirtualURI:     "virtual://movie/tt70000221?profile=1080p&result=hash-one",
			Label:          "1080p",
			Resolution:     "1080p",
			CodecVideo:     "h264",
			CodecAudio:     "aac",
			AudioLanguages: []string{"eng"},
		}},
	}
	res, err := reg.UpsertVirtualMedia(ctx, 11, in)
	if err != nil {
		t.Fatalf("register virtual media variant: %v", err)
	}
	probedAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	seedProbedVirtualRow(t, ctx, pool, res.MediaID, in.Variants[0].VirtualURI, "1080p", probedAt)

	in.Variants[0].VirtualURI = "virtual://movie/tt70000221?profile=1080p&result=hash-two"
	if _, err := reg.UpsertVirtualMedia(ctx, 11, in); err != nil {
		t.Fatalf("rotate virtual media variant: %v", err)
	}
	assertVirtualRegistrationClearedProbe(t, ctx, pool, res.MediaID, in.Variants[0].VirtualURI, "1080p")
}

// Variant registration's INSERT ON CONFLICT branch must clear the stamp too.
func TestVirtualMediaVariantRegistrationClearsProbeOnConflictRow(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(999,'TestVirtualProbe','movies',true)`); err != nil {
		t.Fatalf("seed virtual media folder: %v", err)
	}
	reg := newReleasedVirtualMediaRegistrar(pool)

	in := VirtualMedia{
		LibraryID:      "999",
		MediaType:      "movie",
		Title:          "Variant Conflict Clear Film",
		IMDbID:         "tt70000231",
		TMDBID:         "70000231",
		Source:         "provider-probe-clear",
		RuntimeMinutes: 100,
		Variants: []VirtualMediaVariant{{
			VirtualURI:     "virtual://movie/tt70000231?profile=1080p&result=hash-one",
			Label:          "1080p",
			Resolution:     "1080p",
			CodecVideo:     "h264",
			CodecAudio:     "aac",
			AudioLanguages: []string{"eng"},
		}},
	}
	res, err := reg.UpsertVirtualMedia(ctx, 11, in)
	if err != nil {
		t.Fatalf("register virtual media variant: %v", err)
	}
	probedAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	seedProbedVirtualRow(t, ctx, pool, res.MediaID, in.Variants[0].VirtualURI, "legacy-edition", probedAt)

	if _, err := reg.UpsertVirtualMedia(ctx, 11, in); err != nil {
		t.Fatalf("re-register conflicting virtual media variant: %v", err)
	}
	assertVirtualRegistrationClearedProbe(t, ctx, pool, res.MediaID, in.Variants[0].VirtualURI, "1080p")
}
