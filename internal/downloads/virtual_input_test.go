package downloads

import (
	"context"
	"errors"
	"testing"

	"github.com/Silo-Server/silo-server/internal/downloadprepare"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/tonemap"
)

type stubVirtualResolver struct {
	inputPath string
	cleanup   func()
	err       error
	calls     int
}

func (s *stubVirtualResolver) ResolveVirtualDownloadInput(_ context.Context, _ *models.MediaFile) (string, func(), error) {
	s.calls++
	return s.inputPath, s.cleanup, s.err
}

func TestApplyVirtualInputRewritesPreparerInput(t *testing.T) {
	virtual := &models.MediaFile{ID: 7, Container: "virtual", FilePath: "virtual://movie/tt1?result=a"}
	released := false
	resolver := &stubVirtualResolver{
		inputPath: "http://127.0.0.1:4000/source/token/movie.mp4",
		cleanup:   func() { released = true },
	}
	m := &ArtifactManager{virtualInput: resolver}
	opts := playback.TranscodeOpts{InputPath: virtual.FilePath}

	cleanup, err := m.applyVirtualInput(context.Background(), virtual, &opts)
	if err != nil {
		t.Fatalf("applyVirtualInput error = %v", err)
	}
	if resolver.calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", resolver.calls)
	}
	if opts.InputPath != resolver.inputPath {
		t.Fatalf("InputPath = %q, want resolved relay %q", opts.InputPath, resolver.inputPath)
	}
	if opts.CanonicalInputPath != virtual.FilePath {
		t.Fatalf("CanonicalInputPath = %q, want %q", opts.CanonicalInputPath, virtual.FilePath)
	}
	if cleanup == nil {
		t.Fatal("cleanup = nil, want the relay release")
	}
	cleanup()
	if !released {
		t.Fatal("cleanup did not release the relay registration")
	}
}

func TestApplyVirtualInputNonVirtualIsNoop(t *testing.T) {
	local := &models.MediaFile{ID: 8, Container: "mkv", FilePath: "/media/movie.mkv"}
	resolver := &stubVirtualResolver{inputPath: "http://relay"}
	m := &ArtifactManager{virtualInput: resolver}
	opts := playback.TranscodeOpts{InputPath: local.FilePath}

	cleanup, err := m.applyVirtualInput(context.Background(), local, &opts)
	if err != nil || cleanup != nil {
		t.Fatalf("applyVirtualInput = (cleanup set: %t, err: %v), want no cleanup and no error", cleanup != nil, err)
	}
	if resolver.calls != 0 {
		t.Fatalf("resolver calls = %d, want 0", resolver.calls)
	}
	if opts.InputPath != local.FilePath {
		t.Fatalf("InputPath = %q, want unchanged %q", opts.InputPath, local.FilePath)
	}
}

func TestApplyVirtualInputResolverFailureReleasesCleanup(t *testing.T) {
	virtual := &models.MediaFile{ID: 9, Container: "virtual", FilePath: "virtual://movie/tt2?result=b"}
	released := false
	resolver := &stubVirtualResolver{
		err:     errors.New("provider unavailable"),
		cleanup: func() { released = true },
	}
	m := &ArtifactManager{virtualInput: resolver}
	opts := playback.TranscodeOpts{InputPath: virtual.FilePath}

	if _, err := m.applyVirtualInput(context.Background(), virtual, &opts); err == nil {
		t.Fatal("applyVirtualInput error = nil, want resolver failure")
	}
	if !released {
		t.Fatal("failed resolve did not release the partial relay registration")
	}
}

func TestApplyVirtualInputEmptyResolveRefused(t *testing.T) {
	virtual := &models.MediaFile{ID: 10, Container: "virtual", FilePath: "virtual://movie/tt3?result=c"}
	m := &ArtifactManager{virtualInput: &stubVirtualResolver{}}
	opts := playback.TranscodeOpts{InputPath: virtual.FilePath}

	if _, err := m.applyVirtualInput(context.Background(), virtual, &opts); err == nil {
		t.Fatal("applyVirtualInput error = nil, want empty resolve refused")
	}
}

func TestApplyVirtualInputWithoutResolverDefers(t *testing.T) {
	virtual := &models.MediaFile{ID: 11, Container: "virtual", FilePath: "virtual://movie/tt4?result=d"}
	m := &ArtifactManager{}
	opts := playback.TranscodeOpts{InputPath: virtual.FilePath}

	_, err := m.applyVirtualInput(context.Background(), virtual, &opts)
	if !errors.Is(err, errVirtualInputResolverPending) {
		t.Fatalf("applyVirtualInput error = %v, want errVirtualInputResolverPending", err)
	}
}

// The frozen execution fingerprint of a virtual artifact is derived from the
// durable canonical source identity, so a per-attempt relay URL rotation never
// invalidates a healthy artifact while a real source change still does. This is
// the invariant the hourly integrity probe and retries rely on.
func TestArtifactExecutionFingerprintUsesCanonicalPathForVirtual(t *testing.T) {
	canonical := "virtual://movie/tt1?result=a"
	frozen := playback.TranscodeOpts{
		InputPath: canonical, ToneMapPolicy: tonemap.PolicySoftwareOnly,
		ToneMapMode: tonemap.ModeSoftware, ToneMapSourceKind: tonemap.SourcePQ,
		ToneMapRecipeVersion:  playback.TransformationHDRToSDRToneMapRecipeVersionV3,
		ToneMapSourceRevision: tonemap.SourceRevision{MediaFileID: 42, FileSize: 100},
		TargetCodecVideo:      "h264", TargetCodecAudio: "aac",
	}
	a := &Artifact{ID: "art-fingerprint", ToneMapMode: tonemap.ModeSoftware}
	a.ParamsHash = downloadprepare.NewRequest(a.ID, frozen).ExecutionFingerprint()
	if a.ParamsHash == "" {
		t.Fatal("frozen virtual fingerprint is empty")
	}

	// The resolver rewrites InputPath to a rotating relay URL but preserves the
	// canonical identity; the frozen fingerprint must still match.
	resolved := frozen
	resolved.InputPath = "http://relay/attempt-2/token"
	resolved.CanonicalInputPath = canonical
	if !artifactExecutionFingerprintMatches(a, resolved) {
		t.Fatal("relay rotation invalidated the frozen virtual fingerprint")
	}

	// A genuine source change must not match.
	changed := resolved
	changed.CanonicalInputPath = "virtual://movie/tt1?result=other-release"
	if artifactExecutionFingerprintMatches(a, changed) {
		t.Fatal("a different canonical source matched the frozen fingerprint")
	}
}

func TestCancelAbandonedPrepareIgnoresEmptyArtifact(t *testing.T) {
	// No repository is touched for an empty id, so this is safe on a zero-value
	// manager with no database.
	m := &ArtifactManager{}
	m.CancelAbandonedPrepare(context.Background(), "")
	m.SetVirtualInputResolver(nil)
}
