package transcodenode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

type staticCatalogPaths struct {
	active map[string]bool
	err    error
}

func (s staticCatalogPaths) IsActivePath(_ context.Context, path string) (bool, error) {
	return s.active[path], s.err
}

func TestCatalogPathAuthorizerAllowsCataloguedRegularFilesAndSymlinks(t *testing.T) {
	mediaPath := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(mediaPath, []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside.mkv")
	if err := os.WriteFile(target, []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "linked.mkv")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks not supported on this platform: %v", err)
	}
	authorizer := NewCatalogPathAuthorizer(staticCatalogPaths{active: map[string]bool{
		mediaPath: true,
		link:      true,
	}})

	for _, path := range []string{mediaPath, link} {
		allowed, err := authorizer.Allowed(context.Background(), path)
		if err != nil || !allowed {
			t.Fatalf("approved media path %q: allowed = %v, err = %v", path, allowed, err)
		}
	}
}

func TestCatalogPathAuthorizerRejectsUnsafeOrUncataloguedInputs(t *testing.T) {
	existingUncatalogued := filepath.Join(t.TempDir(), "outside.mkv")
	if err := os.WriteFile(existingUncatalogued, []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	missingCatalogued := filepath.Join(t.TempDir(), "missing.mkv")
	directoryCatalogued := t.TempDir()
	authorizer := NewCatalogPathAuthorizer(staticCatalogPaths{active: map[string]bool{
		missingCatalogued:   true,
		directoryCatalogued: true,
	}})

	for _, candidate := range []string{
		"relative/movie.mkv",
		"http://example.test/movie.mkv",
		"virtual://movie/example",
		"http://127.0.0.1:45678/source/",
		"http://127.0.0.1:45678/source/../etc/passwd",
		"http://127.0.0.1:45678/source/./movie.mp4",
		"http://127.0.0.1:45678/source",
		"http://127.0.0.1:45678/source/token/../../etc/passwd",
		"http://127.0.0.1:45678/source/token/../../../etc/shadow",
		"http://127.0.0.1:45678/source/token/sub/../../etc/passwd",
		"http://127.0.0.1:45678/source/token/%2e%2e/passwd",
		"http://127.0.0.1:45678/source/token/..",
		"http://127.0.0.1:45678/source/token/.",
		"http:///source/token/movie.mp4",
		"ftp://127.0.0.1:45678/source/token/movie.mp4",
		"file:" + existingUncatalogued,
		"concat:" + existingUncatalogued + "|" + existingUncatalogued,
		existingUncatalogued,
		missingCatalogued,
		directoryCatalogued,
	} {
		allowed, err := authorizer.Allowed(context.Background(), candidate)
		if err != nil {
			t.Fatalf("candidate %q: %v", candidate, err)
		}
		if allowed {
			t.Errorf("candidate %q was allowed", candidate)
		}
	}
}

func TestCatalogPathAuthorizerAllowsStreamURLs(t *testing.T) {
	authorizer := NewCatalogPathAuthorizer(staticCatalogPaths{})
	for _, candidate := range []string{
		"http://127.0.0.1:45678/source/token",
		"http://127.0.0.1:45678/source/token/movie.mp4",
		"https://example.com/source/token/stream.mkv",
	} {
		allowed, err := authorizer.Allowed(context.Background(), candidate)
		if err != nil {
			t.Fatalf("candidate %q: %v", candidate, err)
		}
		if !allowed {
			t.Errorf("candidate %q was not allowed", candidate)
		}
	}
}

func TestCatalogPathAuthorizerPropagatesRepositoryErrors(t *testing.T) {
	wantErr := errors.New("database unavailable")
	authorizer := NewCatalogPathAuthorizer(staticCatalogPaths{err: wantErr})
	allowed, err := authorizer.Allowed(context.Background(), "/media/movie.mkv")
	if allowed || !errors.Is(err, wantErr) {
		t.Fatalf("allowed = %v, err = %v", allowed, err)
	}
}

func TestPathWithinRootRejectsSymlinkedOutputParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	linkedParent := filepath.Join(root, "linked")
	if err := os.Symlink(outside, linkedParent); err != nil {
		t.Fatal(err)
	}
	if pathWithinRoot(root, filepath.Join(linkedParent, "artifact.mp4")) {
		t.Fatal("output under symlinked escape was accepted")
	}
	if !pathWithinRoot(root, filepath.Join(root, "artifact.mp4")) {
		t.Fatal("direct child output was rejected")
	}
}
