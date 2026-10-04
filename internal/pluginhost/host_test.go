package pluginhost_test

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/Silo-Server/silo-server/internal/pluginhost"
)

func TestConfig_AcceptsEventPublisherAndLibraryLister(t *testing.T) {
	// Compile-time assertion that pluginhost.Config has these fields.
	_ = pluginhost.Config{
		EventPublisher: nil,
		LibraryLister:  nil,
	}
}

func buildFixturePlugin(t *testing.T) (string, *pluginv1.PluginManifest) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "exitingplugin")
	build := exec.Command("go", "build", "-o", bin, "./testdata/exitingplugin")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build exitingplugin: %v\n%s", err, out)
	}
	raw, err := exec.Command(bin, "manifest").Output()
	if err != nil {
		t.Fatalf("read fixture manifest: %v", err)
	}
	manifest := &pluginv1.PluginManifest{}
	if err := protojson.Unmarshal(raw, manifest); err != nil {
		t.Fatalf("decode fixture manifest: %v", err)
	}
	return bin, manifest
}

func TestHostStart_StrictManifestMatchingSucceeds(t *testing.T) {
	bin, manifest := buildFixturePlugin(t)
	host := pluginhost.NewHost(pluginhost.Config{Logger: hclog.NewNullLogger()})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := host.Start(ctx, pluginhost.StartRequest{
		InstallationID: 1,
		BinaryPath:     bin,
		Manifest:       manifest,
	})
	if err != nil {
		t.Fatalf("host.Start with matching manifest: %v", err)
	}
	defer func() { _ = host.Stop(1) }()

	if client == nil {
		t.Fatal("expected non-nil client on successful start")
	}
	if _, err := host.Client(1); err != nil {
		t.Fatalf("host.Client(1): %v", err)
	}
}

func TestHostStart_StrictManifestMismatchRefused(t *testing.T) {
	bin, manifest := buildFixturePlugin(t)
	host := pluginhost.NewHost(pluginhost.Config{Logger: hclog.NewNullLogger()})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Tamper the manifest: strict proto.Equal must reject the launch.
	tampered := proto.Clone(manifest).(*pluginv1.PluginManifest)
	tampered.Checksum = "mismatched-checksum"

	_, err := host.Start(ctx, pluginhost.StartRequest{
		InstallationID: 2,
		BinaryPath:     bin,
		Manifest:       tampered,
	})
	if err == nil {
		_ = host.Stop(2)
		t.Fatal("host.Start with tampered manifest must fail")
	}
	if !strings.Contains(err.Error(), "plugin runtime manifest does not match installed manifest") {
		t.Fatalf("error = %q, want mismatch error", err)
	}

	// Verify the process was cleaned up and no client was retained.
	if _, err := host.Client(2); !errors.Is(err, pluginhost.ErrClientNotFound) {
		t.Fatalf("host.Client(2) = %v, want ErrClientNotFound", err)
	}
}
