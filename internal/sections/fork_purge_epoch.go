package sections

import "sync/atomic"

// editorialCandidateCacheEpoch is the process-global purge epoch the fork's
// resolved-list caches read. A destructive purge bumps it, so entries written
// under an older epoch are never read again. Package-global so every Fetcher
// instance (native API, recommendations, jellycompat) observes the same epoch.
// Upstream's fetcher invalidates through its own cache generation; this epoch
// remains the fork's list-level generation.
var editorialCandidateCacheEpoch atomic.Uint64
