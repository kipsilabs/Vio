package handlers

import (
	"context"
	"testing"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
)

func TestSelectedProbeTargetRejectsUnauthorizedFetchedRow(t *testing.T) {
	for _, name := range []string{"wrong_episode", "hidden_library", "mutated_row"} {
		t.Run(name, func(t *testing.T) {
			listed := probeChainRow(901, "virtual://movie/probe-chain?result=selected", nil)
			listed.MediaFolderID = 77
			fetched := *listed
			switch name {
			case "wrong_episode":
				fetched.EpisodeID = "another-episode"
			case "hidden_library":
				listed.MediaFolderID = 78
				fetched.MediaFolderID = 78
			case "mutated_row":
				fetched.MediaFolderID = 78
			}
			reader := &probeChainVersionReader{files: []*models.MediaFile{listed}, rows: map[int]*models.MediaFile{listed.ID: &fetched}}
			target := NewVirtualDefaultProbeTarget(reader, reader)
			filter := catalog.AccessFilter{SelectedFileID: listed.ID, AllowedLibraryIDs: []int{77}}
			row, err := target.DefaultVirtualProbeTarget(context.Background(), probeChainTestContent, filter)
			if err != nil || row != nil {
				t.Fatalf("target = %v, error = %v; want no target", row, err)
			}
			prober := &recordingProber{}
			h := &PlaybackHandler{ServiceContext: context.Background(), DefaultProbeTarget: target, VirtualPlaybackSourceProber: prober.probe}
			h.ScheduleDefaultFileProbe(probeChainTestContent, probeChainSourceRow(), probeChainStreams(listed), filter)
			h.probeChainWG.Wait()
			if prober.count() != 0 {
				t.Fatal("unauthorized row was probed")
			}
		})
	}
}

func TestProbeGroupsFirstCompletesBeforeSelectedGroup(t *testing.T) {
	row := probeChainRow(902, "virtual://movie/probe-chain?result=selected", nil)
	h, target, prober := probeChainHandler(row)
	target.releaseRow()
	first := *probeChainSourceRow()
	first.FilePath = "virtual://movie/other-group"
	filter := catalog.AccessFilter{SelectedFileID: row.ID}
	h.ScheduleDefaultFileProbe(probeChainTestContent, &first, []VirtualPlaybackStream{{URI: first.FilePath + "?result=other", OwnerInstallationID: 5}}, filter)
	h.probeChainWG.Wait()
	if prober.count() != 0 {
		t.Fatal("first group probed an absent target")
	}
	h.ScheduleDefaultFileProbe(probeChainTestContent, probeChainSourceRow(), probeChainStreams(row), filter)
	h.probeChainWG.Wait()
	if prober.count() != 1 || prober.last() != row.FilePath {
		t.Fatal("second group did not probe exactly the selected target")
	}
	reads := target.callCount()
	h.ScheduleDefaultFileProbe(probeChainTestContent, probeChainSourceRow(), probeChainStreams(row), catalog.AccessFilter{})
	h.probeChainWG.Wait()
	if prober.count() != 1 || target.callCount() != reads {
		t.Fatal("source cooldown did not suppress both read and probe for default intent")
	}
	h.ScheduleDefaultFileProbe(probeChainTestContent, &first, []VirtualPlaybackStream{{URI: first.FilePath + "?result=other", OwnerInstallationID: 5}}, filter)
	h.probeChainWG.Wait()
	if target.callCount() != reads+1 {
		t.Fatal("unrelated group was blocked by another group's cooldown")
	}
}
