package metadata

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
)

// A virtual provider source has no local files, so the NFO reader's failure for
// it is expected rather than reportable. The classifier keys on the path the
// failure carries, which is why the provider must keep recording it.
func TestLocalMetadataUnavailableForVirtual(t *testing.T) {
	tests := []struct {
		name string
		path string
		want bool
	}{
		{name: "virtual series path", path: "virtual://series/tvdb/393159/1/2", want: true},
		{name: "upper-case scheme", path: "VIRTUAL://movie/tt0113277", want: true},
		{name: "local path", path: "/media/Film (2001)/movie.nfo", want: false},
		{name: "relative local path", path: "media/Film/movie.nfo", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := errors.Join(ErrLocalMetadataUnavailable,
				&LocalMetadataPathError{Path: test.path, Err: fs.ErrNotExist})
			if got := LocalMetadataUnavailableForVirtual(err); got != test.want {
				t.Fatalf("LocalMetadataUnavailableForVirtual(%q) = %v, want %v", test.path, got, test.want)
			}
		})
	}
}

// A local sidecar failure for a local path keeps its warning-worthy verdict,
// and unrelated failures are never reclassified as expected.
func TestLocalMetadataUnavailableForVirtualRejectsOtherFailures(t *testing.T) {
	local := errors.Join(ErrLocalMetadataUnavailable,
		&LocalMetadataPathError{Path: "/media/Film/movie.nfo", Err: fs.ErrPermission})
	if LocalMetadataUnavailableForVirtual(local) {
		t.Fatal("a permission failure on a local path must stay reportable")
	}
	if LocalMetadataUnavailableForVirtual(errors.New("provider timeout")) {
		t.Fatal("an unrelated failure is not a local-metadata failure")
	}
	if LocalMetadataUnavailableForVirtual(ErrLocalMetadataUnavailable) {
		t.Fatal("a local-metadata failure without a recorded path is ambiguous, not virtual")
	}
}

// The path-carrying error preserves both the message the provider has always
// produced and the wrapped causes callers match on.
func TestLocalMetadataPathErrorKeepsMessageAndCauses(t *testing.T) {
	err := &LocalMetadataPathError{Path: "/media/Film/movie.nfo", Err: fs.ErrPermission}
	if got, want := err.Error(), "/media/Film/movie.nfo: permission denied"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatal("wrapped cause is not reachable with errors.Is")
	}
	path, ok := LocalMetadataFailurePath(err)
	if !ok || path != "/media/Film/movie.nfo" {
		t.Fatalf("LocalMetadataFailurePath = %q, %v", path, ok)
	}
	if strings.TrimSpace(path) == "" {
		t.Fatal("path must not be empty when it is reported")
	}
}
