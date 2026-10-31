package nfo

import (
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/metadata"
)

// The NFO reader keeps producing the same message and causes it always has,
// while now recording which path failed: that path is how the refresh log tells
// an expected virtual-source failure from a real local mount problem.
func TestUnavailableRecordsPathAndKeepsMessage(t *testing.T) {
	err := unavailable("virtual://series/tvdb/393159/1/2", fs.ErrNotExist)
	msg := err.Error()
	if !strings.Contains(msg, "local metadata unavailable") ||
		!strings.Contains(msg, "virtual://series/tvdb/393159/1/2") {
		t.Fatalf("message = %q, want the local-metadata prefix and the path", msg)
	}
	if !errors.Is(err, metadata.ErrLocalMetadataUnavailable) || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("causes lost: %v", err)
	}
	path, ok := metadata.LocalMetadataFailurePath(err)
	if !ok || path != "virtual://series/tvdb/393159/1/2" {
		t.Fatalf("recorded path = %q, %v", path, ok)
	}
	if !metadata.LocalMetadataUnavailableForVirtual(err) {
		t.Fatal("a virtual path failure must read as the expected virtual-source case")
	}

	local := unavailable("/media/Film (2001)/movie.nfo", fs.ErrPermission)
	if metadata.LocalMetadataUnavailableForVirtual(local) {
		t.Fatal("a local path failure must stay reportable")
	}
}
