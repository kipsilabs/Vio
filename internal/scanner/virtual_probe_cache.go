package scanner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/Silo-Server/silo-server/internal/models"
)

const (
	defaultVirtualProbeCacheTTL     = 10 * time.Minute
	defaultVirtualProbeCacheEntries = 256
)

// VirtualProbeTimeout bounds one playback-time probe. The cache runs the real
// probe under this timeout and the playback handler uses it as its background
// probe budget, so the two cannot drift: a caller that stops waiting before
// this fires has not learned a verdict, while the probe itself keeps running to
// completion. It covers the ffprobe and the nested copy-safety and Dolby Vision
// scans in ProbeVirtualSource.
const VirtualProbeTimeout = 60 * time.Second

type virtualProbeCacheEntry struct {
	file      *models.MediaFile
	expiresAt time.Time
	usedAt    time.Time
}

// VirtualProbeCache bounds and coalesces playback-time ffprobe work. Its key
// includes the selected provider and resolved-source fingerprint. An exact
// result= URI is the provider contract's stable byte identity, so rotating
// query credentials on the same provider path can reuse its bounded probe.
// Unselected profile/base URIs retain the complete URL because their top
// result may change between calls.
type VirtualProbeCache struct {
	mu         sync.Mutex
	entries    map[string]virtualProbeCacheEntry
	ttl        time.Duration
	maxEntries int
	group      singleflight.Group
}

type VirtualProbeFunc func(context.Context, string, *models.MediaFile) (*models.MediaFile, error)

func NewVirtualProbeCache(ttl time.Duration, maxEntries int) *VirtualProbeCache {
	if ttl <= 0 {
		ttl = defaultVirtualProbeCacheTTL
	}
	if maxEntries <= 0 {
		maxEntries = defaultVirtualProbeCacheEntries
	}
	return &VirtualProbeCache{entries: make(map[string]virtualProbeCacheEntry), ttl: ttl, maxEntries: maxEntries}
}

func (c *VirtualProbeCache) Probe(
	ctx context.Context,
	sourceURL string,
	file *models.MediaFile,
	probe VirtualProbeFunc,
) (*models.MediaFile, error) {
	if file == nil {
		return nil, nil
	}
	if probe == nil {
		return file, errors.New("virtual probe function is not configured")
	}
	inputSnapshot := cloneVirtualProbeFile(file)
	if c == nil {
		return probe(ctx, sourceURL, inputSnapshot)
	}
	key := virtualProbeCacheKey(sourceURL, inputSnapshot)
	now := time.Now()
	if cached := c.load(key, now); cached != nil {
		return cached, nil
	}
	ch := c.group.DoChan(key, func() (any, error) {
		if cached := c.load(key, time.Now()); cached != nil {
			return cached, nil
		}
		probeCtx, probeCancel := context.WithTimeout(context.WithoutCancel(ctx), VirtualProbeTimeout)
		defer probeCancel()
		workerSnapshot := cloneVirtualProbeFile(inputSnapshot)
		probed, err := probe(probeCtx, sourceURL, workerSnapshot)
		if err != nil || probed == nil {
			return probed, err
		}
		c.store(key, probed, time.Now())
		return cloneVirtualProbeFile(probed), nil
	})
	select {
	case <-ctx.Done():
		return cloneVirtualProbeFile(inputSnapshot), ctx.Err()
	case res := <-ch:
		if res.Err != nil || res.Val == nil {
			return cloneVirtualProbeFile(inputSnapshot), res.Err
		}
		probed, ok := res.Val.(*models.MediaFile)
		if !ok {
			return cloneVirtualProbeFile(inputSnapshot), errors.New("virtual probe returned an invalid media file")
		}
		return cloneVirtualProbeFile(probed), nil
	}
}

// Lookup returns a completed probe cached for this source without starting a
// new one. It is the cache-only seam the playback handler uses to recover
// evidence from a probe that outlived the caller's wait. A nil result means no
// completed probe is available.
func (c *VirtualProbeCache) Lookup(sourceURL string, file *models.MediaFile) *models.MediaFile {
	if c == nil || file == nil {
		return nil
	}
	return c.load(virtualProbeCacheKey(sourceURL, file), time.Now())
}

