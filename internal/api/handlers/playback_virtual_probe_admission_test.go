package handlers

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

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

func TestPreparationRedisBootstrap(t *testing.T) {
	for _, configured := range []bool{false, true} {
		lease := NewVirtualProbeLease(nil, configured)
		release, ok := lease.acquire(context.Background(), "bootstrap")
		if ok == configured {
			t.Fatalf("configured=%v admitted=%v", configured, ok)
		}
		if ok {
			release()
		}
	}
}

func TestPreparationRedisUnreachable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	client := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1})
	defer func() { _ = client.Close() }()
	if _, ok := NewVirtualProbeLease(client, true).acquire(context.Background(), "unreachable"); ok {
		t.Fatal("unreachable Redis admitted probe")
	}
}

func TestPreparationRedisContentionAndExpiry(t *testing.T) {
	endpoint := os.Getenv("SILO_TEST_REDIS_URL")
	if endpoint == "" {
		t.Skip("SILO_TEST_REDIS_URL required for running Redis lease coverage")
	}
	opts, err := redis.ParseURL(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	client := redis.NewClient(opts)
	defer func() { _ = client.Close() }()
	ctx := context.Background()
	provider := fmt.Sprintf("lease-test-%d", time.Now().UnixNano())
	lease := NewVirtualProbeLease(client, true)
	release, ok := lease.acquire(ctx, provider)
	if !ok {
		t.Fatal("first lease refused")
	}
	defer release()
	if _, ok := lease.acquire(ctx, provider); ok {
		t.Fatal("contending lease admitted")
	}
	key := fmt.Sprintf("vio:preplay:item:%x", sha256.Sum256([]byte(provider)))
	if err := client.PExpire(ctx, key, -time.Millisecond).Err(); err != nil {
		t.Fatal(err)
	}
	secondRelease, ok := lease.acquire(ctx, provider)
	if !ok {
		t.Fatal("expired lease blocked new owner")
	}
	defer secondRelease()
	release()
	if _, ok := lease.acquire(ctx, provider); ok {
		t.Fatal("old owner released new lease")
	}
	secondRelease()
	thirdRelease, ok := lease.acquire(ctx, provider)
	if !ok {
		t.Fatal("released lease blocked new owner")
	}
	thirdRelease()
}

func TestSlowPreparationDoesNotBlockForegroundProbe(t *testing.T) {
	row := probeChainRow(904, "virtual://movie/probe-chain?result=slow", nil)
	entered, unblock := make(chan struct{}), make(chan struct{})
	h, target, _ := probeChainHandler(row)
	target.releaseRow()
	h.VirtualPlaybackSourceProber = func(ctx context.Context, url string, file *models.MediaFile) (*models.MediaFile, error) {
		// Preparation probes the listing's concrete provider URL, never the
		// catalog's virtual:// row path: the prober cannot open a virtual URI.
		if url == probeChainProviderURL(row.FilePath) {
			close(entered)
			select {
			case <-unblock:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return file, nil
	}
	defer func() { close(unblock); h.probeChainWG.Wait() }()
	h.ScheduleDefaultFileProbe(probeChainTestContent, probeChainSourceRow(), probeChainStreams(row), catalog.AccessFilter{})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("preparation did not enter provider")
	}
	done := make(chan error, 1)
	go func() {
		_, err := h.probeVirtualSource(context.Background(), "foreground", &models.MediaFile{}, nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("foreground probe waited for slow preparation")
	}
}
