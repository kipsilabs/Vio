package handlers

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
)

func TestProviderPreparationStartBounds(t *testing.T) {
	var a defaultProbeChainAdmission
	for i := 0; i < 4; i++ {
		if !a.allowProviderStart("one") {
			t.Fatal("early refusal")
		}
	}
	if a.allowProviderStart("one") {
		t.Fatal("fifth start admitted")
	}
	if !a.allowProviderStart("two") {
		t.Fatal("other provider blocked")
	}
	for i := 0; i < virtualProbeCooldownCapacity; i++ {
		a.allowProviderStart(strconv.Itoa(i))
	}
	if len(a.providerStarts) != virtualProbeCooldownCapacity {
		t.Fatal("unbounded provider map")
	}
	if a.allowProviderStart("overflow") {
		t.Fatal("overflow admitted")
	}
	a.providerStarts["one"] = []time.Time{time.Now().Add(-time.Minute)}
	if !a.allowProviderStart("one") {
		t.Fatal("expired allowance not reclaimed")
	}
}

func TestPreparationLeaseConfiguration(t *testing.T) {
	var absent *VirtualProbeLease
	release, ok := absent.acquire(context.Background(), "one")
	if !ok {
		t.Fatal("unconfigured Redis must allow process-local mode")
	}
	release()
	if _, ok := (&VirtualProbeLease{}).acquire(context.Background(), "one"); ok {
		t.Fatal("configured unavailable Redis admitted work")
	}
}

func TestSelectedProbeSubmissionRevalidatesPermissions(t *testing.T) {
	row := probeChainRow(903, "virtual://movie/probe-chain?result=selected", nil)
	row.MediaFolderID = 77
	reader := &probeChainVersionReader{files: []*models.MediaFile{row}, rows: map[int]*models.MediaFile{row.ID: row}}
	production := NewVirtualDefaultProbeTarget(reader, reader)
	reads := 0
	target := revalidatingVirtualProbeTarget{VirtualDefaultProbeTargetFunc(func(ctx context.Context, id string, filter catalog.AccessFilter) (*models.MediaFile, error) {
		reads++
		if reads == 2 {
			row.MediaFolderID = 78
		}
		return production.DefaultVirtualProbeTarget(ctx, id, filter)
	})}
	prober := &recordingProber{}
	h := &PlaybackHandler{ServiceContext: context.Background(), DefaultProbeTarget: target, VirtualPlaybackSourceProber: prober.probe}
	h.ScheduleDefaultFileProbe(probeChainTestContent, probeChainSourceRow(), probeChainStreams(row), catalog.AccessFilter{SelectedFileID: row.ID, AllowedLibraryIDs: []int{77}})
	h.probeChainWG.Wait()
	if reads != 2 {
		t.Fatalf("reads = %d; want submission revalidation", reads)
	}
	if prober.count() != 0 {
		t.Fatal("permission change did not stop submission")
	}
}
