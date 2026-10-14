package virtuallibrary_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

// trustWindowProvider serves one live release so an absent pin can be
// distinguished from a dead provider.
func trustWindowProvider(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/manifest.json" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"org.stremio.test","resources":["stream"],"types":["movie","series"]}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"streams": [
			{"name": "1080p", "title": "Live sibling", "url": "http://192.168.1.100:8080/stream1.mkv",
			 "behaviorHints": {"videoHash": "hash-live"}}
		]}`))
	}))
	t.Cleanup(server.Close)
	return server
}

func trustWindowService(t *testing.T, manifestURL string) *virtuallibrary.Service {
	t.Helper()
	svc := virtuallibrary.New(virtuallibrary.Config{
		Enabled:             true,
		ManifestURL:         manifestURL,
		AllowInsecureHTTP:   true,
		AllowPrivateStreams: true,
	}, nil, nil)
	if svc == nil {
		t.Fatal("expected non-nil service")
	}
	return svc
}

// TestResolveDetailedTrustWindowRefusesToSubstitute proves that a session-bound
// pin that the provider no longer lists is refused with ErrPersistedCandidateTrusted
// when the caller declared the persisted same-identity row as trusted. It is
// deliberately not ErrSessionBoundCandidateAbsent, so the serve layer and the
// rehydration retry do not rotate to a sibling under the viewer's selection.
func TestResolveDetailedTrustWindowRefusesToSubstitute(t *testing.T) {
	server := trustWindowProvider(t)
	svc := trustWindowService(t, server.URL+"/manifest.json")

	ctx := virtuallibrary.WithPersistedCandidateIdentity(context.Background(), virtuallibrary.PersistedCandidateIdentity{
		VideoHash:   "hash-persisted",
		ReleaseName: "Movie.2024.1080p",
	})
	ctx = virtuallibrary.WithPersistedCandidateTrust(ctx, true)

	_, err := svc.ResolveDetailed(ctx, "virtual://movie/tt100?result=absentrelease", false, nil, "", true, false)
	if !errors.Is(err, virtuallibrary.ErrPersistedCandidateTrusted) {
		t.Fatalf("err = %v, want ErrPersistedCandidateTrusted", err)
	}
	if errors.Is(err, virtuallibrary.ErrSessionBoundCandidateAbsent) {
		t.Fatal("trusted candidate must not carry the rotation sentinel")
	}
}

// TestResolveDetailedTrustedRowWithoutIdentityStillRefusesSubstitution proves
// the trusted flag is honored even without a durable identity: an identity-less
// row with transport evidence (which is what the handler requires before it sets
// the flag) is still the viewer's selection, so the resolver must signal the
// trust sentinel, not the rotation sentinel, and let the handler re-serve its
// stored URL. Trust alone never substitutes a sibling.
func TestResolveDetailedTrustedRowWithoutIdentityStillRefusesSubstitution(t *testing.T) {
	server := trustWindowProvider(t)
	svc := trustWindowService(t, server.URL+"/manifest.json")

	ctx := virtuallibrary.WithPersistedCandidateTrust(context.Background(), true)
	_, err := svc.ResolveDetailed(ctx, "virtual://movie/tt100?result=absentrelease", false, nil, "", true, false)
	if !errors.Is(err, virtuallibrary.ErrPersistedCandidateTrusted) {
		t.Fatalf("err = %v, want ErrPersistedCandidateTrusted", err)
	}
	if errors.Is(err, virtuallibrary.ErrSessionBoundCandidateAbsent) {
		t.Fatal("a trusted identity-less row must not carry the rotation sentinel")
	}
}

// TestResolveDetailedUntrustedIdentitylessStillRotates proves the boundary of
// the relaxation: an identity-less row WITHOUT the trusted flag keeps today's
// rotation sentinel, so callers outside the window still rotate as before.
func TestResolveDetailedUntrustedIdentitylessStillRotates(t *testing.T) {
	server := trustWindowProvider(t)
	svc := trustWindowService(t, server.URL+"/manifest.json")

	_, err := svc.ResolveDetailed(context.Background(), "virtual://movie/tt100?result=absentrelease", false, nil, "", true, false)
	if !errors.Is(err, virtuallibrary.ErrSessionBoundCandidateAbsent) {
		t.Fatalf("err = %v, want ErrSessionBoundCandidateAbsent", err)
	}
	if errors.Is(err, virtuallibrary.ErrPersistedCandidateTrusted) {
		t.Fatal("an untrusted identity-less row must not carry the trust sentinel")
	}
}

// TestResolveDetailedTrustWindowBeyondWindowUnchanged proves that without the
// trusted flag the pre-window refusal is unchanged: a session-bound absent pin
// still carries ErrSessionBoundCandidateAbsent so callers can rotate.
func TestResolveDetailedTrustWindowBeyondWindowUnchanged(t *testing.T) {
	server := trustWindowProvider(t)
	svc := trustWindowService(t, server.URL+"/manifest.json")

	ctx := virtuallibrary.WithPersistedCandidateIdentity(context.Background(), virtuallibrary.PersistedCandidateIdentity{
		VideoHash:   "hash-persisted",
		ReleaseName: "Movie.2024.1080p",
	})
	_, err := svc.ResolveDetailed(ctx, "virtual://movie/tt100?result=absentrelease", false, nil, "", true, false)
	if !errors.Is(err, virtuallibrary.ErrSessionBoundCandidateAbsent) {
		t.Fatalf("err = %v, want ErrSessionBoundCandidateAbsent", err)
	}
	if errors.Is(err, virtuallibrary.ErrPersistedCandidateTrusted) {
		t.Fatal("untrusted candidate must not carry the trust sentinel")
	}
}

// TestResolveDetailedTrustedFreshSelectionRefusesSubstitution proves the trust
// window extends the session-binding identity contract to a fresh explicit
// selection: a trusted (in-window) pin that the provider dropped is refused
// with the trust sentinel, not the rotation sentinel, even though sessionBound
// is false. Callers therefore do not rotate an explicitly selected release.
func TestResolveDetailedTrustedFreshSelectionRefusesSubstitution(t *testing.T) {
	server := trustWindowProvider(t)
	svc := trustWindowService(t, server.URL+"/manifest.json")

	ctx := virtuallibrary.WithPersistedCandidateIdentity(context.Background(), virtuallibrary.PersistedCandidateIdentity{
		VideoHash:   "hash-persisted",
		ReleaseName: "Movie.2024.1080p",
	})
	ctx = virtuallibrary.WithPersistedCandidateTrust(ctx, true)

	// sessionBound=false, substitution withheld: an explicit pick's shape.
	_, err := svc.ResolveDetailed(ctx, "virtual://movie/tt100?result=absentrelease", false, nil, "", false, false)
	if !errors.Is(err, virtuallibrary.ErrPersistedCandidateTrusted) {
		t.Fatalf("err = %v, want ErrPersistedCandidateTrusted", err)
	}
	if errors.Is(err, virtuallibrary.ErrSessionBoundCandidateAbsent) {
		t.Fatal("trusted fresh selection must not carry the rotation sentinel")
	}
}

// TestResolveDetailedUntrustedFreshSelectionStillSubstitutes proves the
// boundary: without trust (outside the window) a fresh selection keeps today's
// ordinary substitution, so the window relaxation does not leak.
func TestResolveDetailedUntrustedFreshSelectionStillSubstitutes(t *testing.T) {
	server := trustWindowProvider(t)
	svc := trustWindowService(t, server.URL+"/manifest.json")

	res, err := svc.ResolveDetailed(context.Background(), "virtual://movie/tt100?result=absentrelease", false, nil, "", false, false)
	if err != nil {
		t.Fatalf("ResolveDetailed: %v", err)
	}
	if res.CandidateID == "" {
		t.Fatal("expected the live sibling to be served without trust")
	}
}