func (c *VirtualProbeCache) load(key string, now time.Time) *models.MediaFile {
	c.mu.Lock()
	defer c.mu.Unlock()
	for candidateKey, entry := range c.entries {
		if !now.Before(entry.expiresAt) {
			delete(c.entries, candidateKey)
		}
	}
	entry, ok := c.entries[key]
	if !ok {
		return nil
	}
	entry.usedAt = now
	c.entries[key] = entry
	return cloneVirtualProbeFile(entry.file)
}

func (c *VirtualProbeCache) store(key string, file *models.MediaFile, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for candidateKey, entry := range c.entries {
		if !now.Before(entry.expiresAt) {
			delete(c.entries, candidateKey)
		}
	}
	for len(c.entries) >= c.maxEntries {
		oldestKey := ""
		var oldest time.Time
		for candidateKey, entry := range c.entries {
			if oldestKey == "" || entry.usedAt.Before(oldest) {
				oldestKey, oldest = candidateKey, entry.usedAt
			}
		}
		delete(c.entries, oldestKey)
	}
	c.entries[key] = virtualProbeCacheEntry{
		file: cloneVirtualProbeFile(file), expiresAt: now.Add(c.ttl), usedAt: now,
	}
}

func virtualProbeCacheKey(sourceURL string, file *models.MediaFile) string {
	streamIdentity := strings.TrimSpace(sourceURL)
	canonical := ""
	fileID := 0
	ownerID := 0
	if file != nil {
		canonical = file.FilePath
		fileID = file.ID
		ownerID = file.VirtualOwnerInstallationID
	}
	if selected, err := url.Parse(canonical); err == nil && strings.TrimSpace(selected.Query().Get("result")) != "" {
		if providerURL, parseErr := url.Parse(streamIdentity); parseErr == nil && providerURL.IsAbs() {
			providerURL.RawQuery = ""
			providerURL.Fragment = ""
			streamIdentity = providerURL.String()
		}
	}
	// The completeness gate's verdict depends on the input's expected-audio
	// signal: a video-without-audio enumeration is complete for an item that
	// declares no audio and partial for one that does. A cached result measured
	// under one expectation must therefore never be replayed to a probe under
	// the other, or a thin enumeration cached for a video-only input would be
	// served — and, through the cache-only recovery path, persisted as verified —
	// to a row that declares audio without the gate ever seeing it. Fold the
	// signal into the key so both expectations re-run the real probe.
	expectAudio := virtualSourceExpectsAudio(file)
	digest := sha256.Sum256([]byte(strconv.Itoa(fileID) + "\x00" + strconv.Itoa(ownerID) + "\x00" + canonical + "\x00" + streamIdentity + "\x00" + strconv.FormatBool(expectAudio)))
	return hex.EncodeToString(digest[:])
}

func cloneVirtualProbeFile(file *models.MediaFile) *models.MediaFile {
	if file == nil {
		return nil
	}
	clone := *file
	if len(file.IdentityJSON) > 0 {
		clone.IdentityJSON = append([]byte(nil), file.IdentityJSON...)
	}
	if len(file.VideoTracks) > 0 {
		clone.VideoTracks = make([]models.VideoTrack, len(file.VideoTracks))
		for i, vt := range file.VideoTracks {
			clone.VideoTracks[i] = vt
			if vt.DVProvenanceCurrent != nil {
				val := *vt.DVProvenanceCurrent
				clone.VideoTracks[i].DVProvenanceCurrent = &val
			}
			if vt.MultiplePPS != nil {
				val := *vt.MultiplePPS
				clone.VideoTracks[i].MultiplePPS = &val
			}
			if vt.DVRPUStrippable != nil {
				val := *vt.DVRPUStrippable
				clone.VideoTracks[i].DVRPUStrippable = &val
			}
		}
	}
	clone.AudioTracks = append([]models.AudioTrack(nil), file.AudioTracks...)
	clone.SubtitleTracks = append([]models.SubtitleTrack(nil), file.SubtitleTracks...)
	clone.ExternalSubtitles = append([]models.ExternalSubtitle(nil), file.ExternalSubtitles...)
	clone.Chapters = append([]models.MediaChapter(nil), file.Chapters...)
	return &clone
}
