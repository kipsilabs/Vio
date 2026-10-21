package handlers

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// VirtualProbeLease isolates speculative admission from every foreground permit.
// The lease is per item: one item is speculative-probed at a time across
// replicas, while different items prepare independently. Keying it on the
// provider installation instead serialized the whole library, so opening a
// second episode while the first was probing dropped the second one's
// preparation entirely. A configured but unreachable Redis rejects optional
// work.
type VirtualProbeLease struct{ Client *redis.Client }

// NewVirtualProbeLease preserves configured-but-unavailable Redis as fail closed.
func NewVirtualProbeLease(client *redis.Client, configured bool) *VirtualProbeLease {
	if !configured && client == nil {
		return nil
	}
	return &VirtualProbeLease{Client: client}
}

var releaseVirtualProbeLease = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
 return redis.call('DEL', KEYS[1])
end
return 0
`)

func (l *VirtualProbeLease) acquire(ctx context.Context, item string) (func(), bool) {
	if l == nil {
		return func() {}, true
	}
	if l.Client == nil {
		return nil, false
	}
	key := fmt.Sprintf("vio:preplay:item:%x", sha256.Sum256([]byte(item)))
	token := uuid.NewString()
	attempt, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	// SetArgs with NX replaces the deprecated SetNX. It reports redis.Nil when
	// the key already exists, which means another holder has the lease.
	if _, err := l.Client.SetArgs(attempt, key, token, redis.SetArgs{
		Mode: string(redis.NX),
		TTL:  virtualDefaultProbeChainBudget + time.Minute,
	}).Result(); err != nil {
		return nil, false
	}
	return func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = releaseVirtualProbeLease.Run(cleanup, l.Client, []string{key}, token).Err()
	}, true
}
