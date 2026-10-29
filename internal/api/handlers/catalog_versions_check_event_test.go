package handlers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/notifications"
)

// versionCheckFileState is an in-memory stand-in for the file repository: it
// serves the row the handler re-reads after a health write and applies the same
// fenced stamp semantics the database does, including the no-op paths that
// still return a nil error (a delivery-grace skip and a rotated candidate).
type versionCheckFileState struct {
	row     *models.MediaFile
	markErr error
	// skipGrace makes a stamp a no-op while still returning nil, mirroring the
	// repository's VirtualCandidateDeliveryGrace branch.
	skipGrace bool
}

func (s *versionCheckFileState) GetByID(context.Context, int) (*models.MediaFile, error) {
	if s.row == nil {
		return nil, errors.New("file not found")
	}
	clone := *s.row
	return &clone, nil
}

func (s *versionCheckFileState) MarkVirtualCandidateFailed(_ context.Context, _ int, expectedFilePath string, observedFailedAt *time.Time) error {
	if s.markErr != nil {
		return s.markErr
	}
	if s.skipGrace || s.row == nil || s.row.FilePath != expectedFilePath {
		return nil
	}
	switch {
	case s.row.FailedAt == nil && observedFailedAt == nil:
	case s.row.FailedAt != nil && observedFailedAt != nil && s.row.FailedAt.Equal(*observedFailedAt):
	default:
		return nil
	}
	stamp := time.Now()
	s.row.FailedAt = &stamp
	return nil
}

// versionCheckEventHandler wires the stamp callback, the post-write re-read, and
// the catalog hub the liveness check publishes through, recording every
// callback invocation.
func versionCheckEventHandler(bus *recordingEventBus, state *versionCheckFileState) (*CatalogResourceHandler, *int) {
	calls := 0
	h := &CatalogResourceHandler{
		FileResolver: state,
		Events:       notifications.NewHub("versions-check", bus),
		MarkVirtualFailed: func(ctx context.Context, fileID int, expectedFilePath string, observedFailedAt *time.Time) error {
			calls++
			return state.MarkVirtualCandidateFailed(ctx, fileID, expectedFilePath, observedFailedAt)
		},
	}
	return h, &calls
}

// A stamp that actually lands is what removes the row from the version list,
// so it must announce the change: clients then learn about the newly dead row
// in seconds instead of on cache expiry.
func TestVersionCheckStampPublishesVersionsUpdated(t *testing.T) {
	bus := &recordingEventBus{}
	state := &versionCheckFileState{row: &models.MediaFile{ID: 7, ContentID: "movie:check-event", FilePath: "virtual://movie/check-event?result=a"}}
	h, calls := versionCheckEventHandler(bus, state)
	file := *state.row

	if confirmed := h.stampVirtualCandidateFailed(context.Background(), file.ID, &file); !confirmed {
		t.Fatal("a landed stamp was not reported as confirmed")
	}

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
	state := &versionCheckFileState{row: &models.MediaFile{
		ID:        21,
		ContentID: "series:check-event",
		EpisodeID: "episode:check-event",
		FilePath:  "virtual://series/check-event?result=a",
	}}
	h, _ := versionCheckEventHandler(bus, state)
	file := *state.row

	if confirmed := h.stampVirtualCandidateFailed(context.Background(), file.ID, &file); !confirmed {
		t.Fatal("a landed stamp was not reported as confirmed")
	}

	for _, id := range []string{file.ContentID, file.EpisodeID} {
		if n := recordingEventCount(bus, "catalog.item.changed", "versions_updated", id); n != 1 {
			t.Fatalf("versions_updated for %s fired %d times, want 1; payloads=%v",
				id, n, recordingEventPayloads(bus))
		}
	}
}

// versionCheckSilentFile is the fresh, unstamped row the silent-path tests
// attempt to stamp.
func versionCheckSilentFile() *models.MediaFile {
	return &models.MediaFile{ID: 9, ContentID: "movie:check-silent", FilePath: "virtual://movie/check-silent?result=a"}
}

