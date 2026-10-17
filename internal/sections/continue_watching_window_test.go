package sections

import (
	"context"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/userstore"
)

type fixedSettingsReader map[string]string

func (r fixedSettingsReader) Get(_ context.Context, key string) (string, error) {
	return r[key], nil
}

func progressAt(id string, updatedAt time.Time) userstore.WatchProgress {
	return userstore.WatchProgress{
		MediaItemID: id,
		UpdatedAt:   updatedAt.UTC().Format(time.RFC3339),
	}
}

// An entry older than the window is dropped, one inside it stays, and a zero
// cutoff keeps everything.
func TestDropProgressBefore(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-14 * 24 * time.Hour)
	entries := []userstore.WatchProgress{
		progressAt("inside", now.Add(-time.Hour)),
		progressAt("edge", now.Add(-13*24*time.Hour)),
		progressAt("stale", now.Add(-15*24*time.Hour)),
		progressAt("staler", now.Add(-30*24*time.Hour)),
	}

	kept, reached := dropProgressBefore(entries, cutoff)
	if !reached {
		t.Fatal("reached = false, want true once a stale entry is seen")
	}
	got := make([]string, 0, len(kept))
	for _, entry := range kept {
		got = append(got, entry.MediaItemID)
	}
	want := []string{"inside", "edge"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("kept = %v, want %v", got, want)
	}

	all, reached := dropProgressBefore(entries, time.Time{})
	if reached {
		t.Fatal("reached = true for a zero cutoff, want false")
	}
	if len(all) != len(entries) {
		t.Fatalf("kept %d entries with no limit, want %d", len(all), len(entries))
	}
}

// Ebook Continue Reading resume points use the same RFC3339 updated_at column,
// so the shared window filter drops stale ebook entries too.
func TestDropProgressBeforeFiltersEbookEntries(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-7 * 24 * time.Hour)
	ebook := func(id string, age time.Duration) userstore.WatchProgress {
		return userstore.WatchProgress{
			MediaItemID:     id,
			PositionSeconds: 12,
			DurationSeconds: 1,
			UpdatedAt:       now.Add(-age).UTC().Format(time.RFC3339),
		}
	}
	entries := []userstore.WatchProgress{
		ebook("read-recently", time.Hour),
		ebook("read-this-week", 6*24*time.Hour),
		ebook("read-last-month", 30*24*time.Hour),
	}

	kept, reached := dropProgressBefore(entries, cutoff)
	if !reached {
		t.Fatal("reached = false, want true")
	}
	if len(kept) != 2 || kept[0].MediaItemID != "read-recently" || kept[1].MediaItemID != "read-this-week" {
		t.Fatalf("kept = %+v, want the two entries inside the window", kept)
	}
}

func TestContinueWatchingCutoff(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		weeks    string
		wantZero bool
		want     time.Time
	}{
		{name: "two weeks", weeks: "2", want: now.Add(-14 * 24 * time.Hour)},
		{name: "zero means no limit", weeks: "0", wantZero: true},
		{name: "no stored value defaults to no limit", weeks: "", wantZero: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &Fetcher{
				Clock:    fixedClock(now),
				Settings: fixedSettingsReader{config.PlaybackContinueWatchingWeeksSettingKey: tt.weeks},
			}
			got := f.continueWatchingCutoff(context.Background())
			if tt.wantZero {
				if !got.IsZero() {
					t.Fatalf("cutoff = %v, want zero", got)
				}
				return
			}
			if !got.Equal(tt.want) {
				t.Fatalf("cutoff = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestContinueWatchingCutoffWithoutSettings(t *testing.T) {
	t.Parallel()

	f := &Fetcher{Clock: fixedClock(time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC))}
	if got := f.continueWatchingCutoff(context.Background()); !got.IsZero() {
		t.Fatalf("cutoff = %v, want zero without a settings reader", got)
	}
}
