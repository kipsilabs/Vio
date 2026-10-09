package downloads

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/scanner"
)

// missingFileRepo reports every lookup the way the real repository does for a
// row that does not exist.
type missingFileRepo struct{ err error }

func (r missingFileRepo) GetByID(context.Context, int) (*models.MediaFile, error) {
	return nil, r.err
}

func (r missingFileRepo) GetByContentID(context.Context, string) ([]*models.MediaFile, error) {
	return nil, r.err
}

func (r missingFileRepo) GetByEpisodeID(context.Context, string) ([]*models.MediaFile, error) {
	return nil, r.err
}

func (r missingFileRepo) ListByEpisodeIDs(context.Context, []string) (map[string][]*models.MediaFile, error) {
	return nil, r.err
}

type stubUserRepo struct{ user *models.User }

func (r stubUserRepo) GetByID(context.Context, int) (*models.User, error) { return r.user, nil }

func serviceWithFileRepo(repo FileResolver) *Service {
	allowed := true
	return &Service{
		fileRepo:    repo,
		userRepo:    stubUserRepo{user: &models.User{ID: 7, DownloadAllowed: &allowed}},
		cfg:         config.DownloadConfig{Enabled: true},
		cfgLoadedAt: time.Now(),
	}
}

func TestResolveDirectFileTranslatesMissingFileToNotFound(t *testing.T) {
	svc := serviceWithFileRepo(missingFileRepo{err: scanner.ErrFileNotFound})

	_, err := svc.ResolveDirectFile(context.Background(), 7, 4242, "", catalog.AccessFilter{})
	if !errors.Is(err, catalog.ErrItemNotFound) {
		t.Fatalf("err = %v, want catalog.ErrItemNotFound", err)
	}
}

func TestResolveFileTranslatesMissingFileToNotFound(t *testing.T) {
	svc := serviceWithFileRepo(missingFileRepo{err: scanner.ErrFileNotFound})

	_, err := svc.resolveFile(context.Background(), 7, CreateRequest{FileID: 4242}, catalog.AccessFilter{})
	if !errors.Is(err, catalog.ErrItemNotFound) {
		t.Fatalf("err = %v, want catalog.ErrItemNotFound", err)
	}
}

// fileRepoWithFile returns one file for every id, so a resolve reaches the
// access checks that sit behind the file lookup.
type fileRepoWithFile struct{ file *models.MediaFile }

func (r fileRepoWithFile) GetByID(context.Context, int) (*models.MediaFile, error) {
	return r.file, nil
}

func (r fileRepoWithFile) GetByContentID(context.Context, string) ([]*models.MediaFile, error) {
	return nil, nil
}

func (r fileRepoWithFile) GetByEpisodeID(context.Context, string) ([]*models.MediaFile, error) {
	return nil, nil
}

func (r fileRepoWithFile) ListByEpisodeIDs(context.Context, []string) (map[string][]*models.MediaFile, error) {
	return nil, nil
}

// denyItemAccess refuses every content id the way a library-scope miss does.
type denyItemAccess struct{}

func (denyItemAccess) EnsureAccessible(context.Context, string, catalog.AccessFilter) error {
	return catalog.ErrItemNotFound
}

// allowItemAccess admits every content id so the quality-ceiling predicate is
// the check that runs.
type allowItemAccess struct{}

func (allowItemAccess) EnsureAccessible(context.Context, string, catalog.AccessFilter) error {
	return nil
}

