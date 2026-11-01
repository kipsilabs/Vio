package playback

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	maxSubtitleFontAttachments = 32
	// maxSubtitleFontBytes is the hard ceiling for one returned bundle's raw
	// attachment payload. Real releases often pack complete font families, and
	// an attachment total above the old 32 MiB ceiling made every bundle fail
	// hard ("attached font data exceeds ..."), so clients stayed on fallback
	// fonts forever. 48 MiB clears that common pack size while staying an
	// explicit hard bound: the bundle is base64-inflated into one client JSON
	// response, so ~64 MiB encoded is the largest a pending-bundle refresh can
	// plausibly deliver. A complete pack at or below this bound is returned in
	// full (the historical behavior); a larger pack is reduced to the fonts its
	// ASS scripts actually reference, and only fails closed when that subset
	// cannot be produced.
	maxSubtitleFontBytes     = 48 << 20 // 48 MiB
	subtitleFontTTFMIME      = "font/ttf"
	subtitleFontFallbackMIME = "application/octet-stream"

	// maxSubtitleFontScanBytes bounds how many raw attachment bytes a subset
	// extraction may stream through to identify embedded families. The bytes of
	// unreferenced fonts are drained, never retained, so this is a bandwidth and
	// time bound rather than a memory bound. It only applies when the complete
	// pack is already past maxSubtitleFontBytes.
	maxSubtitleFontScanBytes = 512 << 20 // 512 MiB

	// maxSubtitleFontScriptTracks bounds how many embedded ASS/SSA tracks are
	// inspected for referenced font families in one subset extraction. More
	// tracks than this fall back to the historical whole-pack behavior rather
	// than risk omitting a font referenced by an uninspected track.
	maxSubtitleFontScriptTracks = 8
	// maxSubtitleFontScriptBytes bounds the decoded script text parsed for one
	// bundle across all inspected tracks.
	maxSubtitleFontScriptBytes = 8 << 20 // 8 MiB
	// maxSubtitleFontScriptProbeBytes bounds one ffprobe hex-dump response. The
	// dump is roughly four times the decoded script, and scripts are normally a
	// few hundred kilobytes.
	maxSubtitleFontScriptProbeBytes = 32 << 20 // 32 MiB
	// maxSubtitleFontFamilyLen and maxSubtitleFontFamilies bound the parsed
	// reference set so a pathological script (or font name table) cannot grow it
	// without limit.
	maxSubtitleFontFamilyLen   = 256
	maxSubtitleFontFamilies    = 128
	maxSubtitleFontNameTable   = 64 << 10 // one sfnt name table
	maxSubtitleFontNameRecords = 1024
	maxSubtitleFontTables      = 128
	maxSubtitleFontCollections = 8

	// ffprobeBinary is the fallback probe executable when no ffmpeg path (and
	// therefore no sibling ffprobe) is configured.
	ffprobeBinary = "ffprobe"
	// sfntVersionTrue is the TrueType version tag ffmpeg reports for legacy
	// Apple fonts.
	sfntVersionTrue = "true"

	// subtitleFontProbeTimeout bounds the metadata probe that opens the source
	// and reports attachment stream sizes before any payload is streamed.
	subtitleFontProbeTimeout = 30 * time.Second
	// subtitleFontDumpBaseBudget is the floor for the attachment dump. It
	// covers process startup, the source open, and normal container overhead,
	// and keeps the historical 30s bound for small/unknown payloads.
	subtitleFontDumpBaseBudget = 30 * time.Second
	// subtitleFontDumpBytesPerSecond is the conservative relay throughput the
	// dump budget is sized against. Production relays run slower than LAN, and
	// the old flat 30s could not stream a multi-MiB font pack at this rate.
	subtitleFontDumpBytesPerSecond = 256 << 10 // 256 KiB/s
	// subtitleFontDumpMaxBudget bounds the scaled dump. It stays under the
	// detached single-flight's subtitleFontBundleExtractTimeout (5m) so a
	// pathological pack still yields a real failure instead of outliving the
	// flight that owns the relay registration.
	subtitleFontDumpMaxBudget = 4 * time.Minute
)

// subtitleFontDumpBudget scales the attachment-dump deadline with the payload
// the probe reported. A small or unknown pack keeps the 30s floor; a multi-MiB
// pack gets enough time to stream at a conservative relay rate, capped so the
// extraction always settles inside the detached single-flight budget.
func subtitleFontDumpBudget(streams []attachmentProbeStream) time.Duration {
	var total int64
	for _, stream := range streams {
		if stream.ExtraDataSize > 0 {
			total += stream.ExtraDataSize
		}
	}
	budget := subtitleFontDumpBaseBudget
	if total >= subtitleFontDumpBytesPerSecond {
		budget += time.Duration(total/subtitleFontDumpBytesPerSecond) * time.Second
	}
	if budget > subtitleFontDumpMaxBudget {
		return subtitleFontDumpMaxBudget
	}
	return budget
}

// SubtitleFontAttachment is a font attached to a media container for ASS/SSA
// subtitle rendering.
type SubtitleFontAttachment struct {
	// StreamIndex is the original container stream index used by attachment routes.
	StreamIndex int
	Name        string
	Data        []byte
}

