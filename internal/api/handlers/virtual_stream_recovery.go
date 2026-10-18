package handlers

import (
	"sync"
	"time"
)

// A virtual direct-play request has a tightly bounded recovery: one initial
// resolve plus at most one same-release rotation. When both fail without
// delivering bytes, repeating that recovery on every client segment retry only
// multiplies provider work — the candidate is already known bad for this
// session until it replans. virtualStreamRecoveryTTL is the short window in
// which a matching request fails fast instead of replaying the resolve and
// rotation; a successful serve or a new binding clears the entry sooner.
const virtualStreamRecoveryTTL = 12 * time.Second

// virtualStreamRecoveryMaxEntries bounds the cache regardless of how many
// sessions and candidates fail. Entries also expire on their own, so the cap
// is a backstop rather than the primary cleanup.
const virtualStreamRecoveryMaxEntries = 512

// virtualStreamRecoveryKey names one exhausted session-bound candidate.
// Candidate identity is part of the key so a session that replans to another
// candidate resolves normally instead of inheriting the failed release's
// terminal.
type virtualStreamRecoveryKey struct {
	sessionID   string
	candidateID string
}

// virtualStreamRecoveryCache records the short-lived terminal described above.
// It is race-safe and bounded by TTL and entry count. The zero value is ready
// to use.
type virtualStreamRecoveryCache struct {
	mu      sync.Mutex
	entries map[virtualStreamRecoveryKey]time.Time
	// now is the clock used for TTL checks and eviction ordering; nil uses
	// time.Now. Tests inject a fake to exercise expiry without sleeping.
	now func() time.Time
}

func (c *virtualStreamRecoveryCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// blocked reports whether this session and candidate still carry an exhausted
// recovery terminal. Expired entries are removed on observation.
func (c *virtualStreamRecoveryCache) blocked(sessionID, candidateID string) bool {
	if sessionID == "" || candidateID == "" {
		return false
	}
	key := virtualStreamRecoveryKey{sessionID: sessionID, candidateID: candidateID}
	now := c.clock()

	c.mu.Lock()
	defer c.mu.Unlock()
	expiry, ok := c.entries[key]
	if !ok {
		return false
	}
	if !now.Before(expiry) {
		delete(c.entries, key)
		return false
	}
	return true
}

// record starts the terminal window for one exhausted session-bound
// candidate, evicting expired or oldest entries when the cache is full.
func (c *virtualStreamRecoveryCache) record(sessionID, candidateID string) {
	if sessionID == "" || candidateID == "" {
		return
	}
	key := virtualStreamRecoveryKey{sessionID: sessionID, candidateID: candidateID}
	now := c.clock()

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[virtualStreamRecoveryKey]time.Time)
	}
	c.entries[key] = now.Add(virtualStreamRecoveryTTL)
	c.evictLocked(now)
}

// clearSession drops every terminal for a session after a serve actually
// delivered bytes. Clearing the whole session, not only the delivered
// candidate, means a rotation that recovered the session also retires a
// terminal recorded for the candidate it rotated away from.
func (c *virtualStreamRecoveryCache) clearSession(sessionID string) {
	if sessionID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.entries {
		if key.sessionID == sessionID {
			delete(c.entries, key)
		}
	}
}

func (c *virtualStreamRecoveryCache) evictLocked(now time.Time) {
	for len(c.entries) > virtualStreamRecoveryMaxEntries {
		// Drop everything already expired before spending a live entry.
		for key, expiry := range c.entries {
			if !now.Before(expiry) {
				delete(c.entries, key)
			}
		}
		if len(c.entries) <= virtualStreamRecoveryMaxEntries {
			return
		}
		// Still full: evict the entry closest to expiry so a live terminal
		// survives as long as possible.
		oldestKey := virtualStreamRecoveryKey{}
		var oldestExpiry time.Time
		found := false
		for key, expiry := range c.entries {
			if !found || expiry.Before(oldestExpiry) {
				oldestKey, oldestExpiry, found = key, expiry, true
			}
		}
		if !found {
			return
		}
		delete(c.entries, oldestKey)
	}
}
