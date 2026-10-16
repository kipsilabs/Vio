package handlers

import (
	"math"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// validatedVideoDescriptorV3 returns a running plan's source descriptor with
// every video field the carry reconstructs, including the aggregated
// copy-safety verdict the planner reads for route selection.
func validatedVideoDescriptorV3() playback.SourceDescriptorV3 {
	return playback.SourceDescriptorV3{
		MediaFileID:        720,
		Container:          "mkv",
		VideoCodec:         "hevc",
		VideoProfile:       "main10",
		VideoLevel:         153,
		BitDepth:           10,
		ColorRange:         "tv",
		Width:              3840,
		Height:             2160,
		FrameRate:          24000.0 / 1001.0,
		BitrateKbps:        15_000,
		DynamicRange:       playback.DynamicRangeDolbyVisionV3,
		HDR10Plus:          true,
		DVProfile:          8,
		DVLevel:            6,
		DVBLCompatID:       1,
		DVBaseLayerProven:  true,
		DVEnhancementLayer: playback.EnhancementMELV3,
		VideoCopyUnsafe:    true,
	}
}

func virtualCarryRowV3() *models.MediaFile {
	return &models.MediaFile{ID: 720, FilePath: "virtual://movie/tt-carry"}
}

// A row the carry must not touch is returned unchanged and by the same pointer:
// a nil row, a local file, a descriptor for another row, a row that already
// carries complete evidence, and a descriptor with a missing planner field.
func TestCarryValidatedVirtualVideoEvidenceV3LeavesUntouchedRowsAlone(t *testing.T) {
	descriptor := validatedVideoDescriptorV3()
	probed := func() *models.MediaFile {
		file := virtualCarryRowV3()
		file.Container = "mkv"
		file.CodecVideo = "hevc"
		file.VideoTracks = []models.VideoTrack{{
			Codec: "hevc", Profile: "main10", Level: 153, BitDepth: 10,
			Width: 3840, Height: 2160, FrameRate: "24000/1001", Bitrate: 15_000,
			ColorRange: "tv", HDR10Plus: true, VideoRangeType: playback.DynamicRangeDolbyVisionV3,
		}}
		return file
	}

	cases := []struct {
		name   string
		file   *models.MediaFile
		source playback.SourceDescriptorV3
	}{
		{name: "nil row", file: nil, source: descriptor},
		{name: "non-virtual row", file: &models.MediaFile{ID: 720, FilePath: "/library/movie.mkv"}, source: descriptor},
		{name: "row-id mismatch", file: virtualCarryRowV3(), source: func() playback.SourceDescriptorV3 {
			s := descriptor
			s.MediaFileID = 999
			return s
		}()},
		{name: "complete row", file: probed(), source: descriptor},
		{name: "zero bitrate descriptor", file: virtualCarryRowV3(), source: func() playback.SourceDescriptorV3 {
			s := descriptor
			s.BitrateKbps = 0
			return s
		}()},
		{name: "empty codec descriptor", file: virtualCarryRowV3(), source: func() playback.SourceDescriptorV3 {
			s := descriptor
			s.VideoCodec = ""
			return s
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := carryValidatedVirtualVideoEvidenceV3(tc.file, tc.source); got != tc.file {
				t.Fatalf("carry modified a row it must leave untouched for %q", tc.name)
			}
		})
	}
}

// The empty-track reconstruction fills the descriptor's full planner field set
// and carries the copy-safety verdict and proven DV base layer.
func TestCarryValidatedVirtualVideoEvidenceV3FillsEmptyTrackIncludingCopySafety(t *testing.T) {
	file := virtualCarryRowV3()
	file.Container = "virtual"
	descriptor := validatedVideoDescriptorV3()

	got := carryValidatedVirtualVideoEvidenceV3(file, descriptor)
	if got == file {
		t.Fatal("carry left an unprobed virtual row untouched")
	}
	if len(got.VideoTracks) != 1 {
		t.Fatalf("video tracks = %d, want the reconstructed track", len(got.VideoTracks))
	}
	track := got.VideoTracks[0]
	if track.Codec != "hevc" || track.Profile != "main10" || track.Level != 153 ||
		track.BitDepth != 10 || track.Width != 3840 || track.Height != 2160 ||
		track.Bitrate != 15_000 || track.ColorRange != "tv" || !track.HDR10Plus ||
		track.VideoRangeType != "DOVI" {
		t.Fatalf("reconstructed track = %+v, want the descriptor's planner fields", track)
	}
	if math.Abs(virtualFrameRateV3(track.FrameRate)-descriptor.FrameRate) > 1e-6 {
		t.Fatalf("frame rate = %q, want %v", track.FrameRate, descriptor.FrameRate)
	}
	if track.DVProfile != 8 || track.DVLevel != 6 || track.DVBLCompatID != 1 ||
		!track.DVConfigPresent || !track.DVBLCompatIDPresent || !track.DVBLPresent ||
		track.DVEnhancementLayer != string(playback.EnhancementMELV3) {
		t.Fatalf("DV evidence = %+v, want the descriptor's DV classification and proven base layer", track)
	}
	if !track.VideoCopyUnsafe {
		t.Fatal("copy-safety verdict was not carried; the plan's disqualified route would read copy-safe")
	}
	if got.Container != "mkv" {
		t.Fatalf("container = %q, want the descriptor's mkv", got.Container)
	}
	if !completeVirtualVideoEvidenceV3(got) {
		t.Fatal("carried row is not planner-complete")
	}
}

// The existing-track path must fill the exact same field set as the empty-track
// path. Before the fix it copied only codec/bit-depth/dimensions/frame-rate/
// bitrate, so a partial row dropped the profile, level, color range, HDR flags,
// DV classification, and the copy-safety verdict — a stream the running plan
// knew was not safely copyable read copy-safe.
func TestCarryValidatedVirtualVideoEvidenceV3FillsExistingPartialTrack(t *testing.T) {
	file := virtualCarryRowV3()
	file.CodecVideo = "hevc"
	file.VideoTracks = []models.VideoTrack{{Codec: "hevc"}}
	descriptor := validatedVideoDescriptorV3()

	got := carryValidatedVirtualVideoEvidenceV3(file, descriptor)
	if got == file {
		t.Fatal("carry left a partial-track virtual row untouched")
	}
	track := got.VideoTracks[0]
	if track.Profile != "main10" || track.Level != 153 || track.BitDepth != 10 ||
		track.Width != 3840 || track.Height != 2160 || track.Bitrate != 15_000 ||
		track.ColorRange != "tv" || !track.HDR10Plus ||
		track.VideoRangeType != "DOVI" {
		t.Fatalf("partial track = %+v, want the same planner fields as the empty-track fill", track)
	}
	if !track.VideoCopyUnsafe {
		t.Fatal("copy-safety verdict was not filled on the partial track")
	}
	if track.DVProfile != 8 || !track.DVConfigPresent || !track.DVBLCompatIDPresent || !track.DVBLPresent {
		t.Fatalf("DV evidence = %+v, want the descriptor's DV classification", track)
	}
}

// A descriptor that proves a DV base layer but names no DV profile must not
// produce DV provenance flags on the carried track: a profileless descriptor is
// not a DV classification the route resolver can trust, so the carry keeps the
// three presence booleans false and fails closed.
func TestCarryValidatedVirtualVideoEvidenceV3DoesNotInventDVWithoutProfile(t *testing.T) {
	descriptor := validatedVideoDescriptorV3()
	descriptor.DVProfile = 0
	descriptor.DVLevel = 0
	descriptor.DVBLCompatID = 0

	file := virtualCarryRowV3()
	got := carryValidatedVirtualVideoEvidenceV3(file, descriptor)
	if got == file {
		t.Fatal("carry left the unprobed row untouched")
	}
	track := got.VideoTracks[0]
	if track.DVProfile != 0 || track.DVLevel != 0 || track.DVBLCompatID != 0 ||
		track.DVConfigPresent || track.DVBLCompatIDPresent || track.DVBLPresent {
		t.Fatalf("carry invented DV classification from a profileless descriptor: %+v", track)
	}
	if track.VideoRangeType != "DOVI" {
		t.Fatalf("video range type = %q, want the descriptor's dynamic range", track.VideoRangeType)
	}
}

// A row that already declares a different codec must not receive the// descriptor's evidence: mixing a declared h264 with an hevc descriptor is
// franken-evidence the planner would route against. Normalized agreement
// ("h264"/"avc") must still carry.
func TestCarryValidatedVirtualVideoEvidenceV3RefusesCodecDisagreement(t *testing.T) {
	descriptor := validatedVideoDescriptorV3()

	disagreeing := []struct {
		name string
		file *models.MediaFile
	}{
		{name: "file codec disagrees", file: func() *models.MediaFile {
			file := virtualCarryRowV3()
			file.CodecVideo = "h264"
			return file
		}()},
		{name: "track codec disagrees", file: func() *models.MediaFile {
			file := virtualCarryRowV3()
			file.VideoTracks = []models.VideoTrack{{Codec: "h264"}}
			return file
		}()},
	}
	for _, tc := range disagreeing {
		t.Run(tc.name, func(t *testing.T) {
			got := carryValidatedVirtualVideoEvidenceV3(tc.file, descriptor)
			if got != tc.file {
				t.Fatal("carry applied descriptor evidence to a row declaring a different codec")
			}
			if len(got.VideoTracks) > 0 && got.VideoTracks[0].Width != 0 {
				t.Fatalf("franken-evidence: descriptor geometry reached a disagreeing row: %+v", got.VideoTracks[0])
			}
		})
	}

	aligned := virtualCarryRowV3()
	aligned.CodecVideo = "h264"
	aligned.VideoTracks = []models.VideoTrack{{Codec: "avc"}}
	h264Descriptor := descriptor
	h264Descriptor.VideoCodec = "h264"
	if got := carryValidatedVirtualVideoEvidenceV3(aligned, h264Descriptor); got == aligned {
		t.Fatal("normalized codec agreement (h264/avc) was refused")
	}
}

// A "30000/1001" rational frame rate survives both directions: the
// reconstruction from the descriptor's float is planner-complete, and a stored
// rational form is preserved verbatim while still round-tripping through the
// descriptor parser.
func TestCarryValidatedVirtualVideoEvidenceV3FrameRateRoundTrip(t *testing.T) {
	descriptor := validatedVideoDescriptorV3()
	descriptor.FrameRate = 30000.0 / 1001.0

	file := virtualCarryRowV3()
	got := carryValidatedVirtualVideoEvidenceV3(file, descriptor)
	if got == file || len(got.VideoTracks) != 1 {
		t.Fatal("carry did not reconstruct the video track")
	}
	if !playback.VirtualRouteVideoMetadataCompleteV3(got) {
		t.Fatal("30000/1001 reconstruction is not planner-complete")
	}
	if recovered := playback.SourceDescriptorFromFileV3(got, -1); math.Abs(recovered.FrameRate-descriptor.FrameRate) > 1e-6 {
		t.Fatalf("empty-track frame rate round-trip = %v, want %v", recovered.FrameRate, descriptor.FrameRate)
	}

	stored := virtualCarryRowV3()
	stored.VideoTracks = []models.VideoTrack{{Codec: "hevc", FrameRate: "30000/1001"}}
	storedGot := carryValidatedVirtualVideoEvidenceV3(stored, descriptor)
	if storedGot == stored {
		t.Fatal("carry left a partial row untouched")
	}
	if storedGot.VideoTracks[0].FrameRate != "30000/1001" {
		t.Fatalf("stored frame rate = %q, want the rational form preserved", storedGot.VideoTracks[0].FrameRate)
	}
	if recovered := playback.SourceDescriptorFromFileV3(storedGot, -1); math.Abs(recovered.FrameRate-descriptor.FrameRate) > 1e-6 {
		t.Fatalf("stored frame rate round-trip = %v, want %v", recovered.FrameRate, descriptor.FrameRate)
	}
}

// The descriptor's kbps bitrate lands in the kbps-native row fields and
// round-trips back through the descriptor unchanged.
func TestCarryValidatedVirtualVideoEvidenceV3BitrateUnitRoundTrip(t *testing.T) {
	descriptor := validatedVideoDescriptorV3()
	descriptor.BitrateKbps = 5_673
	file := virtualCarryRowV3()

	got := carryValidatedVirtualVideoEvidenceV3(file, descriptor)
	if got == file {
		t.Fatal("carry left the unprobed row untouched")
	}
	if got.Bitrate != 5_673 || got.VideoTracks[0].Bitrate != 5_673 {
		t.Fatalf("carried bitrate = file %d track %d, want 5673 kbps on both", got.Bitrate, got.VideoTracks[0].Bitrate)
	}
	if recovered := playback.SourceDescriptorFromFileV3(got, -1); recovered.BitrateKbps != 5_673 {
		t.Fatalf("bitrate round-trip = %d, want 5673", recovered.BitrateKbps)
	}
}

// The carry must not touch the row it is handed. A shallow struct copy shares
// VideoTracks' backing array, so topping up the first element in place would
// rewrite the caller's inventory — and race any other reader of it. The carry
// clones the slice before the first write.
func TestCarryValidatedVirtualVideoEvidenceV3DoesNotMutateInputRow(t *testing.T) {
	file := virtualCarryRowV3()
	file.CodecVideo = "hevc"
	file.VideoTracks = []models.VideoTrack{{Codec: "hevc"}}
	before := file.VideoTracks[0]
	inputSlice := file.VideoTracks

	descriptor := validatedVideoDescriptorV3()
	got := carryValidatedVirtualVideoEvidenceV3(file, descriptor)
	if got == file {
		t.Fatal("carry left a partial-track virtual row untouched")
	}
	if file.VideoTracks[0] != before {
		t.Fatalf("carry mutated the input row's video track: %+v -> %+v", before, file.VideoTracks[0])
	}
	if len(inputSlice) != 1 || inputSlice[0] != before {
		t.Fatalf("carry mutated the input row's video-track backing array: %+v", inputSlice)
	}
	if &file.VideoTracks[0] == &got.VideoTracks[0] {
		t.Fatal("carry reused the input row's video-track backing array for the returned row")
	}
	if got.VideoTracks[0].BitDepth != 10 {
		t.Fatalf("returned track bit depth = %d, want the descriptor's 10", got.VideoTracks[0].BitDepth)
	}
}

// assertVideoDescriptorRoundTripV3 compares every planner-facing source field
// the carry reconstructs, so a carry that drops or reclassifies one is visible.
func assertVideoDescriptorRoundTripV3(t *testing.T, in, out playback.SourceDescriptorV3) {
	t.Helper()
	if out.VideoCodec != in.VideoCodec || out.VideoProfile != in.VideoProfile ||
		out.VideoLevel != in.VideoLevel || out.BitDepth != in.BitDepth ||
		out.Width != in.Width || out.Height != in.Height ||
		math.Abs(out.FrameRate-in.FrameRate) > 1e-6 ||
		out.BitrateKbps != in.BitrateKbps || out.ColorRange != in.ColorRange ||
		out.DynamicRange != in.DynamicRange || out.HDR10Plus != in.HDR10Plus ||
		out.DVProfile != in.DVProfile || out.DVLevel != in.DVLevel ||
		out.DVBLCompatID != in.DVBLCompatID || out.DVBaseLayerProven != in.DVBaseLayerProven ||
		out.DVEnhancementLayer != in.DVEnhancementLayer || out.VideoCopyUnsafe != in.VideoCopyUnsafe {
		t.Fatalf("descriptor did not round-trip:\n in = %+v\nout = %+v", in, out)
	}
}

// The carried row must classify to the same dynamic range the running plan
// validated. Assigning the normalized label straight to VideoRangeType did not
// round-trip: "dolby_vision" has no "dovi" token so profileless DV fell to SDR,
// and "hdr_unknown" carries "hdr" so it was promoted to HDR10. This drives the
// descriptor in through the real builder and asserts descriptor-in equals
// descriptor-out for every grade.
func TestCarryValidatedVirtualVideoEvidenceV3DynamicRangeRoundTrip(t *testing.T) {
	baseRow := func() *models.MediaFile {
		return &models.MediaFile{
			ID: 720, FilePath: "virtual://movie/tt-range", Container: "mkv",
			CodecVideo: "hevc", Bitrate: 15_000,
			VideoTracks: []models.VideoTrack{{
				Codec: "hevc", Profile: "main10", Level: 153, BitDepth: 10,
				Width: 3840, Height: 2160, FrameRate: "24000/1001", Bitrate: 15_000,
				ColorRange: "tv",
			}},
		}
	}
	rangeRow := func(rangeType string) *models.MediaFile {
		file := baseRow()
		file.VideoTracks[0].VideoRangeType = rangeType
		return file
	}
	unknownHDRRow := func() *models.MediaFile {
		file := baseRow()
		file.HDR = true
		return file
	}
	hdr10PlusRow := func() *models.MediaFile {
		file := baseRow()
		file.VideoTracks[0].HDR10Plus = true
		return file
	}
	// A profileless DV source carries only the "DOVI" range label: no profile
	// and no base-layer provenance, the shape whose dynamic range was lost.
	profilelessDVRow := func() *models.MediaFile {
		return rangeRow("DOVI")
	}
	// A profiled DV source carries the config record, and the compatibility id
	// may be an explicit zero.
	dvRow := func(compatID int) *models.MediaFile {
		file := rangeRow("DOVIWithHDR10")
		track := &file.VideoTracks[0]
		track.DVProfile = 8
		track.DVLevel = 6
		track.DVBLCompatID = compatID
		track.DVConfigPresent = true
		track.DVBLCompatIDPresent = true
		track.DVBLPresent = true
		return file
	}

	cases := []struct {
		name string
		row  *models.MediaFile
	}{
		{name: "sdr", row: rangeRow("SDR")},
		{name: "hdr10", row: rangeRow("HDR10")},
		{name: "hdr10_plus", row: hdr10PlusRow()},
		{name: "hlg", row: rangeRow("HLG")},
		{name: "unknown_hdr", row: unknownHDRRow()},
		{name: "dv_profileless", row: profilelessDVRow()},
		{name: "dv_compat_one", row: dvRow(1)},
		{name: "dv_compat_zero", row: dvRow(0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			descriptorIn := playback.SourceDescriptorFromFileV3(tc.row, -1)
			stub := virtualCarryRowV3()
			got := carryValidatedVirtualVideoEvidenceV3(stub, descriptorIn)
			if got == stub {
				t.Fatalf("carry left the unprobed row untouched for %s", tc.name)
			}
			descriptorOut := playback.SourceDescriptorFromFileV3(got, -1)
			assertVideoDescriptorRoundTripV3(t, descriptorIn, descriptorOut)
		})
	}
}

// A row that already knows its Dolby compatibility id is zero keeps it: zero is
// a meaningful value tonemap deliberately refuses to resolve, not a missing
// field, and matching codecs alone does not establish that the row's profile
// and compatibility agree with the descriptor's.
func TestCarryValidatedVirtualVideoEvidenceV3PreservesKnownZeroCompatID(t *testing.T) {
	file := virtualCarryRowV3()
	file.VideoTracks = []models.VideoTrack{{
		Codec: "hevc", DVBLCompatID: 0, DVBLCompatIDPresent: true,
	}}
	descriptor := validatedVideoDescriptorV3()
	descriptor.DVProfile = 7
	descriptor.DVBLCompatID = 6
	descriptor.DVBaseLayerProven = true

	got := carryValidatedVirtualVideoEvidenceV3(file, descriptor)
	if got == file {
		t.Fatal("carry left the partial-track virtual row untouched")
	}
	if got.VideoTracks[0].DVBLCompatID != 0 {
		t.Fatalf("known zero compatibility id = %d, want it preserved", got.VideoTracks[0].DVBLCompatID)
	}
	if got.VideoTracks[0].DVProfile != 7 {
		t.Fatalf("profile = %d, want the missing profile filled from the descriptor", got.VideoTracks[0].DVProfile)
	}
}
