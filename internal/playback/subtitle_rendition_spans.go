package playback

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const subtitleSpanRecordName = "subtitle_spans.json"

type subtitleSpanRecord struct {
	Track           int                     `json:"track"`
	SegmentDuration int                     `json:"segment_duration"`
	OnDemand        bool                    `json:"on_demand,omitempty"`
	Spans           []subtitleSpanRecordRow `json:"spans"`
}

type subtitleSpanRecordRow struct {
	File   string   `json:"file"`
	Start  int      `json:"start"`
	End    int      `json:"end"`
	Origin *float64 `json:"origin,omitempty"`
	// MuxShift is the span's muxShift, observed with Origin.
	MuxShift float64 `json:"mux_shift,omitempty"`
	// StartTime is the span's startTime. Absent in records that predate it,
	// which only encodes wrote, whose start time follows from Start.
	StartTime *float64 `json:"start_time,omitempty"`
	// Durations and Final are a closed span's durations and final, for a
	// copy remux on FFmpeg's own playlist.
	Durations []float64 `json:"durations,omitempty"`
	Final     bool      `json:"final,omitempty"`
	// InheritedPlaylist identifies the stream.m3u8 an earlier generation left
	// when the live generation started, so a rebuild can tell whether the
	// playlist on disk is the live generation's progress.
	InheritedPlaylist *subtitleSpanRecordPlaylist `json:"inherited_playlist,omitempty"`
}

type subtitleSpanRecordPlaylist struct {
	Size    int64 `json:"size"`
	ModTime int64 `json:"mod_time_ns"`
}

func recordedPlaylist(info os.FileInfo) *subtitleSpanRecordPlaylist {
	if info == nil {
		return nil
	}
	return &subtitleSpanRecordPlaylist{Size: info.Size(), ModTime: info.ModTime().UnixNano()}
}

func (p *subtitleSpanRecordPlaylist) matches(info os.FileInfo) bool {
	return p != nil && info.Size() == p.Size && info.ModTime().UnixNano() == p.ModTime
}

// liveGenerationPlaylist returns the current generation's own playlist. FFmpeg
// adds an entry only after the segment is complete, and a playlist an earlier
// generation left behind is not this process's progress.
func (s *TranscodeSession) liveGenerationPlaylist() ([]byte, bool) {
	s.mu.Lock()
	inherited := s.inheritedManifest
	s.mu.Unlock()
	data, _, err := readManifestFile(filepath.Join(s.outputDir, "stream.m3u8"), inherited)
	if err != nil {
		return nil, false
	}
	return data, true
}

// generationPlaylistTimeline reads the playlist a generation that started at
// segment start wrote: the durations of the segments it lists, in order, and
// whether it ended the media. A playlist that does not list start, start+1, ...
// from its first entry was written by another generation.
func generationPlaylistTimeline(data []byte, start int) (durations []float64, final, ok bool) {
	timeline, err := parseManifestTimeline(data)
	if err != nil || len(timeline.entries) == 0 {
		return nil, false, false
	}
	durations = make([]float64, 0, len(timeline.entries))
	for i, entry := range timeline.entries {
		if entry.number != start+i || !(entry.duration > 0) {
			return nil, false, false
		}
		durations = append(durations, entry.duration)
	}
	return durations, bytes.Contains(data, []byte("#EXT-X-ENDLIST")), true
}

func subtitleCueFileGeneration(name string) (uint64, bool) {
	if !strings.HasPrefix(name, "subtitles_") || !strings.HasSuffix(name, ".vtt") {
		return 0, false
	}
	n := strings.TrimSuffix(strings.TrimPrefix(name, "subtitles_"), ".vtt")
	v, err := strconv.ParseUint(n, 10, 64)
	return v, err == nil
}

