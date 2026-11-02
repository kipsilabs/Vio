package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Silo-Server/silo-server/internal/blobstore"
)

func TestArtworkSettings(t *testing.T) {
	if got, err := NormalizeAdminSetting("artwork.storage_backend", "LOCAL"); err != nil || got != "local" {
		t.Fatalf("backend = %q, %v", got, err)
	}
	if _, err := NormalizeAdminSetting("artwork.storage_backend", "gcs"); err == nil {
		t.Fatal("invalid backend accepted")
	}
	if _, err := NormalizeAdminSetting("artwork.local_path", "relative"); err == nil {
		t.Fatal("relative path accepted")
	}
	if got, err := NormalizeAdminSetting("artwork.local_path", "/srv/silo/artwork/"); err != nil || got != "/srv/silo/artwork" {
		t.Fatalf("path = %q, %v", got, err)
	}
	if !RestartRequired("artwork.storage_backend") || !RestartRequired("artwork.local_path") {
		t.Fatal("artwork settings must require restart")
	}
}

func TestArtworkLocalPathDefaultsToVioRootForFreshInstall(t *testing.T) {
	if DefaultArtworkLocalPath != "/var/lib/vio/artwork" {
		t.Fatalf("DefaultArtworkLocalPath = %q, want the image and compose mount", DefaultArtworkLocalPath)
	}
	cfg, err := LoadFromDB(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Artwork.LocalPath; got != DefaultArtworkLocalPath {
		t.Fatalf("LoadFromDB(nil) local path = %q, want %q", got, DefaultArtworkLocalPath)
	}
	if got := EffectiveAdminSettings(nil)["artwork.local_path"]; got != DefaultArtworkLocalPath {
		t.Fatalf("admin default local path = %q, want %q", got, DefaultArtworkLocalPath)
	}
}

// TestArtworkLocalPathPreservesRecordedStorageIdentity covers the upgrade
// hazard: moving the default root would make blobstore.Open refuse the store
// an installation already recorded. Only an install with no identity row may
// adopt the new default; an existing one keeps the legacy root until a managed
// storage transition moves it.
func TestArtworkLocalPathPreservesRecordedStorageIdentity(t *testing.T) {
	if artworkIdentitySettingKey != blobstore.IdentitySettingKey {
		t.Fatalf("config key %q does not match blobstore.IdentitySettingKey %q", artworkIdentitySettingKey, blobstore.IdentitySettingKey)
	}
	legacyIdentity := blobstore.BackendLocal + "|" + legacyDefaultArtworkLocalPath
	cases := []struct {
		name   string
		stored map[string]string
		want   string
	}{
		{
			name:   "no identity is a fresh install",
			stored: map[string]string{},
			want:   DefaultArtworkLocalPath,
		},
		{
			name: "legacy local identity keeps its root",
			stored: map[string]string{
				"artwork.local_path":      "",
				artworkIdentitySettingKey: legacyIdentity,
			},
			want: legacyDefaultArtworkLocalPath,
		},
		{
			name:   "s3 identity keeps the legacy local default unused by s3",
			stored: map[string]string{artworkIdentitySettingKey: "s3|https://s3.example|artwork|"},
			want:   legacyDefaultArtworkLocalPath,
		},
		{
			name:   "custom identity is never guessed at",
			stored: map[string]string{artworkIdentitySettingKey: "local|/srv/artwork"},
			want:   legacyDefaultArtworkLocalPath,
		},
		{
			name: "explicit path wins over the default",
			stored: map[string]string{
				"artwork.local_path":      "/srv/artwork",
				artworkIdentitySettingKey: legacyIdentity,
			},
			want: "/srv/artwork",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadFromDB(tc.stored)
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.Artwork.LocalPath; got != tc.want {
				t.Fatalf("runtime local path = %q, want %q", got, tc.want)
			}
			if got := EffectiveAdminSettings(tc.stored)["artwork.local_path"]; got != tc.want {
				t.Fatalf("admin local path = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestYAMLImportLeavesArtworkPathUnset guards the one-time import: writing the
// default there would pin a new root onto an upgraded install that already
// records an identity.
func TestYAMLImportLeavesArtworkPathUnset(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "silo.yaml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	values, err := YAMLToSettingsMap(write(t, "server:\n  listen: \":8080\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := values["artwork.local_path"]; got != "" {
		t.Fatalf("imported artwork.local_path = %q, want it unset", got)
	}
	values, err = YAMLToSettingsMap(write(t, "artwork:\n  local_path: /srv/artwork\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := values["artwork.local_path"]; got != "/srv/artwork" {
		t.Fatalf("imported artwork.local_path = %q, want the configured path", got)
	}
}
