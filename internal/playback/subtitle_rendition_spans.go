package playback

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const subtitleSpanRecordName = "subtitle_spans.json"

type subtitleSpanRecord struct {
	Track           int                     `json:"track"`
	SegmentDuration int                     `json:"segment_duration"`
	Spans           []subtitleSpanRecordRow `json:"spans"`
}

type subtitleSpanRecordRow struct {
	File   string   `json:"file"`
	Start  int      `json:"start"`
	End    int      `json:"end"`
	Origin *float64 `json:"origin,omitempty"`
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
	data, err := os.ReadFile(filepath.Join(opts.OutputDir, subtitleSpanRecordName))
	if err != nil {
		return nil, 0
	}
	var record subtitleSpanRecord
	if json.Unmarshal(data, &record) != nil {
		return nil, 0
	}
	var spans []subtitleCueSpan
	var next uint64
	for _, row := range record.Spans {
		generation, ok := subtitleCueFileGeneration(row.File)
		if !ok {
			continue
		}
		if generation+1 > next {
			next = generation + 1
		}
		if record.Track != opts.SubtitleTrackIndex || record.SegmentDuration != opts.SegmentDuration || row.Start < 0 {
			continue
		}
		if _, err := os.Stat(filepath.Join(opts.OutputDir, row.File)); err != nil {
			continue
		}
		span := subtitleCueSpan{file: row.File, start: row.Start, end: row.End}
		if span.end < 0 {
			continue
		}
		if row.Origin != nil {
			span.origin, span.originKnown = *row.Origin, true
		}
		spans = append(spans, span)
	}
	return spans, next
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
	record := subtitleSpanRecord{Track: opts.SubtitleTrackIndex, SegmentDuration: opts.SegmentDuration}
	for _, span := range s.subtitleSpans {
		row := subtitleSpanRecordRow{File: span.file, Start: span.start, End: span.end}
		if span.originKnown {
			row.Origin = &span.origin
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
