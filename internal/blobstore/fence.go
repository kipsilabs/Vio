package blobstore

import (
	"context"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/semaphore"
)

const mutationFenceWeight int64 = 1 << 30

// MutationFencer waits for active mutations and prevents new ones until the
// returned release function is called. Reads remain available during a move.
type MutationFencer interface {
	BeginMutationFence(context.Context) (func(), error)
}

// MutationFenceReporter reports whether a store currently holds an unreleased
// mutation fence. A storage transition that commits keeps its source fences
// until the process restarts, so this lets the process surface "writes are
// blocked" to health reporting instead of only failing writes silently.
type MutationFenceReporter interface {
	MutationsFenced() bool
}

type fencedStore struct {
	Store
	mutations *semaphore.Weighted
	// fenced is set while a mutation fence is held and cleared when its release
	// function runs. A fence retained after commit stays set for the process
	// lifetime.
	fenced atomic.Bool
}

func (s *fencedStore) unwrapStore() Store { return s.Store }

func (s *fencedStore) Put(ctx context.Context, key string, data []byte) error {
	if err := s.mutations.Acquire(ctx, 1); err != nil {
		return err
	}
	defer s.mutations.Release(1)
	return s.Store.Put(ctx, key, data)
}

func (s *fencedStore) PutStream(ctx context.Context, key string, r io.Reader, contentType string) error {
	if err := s.mutations.Acquire(ctx, 1); err != nil {
		return err
	}
	defer s.mutations.Release(1)
	return s.Store.PutStream(ctx, key, r, contentType)
}

func (s *fencedStore) Delete(ctx context.Context, keys []string) (int, error) {
	if err := s.mutations.Acquire(ctx, 1); err != nil {
		return 0, err
	}
	defer s.mutations.Release(1)
	return s.Store.Delete(ctx, keys)
}

func (s *fencedStore) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	if err := s.mutations.Acquire(ctx, 1); err != nil {
		return 0, err
	}
	defer s.mutations.Release(1)
	return s.Store.DeletePrefix(ctx, prefix)
}

// Matches only reads, so it passes through an active fence. Forwarding it keeps
// the image cache's immutable-object reuse check working on a fenced store.
func (s *fencedStore) Matches(ctx context.Context, key string, data []byte) (bool, error) {
	if matcher, ok := s.Store.(interface {
		Matches(context.Context, string, []byte) (bool, error)
	}); ok {
		return matcher.Matches(ctx, key, data)
	}
	return false, nil
}

func (s *fencedStore) BeginMutationFence(ctx context.Context) (func(), error) {
	if err := s.mutations.Acquire(ctx, mutationFenceWeight); err != nil {
		return nil, err
	}
	s.fenced.Store(true)
	var once sync.Once
	return func() {
		once.Do(func() {
			s.fenced.Store(false)
			s.mutations.Release(mutationFenceWeight)
		})
	}, nil
}

// MutationsFenced reports whether this store currently holds an unreleased
// mutation fence. A transition that commits retains its fences until restart,
// so a true value after a transition committed means writes are stalled.
func (s *fencedStore) MutationsFenced() bool { return s.fenced.Load() }

// FencedStores reports whether any provided store currently holds an unreleased
// mutation fence. Stores without fence support are ignored.
func FencedStores(stores ...Store) bool {
	for _, store := range stores {
		if reporter, ok := store.(MutationFenceReporter); ok && reporter.MutationsFenced() {
			return true
		}
	}
	return false
}

// PauseMutations fences each distinct store once and returns the function that
// resumes their writes. A store shared by two roles, such as a local root, has
// one fence, and a second acquire would never return. If ctx ends first, the
// fences already taken are released.
func PauseMutations(ctx context.Context, stores ...Store) (func(), error) {
	var releases []func()
	resume := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	seen := make([]Store, 0, len(stores))
	for _, store := range stores {
		fencer, ok := store.(MutationFencer)
		if !ok || slices.Contains(seen, store) {
			continue
		}
		seen = append(seen, store)
		release, err := fencer.BeginMutationFence(ctx)
		if err != nil {
			resume()
			return nil, err
		}
		releases = append(releases, release)
	}
	return resume, nil
}

type fencedDirectStore struct {
	*fencedStore
	direct DirectURLer
}

func (s *fencedDirectStore) DirectURL(ctx context.Context, key string, ttl, window time.Duration) (string, time.Time, error) {
	return s.direct.DirectURL(ctx, key, ttl, window)
}

// WithMutationFence wraps a store once and preserves optional direct delivery.
func WithMutationFence(store Store) Store {
	if store == nil {
		return nil
	}
	if _, ok := store.(MutationFencer); ok {
		return store
	}
	base := &fencedStore{Store: store, mutations: semaphore.NewWeighted(mutationFenceWeight)}
	if direct, ok := store.(DirectURLer); ok {
		return &fencedDirectStore{fencedStore: base, direct: direct}
	}
	return base
}
