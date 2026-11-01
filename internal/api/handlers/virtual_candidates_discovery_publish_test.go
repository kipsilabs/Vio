package handlers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/notifications"
)

// discoveryPublishService builds the discovery service the router wires: a
// detail-page listing that persists through the shared sink and announces the
// persisted version list through the realtime hub.
func discoveryPublishService(bus *recordingEventBus, persist VirtualPlaybackStreamSink) *VirtualCandidatesRefreshService {
	source := refreshTestSource(7, "movie:publish", "virtual://movie/publish")
	detail := refreshTestDetail()
	detail.Type = "movie"
	return &VirtualCandidatesRefreshService{
		Detail: &fakeRefreshDetail{detail: detail},
		ListCached: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{{ID: "candidate", URI: "virtual://movie/publish?result=candidate"}}, nil
		}),
		ContentFiles: func(context.Context, string) ([]*models.MediaFile, error) {
			return []*models.MediaFile{source}, nil
		},
		Persist: persist,
		Events:  notifications.NewHub("discovery-publish", bus),
	}
}

// A detail-page discovery answers no longer waits for its listing, so the
// persisted version list is only visible to an open page through the
// versions_updated event this test pins: exactly one, published after the
// persist landed.
func TestVirtualCandidatesDiscoveryPublishesVersionsUpdated(t *testing.T) {
	bus := &recordingEventBus{}
	var order []string
	service := discoveryPublishService(bus, func(context.Context, *models.MediaFile, []VirtualPlaybackStream) error {
		order = append(order, "persist")
		return nil
	})

	if _, err := service.DiscoverVirtualCandidates(context.Background(), 1, "profile", "movie:publish", catalog.AccessFilter{}, VirtualDiscoveryDetail); err != nil {
		t.Fatalf("discovery: %v", err)
	}
	if len(order) != 1 {
		t.Fatalf("persist calls = %v, want one", order)
	}
	payload := waitForRecordingEvent(t, bus, 5*time.Second, "catalog.item.changed", "versions_updated", "movie:publish")
	if payload == "" {
		t.Fatal("discovery did not publish versions_updated")
	}
	if n := recordingEventCount(bus, "catalog.item.changed", "versions_updated", "movie:publish"); n != 1 {
		t.Fatalf("versions_updated fired %d times, want 1", n)
	}
}

// The event is gated on a successful persist: a listing that never reached the
// database must not tell clients to reload it.
func TestVirtualCandidatesDiscoveryPublishRequiresPersist(t *testing.T) {
	bus := &recordingEventBus{}
	service := discoveryPublishService(bus, func(context.Context, *models.MediaFile, []VirtualPlaybackStream) error {
		return errors.New("persist failed")
	})

	if _, err := service.DiscoverVirtualCandidates(context.Background(), 1, "profile", "movie:publish", catalog.AccessFilter{}, VirtualDiscoveryDetail); err == nil {
		t.Fatal("discovery persisted nothing but reported success")
	}
	if payload := recordingEventPayload(bus, "versions_updated"); payload != "" {
		t.Fatalf("versions_updated published without a persist: %s", payload)
	}
}

// A bare listing request prepares nothing and announces nothing: it is
// callable for any item by any client, and only a detail-page read persists a
// list a page is showing.
func TestVirtualCandidatesListingOnlyDoesNotPublishVersionsUpdated(t *testing.T) {
	bus := &recordingEventBus{}
	service := discoveryPublishService(bus, func(context.Context, *models.MediaFile, []VirtualPlaybackStream) error {
		return nil
	})

	if _, err := service.DiscoverVirtualCandidates(context.Background(), 1, "profile", "movie:publish", catalog.AccessFilter{}, VirtualDiscoveryListingOnly); err != nil {
		t.Fatalf("listing: %v", err)
	}
	if payload := recordingEventPayload(bus, "versions_updated"); payload != "" {
		t.Fatalf("listing-only discovery published versions_updated: %s", payload)
	}
}
