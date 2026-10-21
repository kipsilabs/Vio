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
// A configured but unreachable Redis rejects optional work.
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

func (l *VirtualProbeLease) acquire(ctx context.Context, provider string) (func(), bool) {
	if l == nil {
		return func() {}, true
	}
	if l.Client == nil {
		return nil, false
	}
	key := fmt.Sprintf("vio:preplay:provider:%x", sha256.Sum256([]byte(provider)))
	token := uuid.NewString()
	attempt, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	ok, err := l.Client.SetNX(attempt, key, token, virtualDefaultProbeChainBudget+time.Minute).Result()
	if err != nil || !ok {
		return nil, false
	}
	return func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = releaseVirtualProbeLease.Run(cleanup, l.Client, []string{key}, token).Err()
	}, true
}
