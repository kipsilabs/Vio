package handlers

import (
	"context"
	"encoding/json"
	"github.com/Silo-Server/silo-server/internal/playback"
	"time"
)

type staleFirstGetAttemptPlanStoreV3 struct {
	playback.PlanStoreV3
	stale playback.AttemptRecordV3
	first bool
}

func (s *staleFirstGetAttemptPlanStoreV3) GetAttempt(ctx context.Context, id string) (*playback.AttemptRecordV3, error) {
	if !s.first {
		s.first = true
		x := s.stale
		return &x, nil
	}
	return s.PlanStoreV3.GetAttempt(ctx, id)
}

var _ = json.RawMessage{}
var _ = time.Time{}
