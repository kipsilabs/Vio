package playback_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Silo-Server/silo-server/internal/playback"
)

type throttleSettings map[string]string

func (s throttleSettings) Get(_ context.Context, key string) (string, error) {
	return s[key], nil
}

// erroringThrottleSettings returns configured values but fails the read, so a
// test can prove the resolver fails closed instead of trusting a zero value.
type erroringThrottleSettings struct {
	values map[string]string
	err    error
}

func (s erroringThrottleSettings) Get(_ context.Context, key string) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	return s.values[key], nil
}

// selectiveErrorThrottleSettings fails only the reads named in failKeys so a
// test can reach a threshold-only failure after a successful enable read. It
// records every requested key to prove which lookups the resolver made.
type selectiveErrorThrottleSettings struct {
	values    map[string]string
	failKeys  map[string]error
	requested []string
}

func (s *selectiveErrorThrottleSettings) Get(_ context.Context, key string) (string, error) {
	s.requested = append(s.requested, key)
	if err, ok := s.failKeys[key]; ok {
		return "", err
	}
	return s.values[key], nil
}

func (s *selectiveErrorThrottleSettings) requestedKeys() []string {
	return s.requested
}

type recordingThrottleStarter struct {
	thresholds []int
}

func (s *recordingThrottleStarter) StartThrottler(threshold int) {
	s.thresholds = append(s.thresholds, threshold)
}

func TestStartConfiguredTranscodeThrottler(t *testing.T) {
	tests := []struct {
		name       string
		settings   playback.TranscodeThrottleSettings
		thresholds []int
	}{
		{name: "enabled by default when absent", settings: throttleSettings{}, thresholds: []int{300}},
		{name: "enabled by default when empty", settings: throttleSettings{"enable_transcode_throttle": ""}, thresholds: []int{300}},
		{name: "explicitly disabled", settings: throttleSettings{"enable_transcode_throttle": "false"}},
		{name: "configured", settings: throttleSettings{"enable_transcode_throttle": "true", "transcode_throttle_seconds": "180"}, thresholds: []int{180}},
		{name: "invalid threshold uses default", settings: throttleSettings{"enable_transcode_throttle": "true", "transcode_throttle_seconds": "invalid"}, thresholds: []int{300}},
		{
			name:     "read error fails closed even with explicit false",
			settings: erroringThrottleSettings{values: map[string]string{"enable_transcode_throttle": "false"}, err: errors.New("settings store unavailable")},
		},
		{
			name: "enable absent with threshold read error does not start",
			settings: &selectiveErrorThrottleSettings{
				failKeys: map[string]error{"transcode_throttle_seconds": errors.New("settings store unavailable")},
			},
		},
		{
			name: "enable true with threshold read error does not start",
			settings: &selectiveErrorThrottleSettings{
				values:   map[string]string{"enable_transcode_throttle": "true"},
				failKeys: map[string]error{"transcode_throttle_seconds": errors.New("settings store unavailable")},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			starter := &recordingThrottleStarter{}
			playback.StartConfiguredTranscodeThrottler(context.Background(), tt.settings, starter)
			if len(starter.thresholds) != len(tt.thresholds) {
				t.Fatalf("thresholds = %v, want %v", starter.thresholds, tt.thresholds)
			}
			for i, want := range tt.thresholds {
				if starter.thresholds[i] != want {
					t.Fatalf("thresholds = %v, want %v", starter.thresholds, tt.thresholds)
				}
			}
		})
	}
}

func TestConfiguredTranscodeThrottleSeconds(t *testing.T) {
	tests := []struct {
		name     string
		settings playback.TranscodeThrottleSettings
		want     int
	}{
		{name: "enabled by default when absent", settings: throttleSettings{}, want: 300},
		{name: "enabled by default when empty", settings: throttleSettings{"enable_transcode_throttle": ""}, want: 300},
		{name: "explicitly disabled", settings: throttleSettings{"enable_transcode_throttle": "false"}, want: 0},
		{name: "configured", settings: throttleSettings{"enable_transcode_throttle": "true", "transcode_throttle_seconds": "180"}, want: 180},
		{name: "positive value below executor minimum is clamped", settings: throttleSettings{"enable_transcode_throttle": "true", "transcode_throttle_seconds": "30"}, want: 60},
		{name: "invalid threshold uses default", settings: throttleSettings{"enable_transcode_throttle": "true", "transcode_throttle_seconds": "invalid"}, want: 300},
		{
			name:     "read error with explicit false stays disabled",
			settings: erroringThrottleSettings{values: map[string]string{"enable_transcode_throttle": "false"}, err: errors.New("settings store unavailable")},
			want:     0,
		},
		{
			name:     "read error does not enable the default",
			settings: erroringThrottleSettings{values: map[string]string{"enable_transcode_throttle": "true", "transcode_throttle_seconds": "180"}, err: errors.New("settings store unavailable")},
			want:     0,
		},
		{
			name: "enable absent with threshold read error fails closed",
			settings: &selectiveErrorThrottleSettings{
				failKeys: map[string]error{"transcode_throttle_seconds": errors.New("settings store unavailable")},
			},
			want: 0,
		},
		{
			name: "enable true with threshold read error fails closed",
			settings: &selectiveErrorThrottleSettings{
				values:   map[string]string{"enable_transcode_throttle": "true"},
				failKeys: map[string]error{"transcode_throttle_seconds": errors.New("settings store unavailable")},
			},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := playback.ConfiguredTranscodeThrottleSeconds(context.Background(), tt.settings); got != tt.want {
				t.Fatalf("ConfiguredTranscodeThrottleSeconds() = %d, want %d", got, tt.want)
			}
		})
	}
}

// An explicit false disables throttling without ever reading the threshold, so a
// broken threshold setting cannot arm a throttler the admin turned off.
func TestConfiguredTranscodeThrottleSecondsSkipsThresholdWhenDisabled(t *testing.T) {
	settings := &selectiveErrorThrottleSettings{
		values: map[string]string{"enable_transcode_throttle": "false"},
	}
	if got := playback.ConfiguredTranscodeThrottleSeconds(context.Background(), settings); got != 0 {
		t.Fatalf("ConfiguredTranscodeThrottleSeconds() = %d, want 0", got)
	}
	for _, key := range settings.requestedKeys() {
		if key == "transcode_throttle_seconds" {
			t.Fatal("resolver read the threshold setting after an explicit false")
		}
	}
}
