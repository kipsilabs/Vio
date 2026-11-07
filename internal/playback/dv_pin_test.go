package playback

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

// First-verified-wins: a pin established for a file keeps its profile even when
// a later read of the same file reports a different one. This is the same-file
// stability the production incident needed: dv_profile 8 at start, 7 seconds
// later on a replan, on bytes that never changed.
func TestDVPinV3FirstVerifiedWinsForSameFile(t *testing.T) {
	file := &models.MediaFile{ID: 42, FilePath: "/media/movie.mkv"}
	pin := DVPinV3{}
	pin = PinDVPinV3(pin, file, 8)
	if pin != (DVPinV3{FileID: 42, Source: "/media/movie.mkv", Profile: 8}) {
		t.Fatalf("first pin = %#v", pin)
	}
	// A drifted read of the same file is refused.
	pin = PinDVPinV3(pin, file, 7)
	if pin.Profile != 8 {
		t.Fatalf("drifted read changed the pin to %d, want 8", pin.Profile)
	}
	// A move to a different effective file re-arms the pin from its own read.
	other := &models.MediaFile{ID: 84, FilePath: "/media/other.mkv"}
	pin = PinDVPinV3(pin, other, 5)
	if pin != (DVPinV3{FileID: 84, Source: "/media/other.mkv", Profile: 5}) {
		t.Fatalf("rotated file pin = %#v", pin)
	}
}

// A same-row virtual release rotation keeps the row id but changes the pinned
// candidate URI: the rotated bytes must not inherit the previous release's pin.
func TestDVPinV3SameRowRotationReArms(t *testing.T) {
	fileA := &models.MediaFile{ID: 42, FilePath: "virtual://movie/tt?result=A"}
	pin := PinDVPinV3(DVPinV3{}, fileA, 8)
	fileB := &models.MediaFile{ID: 42, FilePath: "virtual://movie/tt?result=B"}
	if got := PinDVPinV3(pin, fileB, 7); got != (DVPinV3{FileID: 42, Source: fileB.FilePath, Profile: 7}) {
		t.Fatalf("same-row rotation pin = %#v, want the rotated release's own read", got)
	}
}

// A zero profile is never evidence: it neither establishes nor clears a pin, so
// an unprobed row cannot lock the session out of a later genuine profile.
func TestDVPinV3ZeroProfileIsNotEvidence(t *testing.T) {
	file := &models.MediaFile{ID: 42, FilePath: "/media/movie.mkv"}
	if pin := PinDVPinV3(DVPinV3{}, file, 0); pin.Profile != 0 {
		t.Fatalf("a zero profile established a pin: %#v", pin)
	}
	pin := PinDVPinV3(DVPinV3{}, file, 8)
	if got := PinDVPinV3(pin, file, 0); got.Profile != 8 {
		t.Fatalf("a zero re-read cleared the pin: %#v", got)
	}
}

// ApplyDVPinToFileV3 overwrites the loaded row's profile only for the pinned
// file identity; another file's fresh read is left alone.
func TestApplyDVPinToFileV3ScopesToThePinnedFile(t *testing.T) {
	pinned := &models.MediaFile{ID: 42, FilePath: "/media/movie.mkv", VideoTracks: []models.VideoTrack{{DVProfile: 7}}}
	ApplyDVPinToFileV3(pinned, DVPinV3{FileID: 42, Source: "/media/movie.mkv", Profile: 8})
	if pinned.VideoTracks[0].DVProfile != 8 {
		t.Fatalf("pinned profile = %d, want 8", pinned.VideoTracks[0].DVProfile)
	}

	other := &models.MediaFile{ID: 84, FilePath: "/media/other.mkv", VideoTracks: []models.VideoTrack{{DVProfile: 7}}}
	ApplyDVPinToFileV3(other, DVPinV3{FileID: 42, Source: "/media/movie.mkv", Profile: 8})
	if other.VideoTracks[0].DVProfile != 7 {
		t.Fatalf("a different file was overwritten by another file's pin: %d", other.VideoTracks[0].DVProfile)
	}

	// Same row id, rotated source: the pin must not apply.
	rotated := &models.MediaFile{ID: 42, FilePath: "virtual://movie/tt?result=B", VideoTracks: []models.VideoTrack{{DVProfile: 7}}}
	ApplyDVPinToFileV3(rotated, DVPinV3{FileID: 42, Source: "virtual://movie/tt?result=A", Profile: 8})
	if rotated.VideoTracks[0].DVProfile != 7 {
		t.Fatalf("a rotated release inherited the previous pin: %d", rotated.VideoTracks[0].DVProfile)
	}
}

// The session stream state must preserve first-verified-wins: a drifted profile
// in a later route-set snapshot for the same file cannot overwrite the pin, but
// a snapshot naming a different identity owns the pin outright.
func TestUpdateStreamStateKeepsDVPinAcrossDrift(t *testing.T) {
	sm := NewSessionManager(0, 0)
	session, err := sm.StartSession(1, "profile-1", 42, PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := sm.UpdateStreamState(session.ID, SessionStreamState{
		DVProfile: 8, DVProfilePin: DVPinV3{FileID: 42, Source: "/media/movie.mkv", Profile: 8}, TranscodeRouteSet: true,
	}); err != nil {
		t.Fatalf("UpdateStreamState(set): %v", err)
	}
	// A replan re-reads the file and reports a drifted 7 under the same id.
	if err := sm.UpdateStreamState(session.ID, SessionStreamState{
		DVProfile: 7, DVProfilePin: DVPinV3{FileID: 42, Source: "/media/movie.mkv", Profile: 7}, TranscodeRouteSet: true,
	}); err != nil {
		t.Fatalf("UpdateStreamState(drift): %v", err)
	}
	got, err := sm.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.DVProfile != 8 || got.DVProfilePin != (DVPinV3{FileID: 42, Source: "/media/movie.mkv", Profile: 8}) {
		t.Fatalf("session DV = profile %d pin %#v, want profile 8 pinned", got.DVProfile, got.DVProfilePin)
	}

	// A binding move to a different file owns the pin, even a zero-profile one.
	if err := sm.UpdateStreamState(session.ID, SessionStreamState{
		DVProfile: 5, DVProfilePin: DVPinV3{FileID: 84, Source: "/media/other.mkv", Profile: 5}, TranscodeRouteSet: true,
	}); err != nil {
		t.Fatalf("UpdateStreamState(move): %v", err)
	}
	got, err = sm.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.DVProfile != 5 || got.DVProfilePin != (DVPinV3{FileID: 84, Source: "/media/other.mkv", Profile: 5}) {
		t.Fatalf("moved session DV = profile %d pin %#v, want profile 5", got.DVProfile, got.DVProfilePin)
	}
}