// Nothing is announced when nothing changed: an already-stamped row, a missing
// stamp callback, and a handler with no hub must all stay silent.
func TestVersionCheckStampPublishesNothingWithoutAStamp(t *testing.T) {
	past := time.Now().Add(-time.Hour)

	t.Run("already stamped", func(t *testing.T) {
		bus := &recordingEventBus{}
		stamped := versionCheckSilentFile()
		stamped.FailedAt = &past
		state := &versionCheckFileState{row: stamped}
		h, calls := versionCheckEventHandler(bus, state)
		file := *stamped

		if confirmed := h.stampVirtualCandidateFailed(context.Background(), file.ID, &file); confirmed {
			t.Fatal("an already-stamped row was reported as a new durable failure")
		}

		if *calls != 0 {
			t.Fatalf("MarkVirtualFailed calls = %d, want 0", *calls)
		}
		if payloads := recordingEventPayloads(bus); len(payloads) != 0 {
			t.Fatalf("already-stamped row published %v", payloads)
		}
	})

	t.Run("no hub", func(t *testing.T) {
		bus := &recordingEventBus{}
		state := &versionCheckFileState{row: versionCheckSilentFile()}
		h, _ := versionCheckEventHandler(bus, state)
		h.Events = nil
		file := *state.row

		if confirmed := h.stampVirtualCandidateFailed(context.Background(), file.ID, &file); !confirmed {
			t.Fatal("a landed stamp was not reported as confirmed")
		}
		if payloads := recordingEventPayloads(bus); len(payloads) != 0 {
			t.Fatalf("nil hub published %v", payloads)
		}
	})

	t.Run("no stamp callback", func(t *testing.T) {
		bus := &recordingEventBus{}
		state := &versionCheckFileState{row: versionCheckSilentFile()}
		h, _ := versionCheckEventHandler(bus, state)
		h.MarkVirtualFailed = nil
		file := *state.row

		if confirmed := h.stampVirtualCandidateFailed(context.Background(), file.ID, &file); confirmed {
			t.Fatal("an unwired stamp callback was reported as a durable failure")
		}
		if payloads := recordingEventPayloads(bus); len(payloads) != 0 {
			t.Fatalf("unwired stamp callback published %v", payloads)
		}
	})
}

// A stamp call can return a nil error and still change nothing: the repository
// deliberately no-ops for a delivery-grace skip and for a fenced-out write on a
// rotated row. None of those paths may be reported as a new durable failure or
// publish a versions_updated event.
func TestVersionCheckStampSilentOnUnconfirmedWrite(t *testing.T) {
	cases := []struct {
		name   string
		state  *versionCheckFileState
		mutate func(state *versionCheckFileState)
		want   string
	}{
		{
			name:  "write error",
			state: &versionCheckFileState{markErr: errors.New("write failed")},
			want:  "a failed write was reported as a durable failure",
		},
		{
			name:  "delivery grace skip",
			state: &versionCheckFileState{skipGrace: true},
			want:  "a delivery-grace skip was reported as a durable failure",
		},
		{
			name:  "concurrent identity change",
			state: &versionCheckFileState{},
			mutate: func(state *versionCheckFileState) {
				// The resolver judged the original identity while another
				// writer rotated the row under the same id.
				state.row.FilePath = "virtual://movie/check-silent?result=replacement"
			},
			want: "a fenced-out stamp on a rotated row was reported as a durable failure",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bus := &recordingEventBus{}
			tc.state.row = versionCheckSilentFile()
			file := *tc.state.row
			if tc.mutate != nil {
				tc.mutate(tc.state)
			}
			h, calls := versionCheckEventHandler(bus, tc.state)

			if confirmed := h.stampVirtualCandidateFailed(context.Background(), file.ID, &file); confirmed {
				t.Fatal(tc.want)
			}
			if *calls != 1 {
				t.Fatalf("MarkVirtualFailed calls = %d, want 1", *calls)
			}
			if payloads := recordingEventPayloads(bus); len(payloads) != 0 {
				t.Fatalf("unconfirmed stamp published %v", payloads)
			}
			if tc.state.row.FailedAt != nil {
				t.Fatalf("unconfirmed stamp marked a row dead: %v", tc.state.row.FailedAt)
			}
		})
	}
}
