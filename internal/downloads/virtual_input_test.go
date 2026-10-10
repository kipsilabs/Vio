package downloads

import (
	"context"
	"errors"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

type stubVirtualResolver struct {
	inputPath string
	cleanup   func()
	err       error
	calls     int
}

func (s *stubVirtualResolver) ResolveVirtualDownloadInput(_ context.Context, _ *models.MediaFile, _ int, _ string) (string, func(), error) {
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

func TestCancelPrepareIgnoresEmptyArtifact(t *testing.T) {
	// No repository is touched for an empty id, so this is safe on a zero-value
	// manager with no database.
	m := &ArtifactManager{}
	m.CancelPrepare(context.Background(), "")
	m.SetVirtualInputResolver(nil)
}
