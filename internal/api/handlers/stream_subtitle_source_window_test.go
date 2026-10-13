package handlers

import (
	"context"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// The prod mismatch: a session bound to the provider-neutral row 4912233 serves
// subtitles whose inventory advertises the adopted result-candidate row 5382473.
// The strict session.MediaFileID/RequestedMediaFileID window rejects that id, so
// the subtitle fetch 400s for the very URL the session published. The accepted
// window must include the row naming the session's bound virtual candidate, while
// every other unrelated row still fails closed.
func TestAcceptedSubtitleSourceFileIDAllowsBoundVirtualCandidateRow(t *testing.T) {
	candidateURI := "virtual://series/tt/2/16?profile=4K+Dolby+Vision&result=cfad71f2"
	session := &playback.Session{
		MediaFileID:          4912233,
		RequestedMediaFileID: 4912233,
		VirtualSourceURI:     candidateURI,
	}
	handler := &StreamHandler{fileResolver: mapPlaybackFileResolver{files: map[int]*models.MediaFile{
		4912233: {ID: 4912233, FilePath: "virtual://series/tt/2/16?profile=4K+Dolby+Vision"},
		5382473: {ID: 5382473, FilePath: candidateURI},
		7001:    {ID: 7001, FilePath: "virtual://series/tt/2/16?profile=4K+Dolby+Vision&result=9f7056a1459d1a3dd21327fe"},
		7002:    {ID: 7002, FilePath: "/media/library/movie.mkv"},
	}}}

	// The existing window keeps working.
	if id, err := handler.acceptedSubtitleSourceFileID(context.Background(), "4912233", session); err != nil || id != 4912233 {
		t.Fatalf("session row: id=%d err=%v, want 4912233", id, err)
	}
	// The adopted virtual candidate row the session advertises is accepted.
	if id, err := handler.acceptedSubtitleSourceFileID(context.Background(), "5382473", session); err != nil || id != 5382473 {
		t.Fatalf("adopted candidate row: id=%d err=%v, want 5382473", id, err)
	}
	// A row naming a different candidate under the same profile is not the
	// session's release and must fail closed.
	if _, err := handler.acceptedSubtitleSourceFileID(context.Background(), "7001", session); err == nil {
		t.Fatal("a sibling release row was accepted into the subtitle window")
	}
	// A non-virtual row is never admitted by the virtual widening.
	if _, err := handler.acceptedSubtitleSourceFileID(context.Background(), "7002", session); err == nil {
		t.Fatal("a local media row was accepted into the virtual subtitle window")
	}
	// An unknown row is rejected.
	if _, err := handler.acceptedSubtitleSourceFileID(context.Background(), "9999", session); err == nil {
		t.Fatal("an unknown row was accepted into the subtitle window")
	}
	// The strict helper is unchanged: the widening is route-scoped.
	if _, err := subtitleSourceFile("5382473", session); err == nil {
		t.Fatal("the strict subtitle source window accepted a foreign row")
	}
	// A non-virtual session gets no widening.
	neutral := &playback.Session{MediaFileID: 4912233, RequestedMediaFileID: 4912233}
	if _, err := handler.acceptedSubtitleSourceFileID(context.Background(), "5382473", neutral); err == nil {
		t.Fatal("a non-virtual session accepted a foreign row")
	}
}