// SubtitleFontBundleItem is the JSON-safe representation sent to web players.
type SubtitleFontBundleItem struct {
	Name string `json:"name"`
	Data string `json:"data"`
}

type attachmentProbeOutput struct {
	Streams []attachmentProbeStream `json:"streams"`
}

type attachmentProbeStream struct {
	Index         int               `json:"index"`
	ExtraDataSize int64             `json:"extradata_size"`
	CodecName     string            `json:"codec_name"`
	CodecType     string            `json:"codec_type"`
	Tags          map[string]string `json:"tags"`
}

// SubtitleFontMetadata identifies an attached font without extracting its bytes.
type SubtitleFontMetadata struct {
	Index    int
	Codec    string
	FileName string
	MimeType string
}

var subtitleFontProbeSlots = make(chan struct{}, 2)

// ListAttachedSubtitleFonts bounds concurrent metadata probes and their duration.
// Busy or unavailable probes return an error; callers can omit optional discovery.
func ListAttachedSubtitleFonts(ctx context.Context, inputPath, ffmpegPath string) ([]SubtitleFontMetadata, error) {
	select {
	case subtitleFontProbeSlots <- struct{}{}:
		defer func() { <-subtitleFontProbeSlots }()
	default:
		return nil, errors.New("subtitle font probes busy")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	streams, err := probeFontAttachmentStreams(ctx, inputPath, ffprobePathFromFFmpeg(ffmpegPath))
	if err != nil {
		return nil, err
	}
	if len(streams) > maxSubtitleFontAttachments {
		streams = streams[:maxSubtitleFontAttachments]
	}
	fonts := make([]SubtitleFontMetadata, 0, len(streams))
	for i, stream := range streams {
		name := safeAttachmentDisplayName(stream, fmt.Sprintf("attachment-%d%s", i, fontAttachmentExt(stream)))
		fonts = append(fonts, SubtitleFontMetadata{Index: stream.Index, Codec: stream.CodecName, FileName: name, MimeType: SubtitleFontMIMEType(name)})
	}
	return fonts, nil
}

// ExtractAttachedSubtitleFonts extracts font attachments from a media file.
// Matroska ASS releases commonly include the exact fonts needed by the script;
// loading them into JASSUB is the closest browser equivalent to libass on a
// native player.
func ExtractAttachedSubtitleFonts(ctx context.Context, inputPath string, ffmpegPath string) ([]SubtitleFontAttachment, error) {
	return extractAttachedSubtitleFonts(ctx, inputPath, ffmpegPath, nil, maxSubtitleFontBytes)
}

// ExtractAttachedSubtitleFont extracts only the requested container stream.
// The same font validation, attachment count and byte limits apply as to bundles.
func ExtractAttachedSubtitleFont(ctx context.Context, inputPath, ffmpegPath string, index int) (*SubtitleFontAttachment, error) {
	fonts, err := extractAttachedSubtitleFonts(ctx, inputPath, ffmpegPath, &index, maxSubtitleFontBytes)
	if err != nil || len(fonts) == 0 {
		return nil, err
	}
	return &fonts[0], nil
}

func extractAttachedSubtitleFonts(ctx context.Context, inputPath, ffmpegPath string, index *int, maxBytes int64) ([]SubtitleFontAttachment, error) {
	if strings.TrimSpace(inputPath) == "" {
		return nil, fmt.Errorf("subtitle fonts: input path is required")
	}

	probePath := ffprobePathFromFFmpeg(ffmpegPath)
	probeCtx, cancelProbe := context.WithTimeout(ctx, subtitleFontProbeTimeout)
	streams, err := probeFontAttachmentStreams(probeCtx, inputPath, probePath)
	cancelProbe()
	if err != nil {
		return nil, err
	}
	if len(streams) == 0 {
		return nil, nil
	}
	if len(streams) > maxSubtitleFontAttachments {
		streams = streams[:maxSubtitleFontAttachments]
	}
	selectedName := ""
	if index != nil {
		selected := slices.IndexFunc(streams, func(stream attachmentProbeStream) bool { return stream.Index == *index })
		if selected < 0 {
			return nil, nil
		}
		selectedName = safeAttachmentDisplayName(streams[selected], fmt.Sprintf("attachment-%d%s", selected, fontAttachmentExt(streams[selected])))
		streams = streams[selected : selected+1]
	}

	// The probe above reports each attachment's payload size, so the streaming
	// phase can be budgeted for the bytes it actually has to move. Keep the
	// caller's deadline (context.WithTimeout inherits an earlier one).
	dumpCtx, cancelDump := context.WithTimeout(ctx, subtitleFontDumpBudget(streams))
	defer cancelDump()
	ctx = dumpCtx

	bin := ffmpegPath
	if strings.TrimSpace(bin) == "" {
		bin = "ffmpeg"
	}

	// Only a whole-bundle request that is already past the ceiling needs the
	// scripts. Smaller packs keep the historical bytes and cost: no extra
	// probe, no family parsing, all attachments returned.
	if index == nil && attachmentPayloadExceeds(streams, maxBytes) {
		fonts, done, subsetErr := extractReferencedFontSubset(ctx, inputPath, bin, probePath, streams, maxBytes)
		if subsetErr != nil {
			return nil, subsetErr
		}
		if done {
			return fonts, nil
		}
		// The script was unavailable or referenced no attached family. Fall
		// through to the bounded whole-pack dump so the caller gets the same
		// explicit size failure (and pending/backoff) it got before subsetting.
	}

	fonts, err := dumpFontAttachments(ctx, inputPath, bin, streams, maxBytes)
	if selectedName != "" && len(fonts) > 0 {
		fonts[0].Name = selectedName
	}
	return fonts, err
}

// attachmentPayloadExceeds reports whether the complete probed payload is past
// the retained-bundle ceiling, which is the only case where script-driven
// subsetting is attempted.
// extractReferencedFontSubset attempts the script-driven reduction. done is
// false when the caller should fall back to the bounded whole-pack dump
// (unavailable script, no references, or no matching attachment), and an error
// reports a real extraction failure that must propagate.
func extractReferencedFontSubset(ctx context.Context, inputPath, ffmpegPath, ffprobePath string, streams []attachmentProbeStream, maxBytes int64) ([]SubtitleFontAttachment, bool, error) {
	referenced, ok := assReferencedFontFamilies(ctx, inputPath, ffprobePath)
	if !ok || len(referenced) == 0 {
		return nil, false, nil
	}
	fonts, err := dumpFontAttachmentsSubset(ctx, inputPath, ffmpegPath, streams, maxBytes, referenced)
	if err == nil {
		return fonts, true, nil
	}
	if errors.Is(err, errSubtitleFontSubsetEmpty) {
		return nil, false, nil
	}
	return nil, false, err
}

func attachmentPayloadExceeds(streams []attachmentProbeStream, maxBytes int64) bool {
	var total int64
	for _, stream := range streams {
		if stream.ExtraDataSize <= 0 {
			return false
		}
		if total > maxBytes-stream.ExtraDataSize {
			return true
		}
		total += stream.ExtraDataSize
	}
	return false
}

// EncodeSubtitleFontBundle converts raw font attachments to base64 JSON items.
func EncodeSubtitleFontBundle(fonts []SubtitleFontAttachment) []SubtitleFontBundleItem {
	items := make([]SubtitleFontBundleItem, 0, len(fonts))
	for _, font := range fonts {
		items = append(items, SubtitleFontBundleItem{
			Name: font.Name,
			Data: base64.StdEncoding.EncodeToString(font.Data),
		})
	}
	return items
}

// dumpFontAttachments concatenates the selected attachments on stdout in one
// FFmpeg process. Probed extradata sizes frame each font; a bounded pipe reader
// rejects oversized or changed output without ever spilling font data to disk.
func dumpFontAttachments(ctx context.Context, inputPath string, ffmpegPath string, streams []attachmentProbeStream, maxBytes int64) ([]SubtitleFontAttachment, error) {
	var expected int64
	for _, stream := range streams {
		if stream.ExtraDataSize <= 0 {
			return nil, errors.New("subtitle fonts: invalid attachment data size")
		}
		if stream.ExtraDataSize > maxBytes-expected {
			return nil, fmt.Errorf("subtitle fonts: attached font data exceeds %d bytes", maxBytes)
		}
		expected += stream.ExtraDataSize
	}
	if expected == 0 {
		return nil, nil
	}
	args := []string{ffmpegHideBannerArg, "-nostats", ffmpegLogLevelArg, ffmpegErrorLogLevel}
	for _, stream := range streams {
		args = append(args, fmt.Sprintf("-dump_attachment:%d", stream.Index), "pipe:1")

	}
	args = append(args, "-i", inputPath, "-map", "0:t?", "-c", "copy", "-f", "null", "-")
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("subtitle fonts: start attachment extract: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(stdout, expected+1))
	if readErr != nil || int64(len(data)) > expected {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, errors.New("subtitle fonts: attachment data exceeds probed size or could not be read")
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("subtitle fonts: extract attachments: %w", err)
	}
	if int64(len(data)) != expected {
		return nil, errors.New("subtitle fonts: attachment data differs from probed size")
	}
	fonts := make([]SubtitleFontAttachment, 0, len(streams))
	offset := 0
	for i, stream := range streams {
		end := offset + int(stream.ExtraDataSize)
		fonts = append(fonts, SubtitleFontAttachment{
			StreamIndex: stream.Index,
			Name:        safeAttachmentDisplayName(stream, fmt.Sprintf("attachment-%d%s", i, fontAttachmentExt(stream))),
			Data:        data[offset:end:end],
		})
		offset = end
	}
	return fonts, nil
}

// errSubtitleFontSubsetEmpty marks a successful script read whose referenced
// families matched no attachment. The caller falls back to the bounded
// whole-pack dump, which preserves the historical failure when the pack is
// genuinely too large.
var errSubtitleFontSubsetEmpty = errors.New("subtitle fonts: no attachment matches the ASS script")

// assScriptProbeOutput is the subset of ffprobe's stream and packet report used
// to reconstruct ASS scripts: the header (styles) lives in the stream
// extradata and the event lines (which carry {\fn} overrides) in packets.
type assScriptProbeOutput struct {
	Streams []struct {
		Index     int    `json:"index"`
		CodecName string `json:"codec_name"`
		Extradata string `json:"extradata"`
	} `json:"streams"`
	Packets []struct {
		StreamIndex int    `json:"stream_index"`
		Data        string `json:"data"`
	} `json:"packets"`
}

// assScriptTrack identifies one embedded ASS/SSA subtitle stream: its 0-based
// ordinal among all subtitle streams (the `0:s:N` specifier) and its container
// stream index (which tags its packets).
type assScriptTrack struct {
	Ordinal   int
	Index     int
	Extradata string
}

// assReferencedFontFamilies reconstructs every embedded ASS/SSA script in the
// container and returns the font families they reference. It reports false when
// a script cannot be read completely or within bounds, when there are no ASS
// tracks, or when there are more ASS tracks than the bounded scan allows; the
// caller must then keep the historical whole-pack behavior rather than risk a
// bundle that silently omits a font.
func assReferencedFontFamilies(ctx context.Context, inputPath, ffprobePath string) ([]string, bool) {
	bin := ffprobePath
	if strings.TrimSpace(bin) == "" {
		bin = ffprobeBinary
	}
	tracks, ok := probeASSScriptTracks(ctx, bin, inputPath)
	if !ok || len(tracks) == 0 || len(tracks) > maxSubtitleFontScriptTracks {
		return nil, false
	}
	var script []byte
	for _, track := range tracks {
		text, ok := probeASSScript(ctx, bin, inputPath, track)
		if !ok {
			return nil, false
		}
		if len(text) == 0 {
			continue
		}
		if len(script)+len(text)+1 > maxSubtitleFontScriptBytes {
			return nil, false
		}
		script = append(script, text...)
		script = append(script, '\n')
		if ctx.Err() != nil {
			return nil, false
		}
	}
	if len(script) == 0 {
		return nil, false
	}
	return parseASSFontFamilies(script), true
}

// probeASSScriptTracks reads the subtitle stream table, including each stream's
// header extradata, and returns the ASS/SSA tracks in subtitle-ordinal order.
// Metadata-only: no packets are demuxed here.
func probeASSScriptTracks(ctx context.Context, ffprobePath, inputPath string) ([]assScriptTrack, bool) {
	out, ok := captureFFprobe(ctx, ffprobePath,
		"-v", "error",
		"-select_streams", "s",
		"-show_streams", "-show_data",
		"-of", "json",
		inputPath,
	)
	if !ok {
		return nil, false
	}
	var probed assScriptProbeOutput
	if err := json.Unmarshal(out, &probed); err != nil {
		return nil, false
	}
	tracks := make([]assScriptTrack, 0, len(probed.Streams))
	for ordinal, stream := range probed.Streams {
		if !IsASS(stream.CodecName) {
			continue
		}
		tracks = append(tracks, assScriptTrack{Ordinal: ordinal, Index: stream.Index, Extradata: stream.Extradata})
		if len(tracks) > maxSubtitleFontScriptTracks {
			return nil, false
		}
	}
	return tracks, true
}

// probeASSScript reads one embedded ASS/SSA track's events and returns the
// header plus every event packet as text. The ffprobe output is a hex dump of
// each blob, so the read is bounded by the encoded size and the decoded script
// separately.
func probeASSScript(ctx context.Context, ffprobePath, inputPath string, track assScriptTrack) ([]byte, bool) {
	out, ok := captureFFprobe(ctx, ffprobePath,
		"-v", "error",
		"-select_streams", fmt.Sprintf("s:%d", track.Ordinal),
		"-show_packets", "-show_data",
		"-of", "json",
		inputPath,
	)
	if !ok {
		return nil, false
	}
	var probed assScriptProbeOutput
	if err := json.Unmarshal(out, &probed); err != nil {
		return nil, false
	}
	var script []byte
	if track.Extradata != "" {
		decoded, ok := decodeFFprobeHexDump(track.Extradata)
		if !ok {
			return nil, false
		}
		script = append(script, decoded...)
		script = append(script, '\n')
	}
	for _, packet := range probed.Packets {
		if packet.StreamIndex != track.Index || packet.Data == "" {
			continue
		}
		decoded, ok := decodeFFprobeHexDump(packet.Data)
		if !ok {
			return nil, false
		}
		if len(script)+len(decoded)+1 > maxSubtitleFontScriptBytes {
			return nil, false
		}
		script = append(script, decoded...)
		script = append(script, '\n')
	}
	if len(script) > maxSubtitleFontScriptBytes {
		return nil, false
	}
	return script, true
}

// captureFFprobe runs one bounded metadata/packet probe and returns its stdout.
// The child's stderr is discarded; a probe that fails, times out, or exceeds the
// encoded bound is reported as unusable so callers fall back instead of guessing.
func captureFFprobe(ctx context.Context, ffprobePath string, args ...string) ([]byte, bool) {
	cmd := exec.CommandContext(ctx, ffprobePath, args...) //nolint:gosec // ffprobePath is operator configuration, never request input.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false
	}
	if err := cmd.Start(); err != nil {
		return nil, false
	}
	out, readErr := io.ReadAll(io.LimitReader(stdout, maxSubtitleFontScriptProbeBytes+1))
	if readErr != nil || len(out) > maxSubtitleFontScriptProbeBytes {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, false
	}
	if err := cmd.Wait(); err != nil {
		return nil, false
	}
	return out, true
}

// decodeFFprobeHexDump decodes one of ffprobe's `00000000: 5b53 6372 ...  [Sc`
// data blocks. Hex groups are exactly four digits; the ASCII column begins
// after a double space, which yields an empty token and ends the block.
func decodeFFprobeHexDump(dump string) ([]byte, bool) {
	if len(dump) > maxSubtitleFontScriptProbeBytes {
		return nil, false
	}
	decoded := make([]byte, 0, len(dump)/4)
	for _, line := range strings.Split(dump, "\n") {
		line = strings.TrimRight(line, "\r")
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		rest := line[colon+1:]
		started := false
		for _, token := range strings.Split(rest, " ") {
			if token == "" {
				// A leading space precedes the first group; the double space
				// before the ASCII column ends the line once groups started.
				if started {
					break
				}
				continue
			}
			if len(token) != 4 && len(token) != 2 {
				break
			}
			value, err := strconv.ParseUint(token, 16, 16)
			if err != nil {
				break
			}
			started = true
			if len(token) == 4 {
				decoded = append(decoded, byte(value>>8))
			}
			decoded = append(decoded, byte(value)) //nolint:gosec // A 16-bit hex group fits two bytes.
			if len(decoded) > maxSubtitleFontScriptBytes {
				return nil, false
			}
		}
	}
	return decoded, true
}

// parseASSFontFamilies returns the distinct font families an ASS/SSA script
// references: the Fontname column of every Style record (honoring the section's
// Format line) plus every {\fn...} override. Parsing is bounded and allocation
// light; it never depends on a script being well formed beyond the fields it
// needs.
func parseASSFontFamilies(script []byte) []string {
	if len(script) > maxSubtitleFontScriptBytes {
		script = script[:maxSubtitleFontScriptBytes]
	}
	var families []string
	seen := make(map[string]struct{})
	add := func(name string) {
		name = strings.TrimSpace(name)
		name = strings.TrimPrefix(name, "@")
		name = strings.TrimSpace(name)
		if name == "" || len(name) > maxSubtitleFontFamilyLen {
			return
		}
		key := strings.ToLower(name)
		if _, ok := seen[key]; ok {
			return
		}
		if len(families) >= maxSubtitleFontFamilies {
			return
		}
		seen[key] = struct{}{}
		families = append(families, name)
	}

	inStyles := false
	fontnameIndex := -1
	for _, raw := range strings.Split(string(script), "\n") {
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			lower := strings.ToLower(line)
			inStyles = strings.HasPrefix(lower, "[v4") && strings.Contains(lower, "styles")
			fontnameIndex = -1
			if inStyles {
				// ASS v4+ and legacy v4 both default to Name, Fontname, ...
				fontnameIndex = 1
			}
			continue
		}
		if inStyles {
			switch {
			case hasASSTag(line, "Format:"):
				fields := strings.Split(line[len("Format:"):], ",")
				for i, field := range fields {
					if strings.EqualFold(strings.TrimSpace(field), "Fontname") {
						fontnameIndex = i
						break
					}
				}
			case hasASSTag(line, "Style:"):
				fields := strings.Split(line[len("Style:"):], ",")
				if fontnameIndex >= 0 && fontnameIndex < len(fields) {
					add(fields[fontnameIndex])
				}
			}
		}
		for _, name := range assInlineFontOverrides(line) {
			add(name)
		}
	}
	return families
}