// TestResolveDirectFileDistinguishesRefusalReasons pins the three distinct
// direct-download refusal signals: a stale/unknown file_id is ErrFileUnavailable,
// an access-filter miss (library scope or quality ceiling) is
// ErrFileAccessDenied, and a non-original format is ErrFormatUnavailable. Each
// still satisfies the historical catalog.ErrItemNotFound for the two 404
// branches so existing not-found handling is unchanged. Authorization runs
// before virtual classification, so a denied virtual row reads as the access
// refusal rather than leaking its classification.
func TestResolveDirectFileDistinguishesRefusalReasons(t *testing.T) {
	ctx := context.Background()

	t.Run("unknown file", func(t *testing.T) {
		svc := serviceWithFileRepo(missingFileRepo{err: scanner.ErrFileNotFound})
		_, err := svc.ResolveDirectFile(ctx, 7, 4242, "", catalog.AccessFilter{})
		if !errors.Is(err, ErrFileUnavailable) {
			t.Fatalf("err = %v, want ErrFileUnavailable", err)
		}
		if !errors.Is(err, catalog.ErrItemNotFound) {
			t.Fatalf("err = %v, want it to keep wrapping catalog.ErrItemNotFound", err)
		}
		if errors.Is(err, ErrFileAccessDenied) {
			t.Fatalf("err = %v, must not be ErrFileAccessDenied", err)
		}
	})

	t.Run("deleted row", func(t *testing.T) {
		svc := serviceWithFileRepo(missingFileRepo{err: nil})
		_, err := svc.ResolveDirectFile(ctx, 7, 4242, "", catalog.AccessFilter{})
		if !errors.Is(err, ErrFileUnavailable) {
			t.Fatalf("err = %v, want ErrFileUnavailable for a nil row", err)
		}
	})

	t.Run("missing since", func(t *testing.T) {
		missing := time.Now()
		svc := serviceWithFileRepo(fileRepoWithFile{file: &models.MediaFile{ID: 42, ContentID: "movie-1", MissingSince: &missing}})
		_, err := svc.ResolveDirectFile(ctx, 7, 42, "", catalog.AccessFilter{})
		if !errors.Is(err, ErrFileUnavailable) {
			t.Fatalf("err = %v, want ErrFileUnavailable for a missing-since row", err)
		}
	})

	t.Run("access filtered", func(t *testing.T) {
		svc := serviceWithFileRepo(fileRepoWithFile{file: &models.MediaFile{ID: 42, ContentID: "movie-1"}})
		svc.itemAccess = denyItemAccess{}
		_, err := svc.ResolveDirectFile(ctx, 7, 42, "", catalog.AccessFilter{})
		if !errors.Is(err, ErrFileAccessDenied) {
			t.Fatalf("err = %v, want ErrFileAccessDenied", err)
		}
		if !errors.Is(err, catalog.ErrItemNotFound) {
			t.Fatalf("err = %v, want it to keep wrapping catalog.ErrItemNotFound", err)
		}
		if errors.Is(err, ErrFileUnavailable) {
			t.Fatalf("err = %v, must not be ErrFileUnavailable", err)
		}
	})

	t.Run("quality ceiling", func(t *testing.T) {
		svc := serviceWithFileRepo(fileRepoWithFile{file: &models.MediaFile{ID: 42, ContentID: "movie-1", Resolution: "2160p", MediaFolderID: 3}})
		svc.itemAccess = allowItemAccess{}
		_, err := svc.ResolveDirectFile(ctx, 7, 42, "", catalog.AccessFilter{AllowedLibraryIDs: []int{9}})
		if !errors.Is(err, ErrFileAccessDenied) {
			t.Fatalf("err = %v, want ErrFileAccessDenied for a library-scope miss", err)
		}
	})

	t.Run("non-original format", func(t *testing.T) {
		svc := serviceWithFileRepo(missingFileRepo{err: scanner.ErrFileNotFound})
		_, err := svc.ResolveDirectFile(ctx, 7, 42, "transcode", catalog.AccessFilter{})
		if !errors.Is(err, ErrFormatUnavailable) {
			t.Fatalf("err = %v, want ErrFormatUnavailable", err)
		}
		if errors.Is(err, ErrFileUnavailable) || errors.Is(err, ErrFileAccessDenied) || errors.Is(err, catalog.ErrItemNotFound) {
			t.Fatalf("err = %v, format refusal must be distinct from the file refusals", err)
		}
	})

	t.Run("authorized virtual placeholder", func(t *testing.T) {
		svc := serviceWithFileRepo(fileRepoWithFile{file: &models.MediaFile{ID: 42, ContentID: "movie-1", FilePath: "virtual://movie/tt1?result=cand", Container: "virtual"}})
		svc.itemAccess = allowItemAccess{}
		_, err := svc.ResolveDirectFile(ctx, 7, 42, "", catalog.AccessFilter{})
		if !errors.Is(err, ErrFormatUnavailable) {
			t.Fatalf("err = %v, want ErrFormatUnavailable for a virtual row", err)
		}
		if !errors.Is(err, catalog.ErrItemNotFound) {
			t.Fatalf("err = %v, want it to keep wrapping catalog.ErrItemNotFound so v1 keeps its 404", err)
		}
	})

	// Authorization runs before virtual classification: a denied virtual row
	// must read as the access refusal, not as a format verdict that leaks the
	// row's classification and produces the wrong copy.
	t.Run("unauthorized virtual placeholder", func(t *testing.T) {
		svc := serviceWithFileRepo(fileRepoWithFile{file: &models.MediaFile{ID: 42, ContentID: "movie-1", FilePath: "virtual://movie/tt1?result=cand", Container: "virtual"}})
		svc.itemAccess = denyItemAccess{}
		_, err := svc.ResolveDirectFile(ctx, 7, 42, "", catalog.AccessFilter{})
		if !errors.Is(err, ErrFileAccessDenied) {
			t.Fatalf("err = %v, want ErrFileAccessDenied for an unauthorized virtual row", err)
		}
		if errors.Is(err, ErrFormatUnavailable) {
			t.Fatalf("err = %v, an access refusal must not leak the row classification", err)
		}
		if !errors.Is(err, catalog.ErrItemNotFound) {
			t.Fatalf("err = %v, want it to keep wrapping catalog.ErrItemNotFound", err)
		}
	})
}

func TestFileLookupKeepsRealFailuresOpaque(t *testing.T) {
	boom := errors.New("connection refused")
	svc := serviceWithFileRepo(missingFileRepo{err: boom})

	_, err := svc.ResolveDirectFile(context.Background(), 7, 4242, "", catalog.AccessFilter{})
	if errors.Is(err, catalog.ErrItemNotFound) || !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the underlying transport failure", err)
	}
}
