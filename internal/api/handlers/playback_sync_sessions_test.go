package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestSyncSessionsTeardownRace(t *testing.T) {
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	deadlineCtx, cancelDeadline := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	t.Cleanup(cancelDeadline)

	tests := []struct {
		name string
		ctx  func() context.Context
		err  error
		want bool
	}{
		{"canceled context", func() context.Context { return canceledCtx }, context.Canceled, true},
		{"wrapped canceled error", func() context.Context { return canceledCtx }, fmt.Errorf("sync sessions: %w", context.Canceled), true},
		{"expired deadline", func() context.Context { return deadlineCtx }, context.DeadlineExceeded, true},
		{"live context", context.Background, context.Canceled, false},
		{"store error on canceled context", func() context.Context { return canceledCtx }, errors.New("database unavailable"), false},
		{"nil context", func() context.Context { return nil }, context.Canceled, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := syncSessionsTeardownRace(tt.ctx(), tt.err); got != tt.want {
				t.Fatalf("syncSessionsTeardownRace() = %v, want %v", got, tt.want)
			}
		})
	}
}

// A canceled replan sync is the replan's own teardown finishing first, so it
// must not read as an ERROR. A genuine store failure keeps the ERROR.
func TestLogSyncSessionsFailureLevel(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	logSyncSessionsFailure(canceledCtx, "test_replan_canceled", context.Canceled)
	logSyncSessionsFailure(context.Background(), "test_store_failure", errors.New("database unavailable"))

	levels := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		if reason, _ := entry["reason"].(string); reason != "" {
			levels[reason], _ = entry["level"].(string)
		}
	}
	if got := levels["test_replan_canceled"]; got != "DEBUG" {
		t.Fatalf("canceled replan sync level = %q, want DEBUG", got)
	}
	if got := levels["test_store_failure"]; got != "ERROR" {
		t.Fatalf("store failure level = %q, want ERROR", got)
	}
}