// hasASSTag reports a record keyword at the start of an already-trimmed line,
// allowing the optional leading whitespace ffmpeg's ass muxer emits.
func hasASSTag(line, tag string) bool {
	return len(line) >= len(tag) && strings.EqualFold(line[:len(tag)], tag)
}

// assInlineFontOverrides extracts every \fn family from one script line. The
// family runs until the next override or the closing brace.
func assInlineFontOverrides(line string) []string {
	var families []string
	lower := strings.ToLower(line)
	for offset := 0; offset < len(lower); {
		index := strings.Index(lower[offset:], `\fn`)
		if index < 0 {
			break
		}
		start := offset + index + len(`\fn`)
		end := start
		for end < len(line) && line[end] != '\\' && line[end] != '}' {
			end++
		}
		if name := strings.TrimSpace(line[start:end]); name != "" {
			families = append(families, name)
		}
		offset = end
	}
	return families
}

// dumpFontAttachmentsSubset streams every probed attachment through one ffmpeg
// invocation and retains only those whose embedded font family is referenced by
// the ASS script. Unreferenced payload is drained rather than buffered, so the
// pack may be much larger than the retained-bundle ceiling. Referenced families
// are matched against the sfnt name table (family, typographic family, full
// name and PostScript name); when a name table cannot be read from the bounded
// prefix, the attachment filename is used as a conservative fallback.
func dumpFontAttachmentsSubset(ctx context.Context, inputPath string, ffmpegPath string, streams []attachmentProbeStream, maxBytes int64, referenced []string) ([]SubtitleFontAttachment, error) {
	var scanTotal int64
	for _, stream := range streams {
		if stream.ExtraDataSize <= 0 {
			return nil, errors.New("subtitle fonts: invalid attachment data size")
		}
		if scanTotal > maxSubtitleFontScanBytes-stream.ExtraDataSize {
			return nil, fmt.Errorf("subtitle fonts: attached font data exceeds scan bound %d bytes", maxSubtitleFontScanBytes)
		}
		scanTotal += stream.ExtraDataSize
	}
	if scanTotal == 0 {
		return nil, nil
	}
	referencedNames := make([]string, 0, len(referenced))
	for _, name := range referenced {
		if normalized := normalizeFontName(name); normalized != "" {
			referencedNames = append(referencedNames, normalized)
		}
	}
	if len(referencedNames) == 0 {
		return nil, errSubtitleFontSubsetEmpty
	}

	args := []string{ffmpegHideBannerArg, "-nostats", ffmpegLogLevelArg, ffmpegErrorLogLevel}
	for _, stream := range streams {
		args = append(args, fmt.Sprintf("-dump_attachment:%d", stream.Index), "pipe:1")
	}
	args = append(args, "-i", inputPath, "-map", "0:t?", "-c", "copy", "-f", "null", "-")
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("subtitle fonts: start attachment extract: %w", err)
	}

	const prefixBytes = 1 << 20
	reader := bufio.NewReaderSize(stdout, 64<<10)
	fonts := make([]SubtitleFontAttachment, 0, len(streams))
	var keptBytes int64
	kill := func(err error) ([]SubtitleFontAttachment, error) {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	for i, stream := range streams {
		size := stream.ExtraDataSize
		prefixLen := size
		if prefixLen > prefixBytes {
			prefixLen = prefixBytes
		}
		prefix := make([]byte, int(prefixLen))
		if _, err := io.ReadFull(reader, prefix); err != nil {
			return kill(errors.New("subtitle fonts: attachment data could not be read"))
		}
		matches := fontAttachmentReferenced(stream, prefix, referencedNames)
		if !matches {
			if _, err := io.CopyN(io.Discard, reader, size-prefixLen); err != nil {
				return kill(errors.New("subtitle fonts: attachment data could not be read"))
			}
			continue
		}
		if size > maxBytes-keptBytes {
			return kill(fmt.Errorf("subtitle fonts: attached font data exceeds %d bytes", maxBytes))
		}
		data := make([]byte, int(size))
		copy(data, prefix)
		if _, err := io.ReadFull(reader, data[len(prefix):]); err != nil {
			return kill(errors.New("subtitle fonts: attachment data could not be read"))
		}
		fonts = append(fonts, SubtitleFontAttachment{
			StreamIndex: stream.Index,
			Name:        safeAttachmentDisplayName(stream, fmt.Sprintf("attachment-%d%s", i, fontAttachmentExt(stream))),
			Data:        data,
		})
		keptBytes += size
	}
	// The probed sizes frame every attachment; anything after the last one
	// means the stream changed under the probe and the framing is untrustworthy.
	if _, err := reader.ReadByte(); err == nil {
		return kill(errors.New("subtitle fonts: attachment data exceeds probed size"))
	} else if !errors.Is(err, io.EOF) {
		return kill(errors.New("subtitle fonts: attachment data could not be read"))
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("subtitle fonts: extract attachments: %w", err)
	}
	if len(fonts) == 0 {
		return nil, errSubtitleFontSubsetEmpty
	}
	return fonts, nil
}

