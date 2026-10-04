package handlers

import (
	"testing"
	"time"
)

// An empty provider answer suppresses a hot re-list inside the window, and a
// non-empty answer clears it so a recovered provider lists immediately.
func TestVirtualEmptyListDamperSuppressesHotRelist(t *testing.T) {
	key := "test-neutral\x005"
	virtualEmptyListMarks.marks = make(map[string]time.Time)
	now := time.Now()
	if virtualEmptyListSuppressed(key, now) {
		t.Fatal("fresh key must not be suppressed")
	}
	virtualEmptyListRecord(key, now)
	if !virtualEmptyListSuppressed(key, now.Add(5*time.Second)) {
		t.Fatal("empty answer must suppress re-list inside window")
	}
	if virtualEmptyListSuppressed(key, now.Add(virtualEmptyListWindow+time.Second)) {
		t.Fatal("suppression must lapse after window")
	}
	virtualEmptyListRecord(key, now)
	virtualEmptyListClear(key)
	if virtualEmptyListSuppressed(key, now.Add(time.Second)) {
		t.Fatal("non-empty answer must clear suppression")
	}
}
