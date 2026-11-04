package handlers

import (
	"context"
	"fmt"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"time"
)

type cueIndexReadsKeyV3 struct{}
type cueIndexReadsV3 struct {
	lookup  func(context.Context, string) (playback.CueIndexedSubtitles, error)
	timeout time.Duration
}

func newCueIndexReadsV3() *cueIndexReadsV3 {
	return &cueIndexReadsV3{lookup: playback.CueIndexedSubtitleTracks, timeout: time.Second}
}

const cueIndexedSubtitleTrackTimeoutV3 = time.Second

func cueIndexedSubtitleTracksV3(ctx context.Context, file *models.MediaFile) func() *playback.CueIndexedSubtitles {
	return func() *playback.CueIndexedSubtitles {
		if file == nil {
			return nil
		}
		r := &cueIndexReadsV3{lookup: playback.CueIndexedSubtitleTracks, timeout: cueIndexedSubtitleTrackTimeoutV3}
		if v, ok := ctx.Value(cueIndexReadsKeyV3{}).(*cueIndexReadsV3); ok {
			r = v
		}
		c, cancel := context.WithTimeout(ctx, r.timeout)
		defer cancel()
		out := make(chan playback.CueIndexedSubtitles, 1)
		go func() {
			v, e := r.lookup(c, file.FilePath)
			if e == nil {
				out <- v
			}
		}()
		select {
		case v := <-out:
			return &v
		case <-c.Done():
			return nil
		}
	}
}

func virtualFrameRateV3(s string) float64 {
	var n, d float64
	if _, e := fmt.Sscanf(s, "%f/%f", &n, &d); e == nil && d != 0 {
		return n / d
	}
	if _, e := fmt.Sscanf(s, "%f", &n); e == nil {
		return n
	}
	return 0
}