// fontAttachmentReferenced reports whether an attachment's embedded or filename
// identity matches one of the normalized referenced families.
func fontAttachmentReferenced(stream attachmentProbeStream, prefix []byte, referenced []string) bool {
	for _, name := range embeddedFontNames(prefix) {
		if fontNameMatches(name, referenced) {
			return true
		}
	}
	// A WOFF/WOFF2 attachment (compressed sfnt) or a name table beyond the
	// bounded prefix yields no embedded names. The muxer's filename is the only
	// remaining evidence; use it rather than silently dropping a referenced font.
	filename := strings.TrimSuffix(stream.Tags["filename"], filepath.Ext(stream.Tags["filename"]))
	return filename != "" && fontNameMatches(filename, referenced)
}

// fontNameMatches compares a candidate font name with the normalized referenced
// families. Token-subset matching lets "Futura Medium" resolve to the "Futura"
// family and "OpenSans" to "Open Sans"; a false positive only adds an extra
// font, while a false negative would drop one the script needs.
func fontNameMatches(candidate string, referenced []string) bool {
	candidateName := normalizeFontName(candidate)
	if candidateName == "" {
		return false
	}
	candidateTokens := strings.Fields(candidateName)
	candidateCompact := strings.ReplaceAll(candidateName, " ", "")
	for _, reference := range referenced {
		if reference == candidateName {
			return true
		}
		referenceTokens := strings.Fields(reference)
		if tokenSubset(referenceTokens, candidateTokens) || tokenSubset(candidateTokens, referenceTokens) {
			return true
		}
		referenceCompact := strings.ReplaceAll(reference, " ", "")
		if len(referenceCompact) >= 3 && len(candidateCompact) >= 3 &&
			(strings.Contains(candidateCompact, referenceCompact) || strings.Contains(referenceCompact, candidateCompact)) {
			return true
		}
	}
	return false
}