func loadSubtitleSpans(opts TranscodeOpts, plan *copySegmentPlan) ([]subtitleCueSpan, uint64) {
	root, err := os.OpenRoot(opts.OutputDir)
	if err != nil {
		return nil, 0
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(subtitleSpanRecordName)
	if err != nil {
		return nil, 0
	}
	var record subtitleSpanRecord
	if json.Unmarshal(data, &record) != nil {
		return nil, 0
	}
	var spans []subtitleCueSpan
	var next uint64
	followsFFmpeg := followsFFmpegPlaylist(opts, plan)
	onDemand := subtitleRenditionsOnDemand(opts)
	for i, row := range record.Spans {
		generation, ok := subtitleCueFileGeneration(row.File)
		if !ok {
			continue
		}
		next = max(next, generation+1)
		if record.OnDemand != onDemand || !onDemand && record.Track != opts.SubtitleTrackIndex ||
			record.SegmentDuration != renditionSegmentDuration(opts) || row.Start < 0 {
			continue
		}
		if _, err := root.Stat(row.File); err != nil && !onDemand {
			continue
		}
		span := subtitleCueSpan{file: row.File, start: row.Start, end: row.End, durations: row.Durations, final: row.Final}
		if row.StartTime != nil {
			span.startTime = *row.StartTime
		} else {
			span.startTime = float64(row.Start * renditionSegmentDuration(opts))
		}
		if span.end < 0 {
			if i != len(record.Spans)-1 || !recordedLiveSpanProgress(root, row, followsFFmpeg, &span) {
				continue
			}
		}
		if followsFFmpeg && len(span.durations) != span.end-span.start+1 {
			// Its segments' times are unknown, so its cues can't be cut.
			continue
		}
		if span.end >= row.Start {
			if row.Origin != nil {
				span.origin, span.originKnown, span.muxShift = *row.Origin, true, row.MuxShift
			}
			spans = append(spans, span)
		}
	}
	return spans, next
}

// playlistSegmentRange returns the lowest and highest segment numbers a
// playlist lists.
func playlistSegmentRange(data []byte) (first, head int, found bool) {
	first, head = -1, -1
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		segNum, parseErr := ParseSegmentNumber(filepath.Base(string(bytes.TrimSpace(line))))
		if parseErr != nil {
			continue
		}
		if !found || segNum < first {
			first = segNum
		}
		head, found = max(head, segNum), true
	}
	return first, head, found
}

// recordedLiveSpanProgress fills in how far a generation that never closed
// got, from its own playlist on disk, and reports whether that playlist is
// its own. A generation's own playlist starts at its first segment; one that
// starts elsewhere was written by another generation, for example after a
// failed record write left this row stale.
func recordedLiveSpanProgress(root *os.Root, row subtitleSpanRecordRow, followsFFmpeg bool, span *subtitleCueSpan) bool {
	data := recordedLivePlaylist(root, row)
	first, head, ok := playlistSegmentRange(data)
	if !ok || first != row.Start {
		return false
	}
	span.end = head
	if followsFFmpeg {
		durations, final, own := generationPlaylistTimeline(data, row.Start)
		if !own {
			return false
		}
		span.durations, span.final = durations, final
	}
	return true
}

// recordedLivePlaylist returns the playlist on disk when it may be the one a
// generation that never closed wrote, or nil when it is the playlist that
// generation inherited.
func recordedLivePlaylist(root *os.Root, row subtitleSpanRecordRow) []byte {
	f, err := root.Open("stream.m3u8")
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || row.InheritedPlaylist.matches(info) {
		return nil
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil
	}
	return data
}

func (s *TranscodeSession) saveSubtitleSpans() {
	if s.outputDir == "" {
		return
	}
	s.mu.Lock()
	opts := s.opts
	if s.outputRemoved {
		s.mu.Unlock()
		return
	}
	record := subtitleSpanRecord{Track: opts.SubtitleTrackIndex, SegmentDuration: renditionSegmentDuration(opts), OnDemand: subtitleRenditionsOnDemand(opts)}
	for _, span := range s.subtitleSpans {
		startTime := span.startTime
		row := subtitleSpanRecordRow{File: span.file, Start: span.start, End: span.end, StartTime: &startTime, Durations: span.durations, Final: span.final}
		if span.originKnown {
			row.Origin, row.MuxShift = &span.origin, span.muxShift
		}
		if span.end < 0 {
			row.InheritedPlaylist = recordedPlaylist(s.inheritedManifest)
		}
		record.Spans = append(record.Spans, row)
	}
	s.mu.Unlock()
	if !subtitleRenditionActive(opts) || len(record.Spans) == 0 {
		return
	}
	data, err := json.Marshal(record)
	if err != nil {
		return
	}
	tmp := filepath.Join(s.outputDir, fmt.Sprintf(".%s.tmp", subtitleSpanRecordName))
	if err = os.WriteFile(tmp, data, 0o600); err == nil {
		err = os.Rename(tmp, filepath.Join(s.outputDir, subtitleSpanRecordName))
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
}
