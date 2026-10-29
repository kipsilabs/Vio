package handlers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/notifications"
)

// versionCheckEventHandler wires the stamp callback and catalog hub the
// liveness check publishes through, recording every callback invocation.
func versionCheckEventHandler(bus *recordingEventBus, markErr error) (*CatalogResourceHandler, *int) {
	calls := 0
	h := &CatalogResourceHandler{
		Events: notifications.NewHub("versions-check", bus),
		MarkVirtualFailed: func(context.Context, int, string, *time.Time) error {
			calls++
			return markErr
		},
	}
	return h, &calls
}

// A stamp that actually lands is what removes the row from the version list,
// so it must announce the change: clients then learn about the newly dead row
// in seconds instead of on cache expiry.
func TestVersionCheckStampPublishesVersionsUpdated(t *testing.T) {
	bus := &recordingEventBus{}
	h, calls := versionCheckEventHandler(bus, nil)
	file := &models.MediaFile{ID: 7, ContentID: "movie:check-event", FilePath: "virtual://movie/check-event?result=a"}

	h.stampVirtualCandidateFailed(context.Background(), file.ID, file)

	if *calls != 1 {
		t.Fatalf("MarkVirtualFailed calls = %d, want 1", *calls)
	}
	if n := recordingEventCount(bus, "catalog.item.changed", "versions_updated", file.ContentID); n != 1 {
		t.Fatalf("versions_updated for %s fired %d times, want 1; payloads=%v",
			file.ContentID, n, recordingEventPayloads(bus))
	}
	if payloads := recordingEventPayloads(bus); len(payloads) != 1 {
		t.Fatalf("published %d events, want exactly 1: %v", len(payloads), payloads)
	}
}

// An episode virtual row carries content_id = series id and episode_id =
// episode id, while the refresh path names the episode. Both ids are announced
// so whichever list the client is showing invalidates.
func TestVersionCheckStampPublishesEpisodeScope(t *testing.T) {
	bus := &recordingEventBus{}
	h, _ := versionCheckEventHandler(bus, nil)
	file := &models.MediaFile{
		ID:        21,
		ContentID: "series:check-event",
		EpisodeID: "episode:check-event",
		FilePath:  "virtual://series/check-event?result=a",
	}

	h.stampVirtualCandidateFailed(context.Background(), file.ID, file)

	for _, id := range []string{file.ContentID, file.EpisodeID} {
		if n := recordingEventCount(bus, "catalog.item.changed", "versions_updated", id); n != 1 {
			t.Fatalf("versions_updated for %s fired %d times, want 1; payloads=%v",
				id, n, recordingEventPayloads(bus))
		}
	}
}

// Nothing is announced when nothing changed: an already-stamped row, a failed
// write, or a handler with no hub must all stay silent.
func TestVersionCheckStampPublishesNothingWithoutAStamp(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	file := &models.MediaFile{ID: 9, ContentID: "movie:check-silent", FilePath: "virtual://movie/check-silent?result=a"}

	t.Run("already stamped", func(t *testing.T) {
		bus := &recordingEventBus{}
		h, calls := versionCheckEventHandler(bus, nil)
		stamped := *file
		stamped.FailedAt = &past

		h.stampVirtualCandidateFailed(context.Background(), stamped.ID, &stamped)

		if *calls != 0 {
			t.Fatalf("MarkVirtualFailed calls = %d, want 0", *calls)
		}
		if payloads := recordingEventPayloads(bus); len(payloads) != 0 {
			t.Fatalf("already-stamped row published %v", payloads)
		}
	})

	t.Run("write error", func(t *testing.T) {
		bus := &recordingEventBus{}
		h, _ := versionCheckEventHandler(bus, errors.New("write failed"))

		h.stampVirtualCandidateFailed(context.Background(), file.ID, file)

		if payloads := recordingEventPayloads(bus); len(payloads) != 0 {
			t.Fatalf("failed write published %v", payloads)
		}
	})

	t.Run("no hub", func(t *testing.T) {
		bus := &recordingEventBus{}
		h, _ := versionCheckEventHandler(bus, nil)
		h.Events = nil

		h.stampVirtualCandidateFailed(context.Background(), file.ID, file)

		if payloads := recordingEventPayloads(bus); len(payloads) != 0 {
			t.Fatalf("nil hub published %v", payloads)
		}
	})
}