func tokenSubset(small, large []string) bool {
	if len(small) == 0 || len(small) > len(large) {
		return false
	}
	for _, token := range small {
		if !slices.Contains(large, token) {
			return false
		}
	}
	return true
}

// normalizeFontName lowercases a family or filename and reduces separators to
// single spaces so style names and sfnt names compare consistently.
func normalizeFontName(name string) string {
	name = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(name), "@"))
	var b strings.Builder
	lastSpace := true
	for _, r := range strings.ToLower(name) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '+':
			b.WriteRune(r)
			lastSpace = false
		case unicode.IsSpace(r) || r == '-' || r == '_' || r == '.':
			if !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// embeddedFontNames reads the family-identifying strings from an sfnt font
// (TTF/OTF, including the first fonts of a TrueType/OpenType collection). Only
// the supplied prefix is available when an attachment was streamed; a name
// table that does not fit returns no names and lets the caller fall back to the
// filename.
func embeddedFontNames(data []byte) []string {
	if len(data) < 12 {
		return nil
	}
	if string(data[:4]) == "ttcf" {
		fonts := int(be32(data[8:12]))
		if fonts <= 0 || fonts > maxSubtitleFontCollections {
			return nil
		}
		var names []string
		for i := 0; i < fonts; i++ {
			offset := int(be32At(data, 12+4*i))
			if offset <= 0 || offset >= len(data) {
				continue
			}
			// Collection table offsets are relative to the start of the
			// collection, not to the subfont's offset table.
			names = append(names, sfntFontNamesAt(data, offset)...)
		}
		return names
	}
	return sfntFontNamesAt(data, 0)
}

// sfntFontNamesAt reads one sfnt font whose offset table starts at base. Table
// offsets are absolute for both standalone fonts (base 0) and collection
// members.
func sfntFontNamesAt(data []byte, base int) []string {
	if base < 0 || base+12 > len(data) {
		return nil
	}
	header := data[base:]
	switch string(header[:4]) {
	case "\x00\x01\x00\x00", "OTTO", sfntVersionTrue, "typ1":
	default:
		return nil
	}
	numTables := int(be16(header[4:6]))
	if numTables <= 0 || numTables > maxSubtitleFontTables {
		return nil
	}
	if len(header) < 12+numTables*16 {
		return nil
	}
	nameOffset, nameLength := -1, 0
	for i := 0; i < numTables; i++ {
		record := header[12+i*16:]
		if string(record[:4]) != "name" {
			continue
		}
		// Table offsets are absolute for both standalone fonts and collection
		// members (from the start of the file/collection).
		nameOffset = int(be32(record[8:12]))
		nameLength = int(be32(record[12:16]))
		break
	}
	if nameOffset < 0 || nameLength <= 0 || nameLength > maxSubtitleFontNameTable {
		return nil
	}
	if nameOffset > len(data) || nameLength > len(data)-nameOffset {
		return nil
	}
	table := data[nameOffset : nameOffset+nameLength]
	if len(table) < 6 {
		return nil
	}
	count := int(be16(table[2:4]))
	stringOffset := int(be16(table[4:6]))
	if count <= 0 || count > maxSubtitleFontNameRecords {
		return nil
	}
	if len(table) < 6+count*12 {
		return nil
	}
	// Name IDs 16 (typographic family) and 1 (family) are the best identity,
	// followed by 4 (full name) and 6 (PostScript name). Keep all of them; the
	// matcher accepts either side of a style suffix.
	priority := []int{16, 1, 4, 6}
	var names []string
	seen := make(map[string]struct{})
	for _, wanted := range priority {
		for i := 0; i < count; i++ {
			record := table[6+i*12:]
			platform := be16(record[0:2])
			nameID := int(be16(record[6:8]))
			if nameID != wanted {
				continue
			}
			length := int(be16(record[8:10]))
			offset := int(be16(record[10:12]))
			start := stringOffset + offset
			if length <= 0 || start < 0 || start > len(table) || length > len(table)-start {
				continue
			}
			name := decodeSFNTName(platform, table[start:start+length])
			name = strings.TrimSpace(name)
			if name == "" || len(name) > maxSubtitleFontFamilyLen {
				continue
			}
			key := strings.ToLower(name)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			names = append(names, name)
			if len(names) >= 8 {
				return names
			}
		}
	}
	return names
}

// decodeSFNTName decodes a name record. Windows/Unicode and Unicode-platform
// records are UTF-16BE; Macintosh records are single-byte and are treated as
// Latin-1, which is enough for family matching.
func decodeSFNTName(platform uint16, raw []byte) string {
	if platform == 3 || platform == 0 {
		if len(raw)%2 != 0 {
			raw = raw[:len(raw)-1]
		}
		runes := make([]rune, 0, len(raw)/2)
		for i := 0; i+1 < len(raw); i += 2 {
			runes = append(runes, rune(binary.BigEndian.Uint16(raw[i:i+2])))
		}
		return string(runes)
	}
	return string(raw)
}

func be16(data []byte) uint16 {
	if len(data) < 2 {
		return 0
	}
	return binary.BigEndian.Uint16(data[:2])
}

func be32(data []byte) uint32 {
	if len(data) < 4 {
		return 0
	}
	return binary.BigEndian.Uint32(data[:4])
}

func be32At(data []byte, offset int) uint32 {
	if offset < 0 || offset+4 > len(data) {
		return 0
	}
	return binary.BigEndian.Uint32(data[offset : offset+4])
}

func probeFontAttachmentStreams(ctx context.Context, inputPath string, ffprobePath string) ([]attachmentProbeStream, error) {
	bin := ffprobePath
	if strings.TrimSpace(bin) == "" {
		bin = ffprobeBinary
	}
	cmd := exec.CommandContext(ctx, bin,
		"-v", "error",
		"-select_streams", "t",
		"-show_entries", "stream=index,codec_name,codec_type,extradata_size:stream_tags=filename,mimetype",
		"-of", "json",
		inputPath,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	const maxProbeBytes = 1 << 20
	out, readErr := io.ReadAll(io.LimitReader(stdout, maxProbeBytes+1))
	if readErr != nil || len(out) > maxProbeBytes {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, errors.New("subtitle fonts: attachment metadata exceeds probe limit or could not be read")
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("subtitle fonts: probe attachments: %w", err)
	}

	var probed attachmentProbeOutput
	if err := json.Unmarshal(out, &probed); err != nil {
		return nil, fmt.Errorf("subtitle fonts: parse attachment probe: %w", err)
	}

	streams := make([]attachmentProbeStream, 0, len(probed.Streams))
	for _, stream := range probed.Streams {
		if isFontAttachment(stream) {
			streams = append(streams, stream)
		}
	}
	return streams, nil
}

func isFontAttachment(stream attachmentProbeStream) bool {
	if strings.ToLower(stream.CodecType) != "attachment" {
		return false
	}
	codec := strings.ToLower(stream.CodecName)
	switch codec {
	case "ttf", "otf", "ttc", "otc", "woff", "woff2":
		return true
	}
	filename := strings.ToLower(stream.Tags["filename"])
	switch filepath.Ext(filename) {
	case ".ttf", ".otf", ".ttc", ".otc", ".woff", ".woff2":
		return true
	}
	mimetype := strings.ToLower(stream.Tags["mimetype"])
	return strings.Contains(mimetype, "font") ||
		strings.Contains(mimetype, "truetype") ||
		strings.Contains(mimetype, "opentype") ||
		strings.Contains(mimetype, "woff")
}

func fontAttachmentExt(stream attachmentProbeStream) string {
	if ext := strings.ToLower(filepath.Ext(stream.Tags["filename"])); isSupportedFontExt(ext) {
		return ext
	}
	switch strings.ToLower(stream.CodecName) {
	case "ttf":
		return ".ttf"
	case "otf":
		return ".otf"
	case "ttc":
		return ".ttc"
	case "otc":
		return ".otc"
	case "woff":
		return ".woff"
	case "woff2":
		return ".woff2"
	default:
		return ".font"
	}
}

func isSupportedFontExt(ext string) bool {
	switch ext {
	case ".ttf", ".otf", ".ttc", ".otc", ".woff", ".woff2":
		return true
	default:
		return false
	}
}

func safeAttachmentDisplayName(stream attachmentProbeStream, fallback string) string {
	name := filepath.Base(stream.Tags["filename"])
	if !isSupportedFontExt(strings.ToLower(filepath.Ext(name))) {
		return fallback
	}
	return name
}

// SubtitleFontMIMEType never trusts a container's MIME metadata or the host's
// extension registrations when serving authenticated attachment bytes inline.
func SubtitleFontMIMEType(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".ttf":
		return subtitleFontTTFMIME
	case ".otf":
		return "font/otf"
	case ".ttc", ".otc":
		return "font/collection"
	case ".woff":
		return "font/woff"
	case ".woff2":
		return "font/woff2"
	default:
		return subtitleFontFallbackMIME
	}
}

func ffprobePathFromFFmpeg(ffmpegPath string) string {
	ffmpegPath = strings.TrimSpace(ffmpegPath)
	if ffmpegPath == "" {
		return ffprobeBinary
	}
	base := filepath.Base(ffmpegPath)
	if i := strings.LastIndex(base, "ffmpeg"); i >= 0 {
		return filepath.Join(filepath.Dir(ffmpegPath), base[:i]+ffprobeBinary+base[i+len("ffmpeg"):])
	}
	return ffprobeBinary
}
