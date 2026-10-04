package usercollections

import (
	"fmt"
	"strings"
	"time"

	"github.com/Silo-Server/silo-server/internal/catalog"
)

// AllowedSyncSchedules maps a UI-friendly cadence label to the cron
// expression we persist. The set is fixed (no user-supplied cron) so no
// schedule runs more than once a day, which keeps provider quota bounded
// without re-parsing the cron tree. All schedules fire at 04:30 on the node's
// local clock (see catalog.ComputeNextSyncAtFrom), so across a daylight saving
// change two daily runs are 23 or 25 hours apart, plus up to 15 minutes of
// jitter either way.
var AllowedSyncSchedules = map[string]string{
	"daily":   "30 4 * * *",
	"weekly":  "30 4 * * 0",
	"monthly": "30 4 1 * *",
}

func ResolveSyncSchedule(label string) (*string, error) {
	label = strings.TrimSpace(strings.ToLower(label))
	if label == "" {
		return nil, nil
	}
	expr, ok := AllowedSyncSchedules[label]
	if !ok {
		return nil, fmt.Errorf("invalid sync_schedule %q: must be one of daily, weekly, monthly", label)
	}
	return &expr, nil
}

func InitialNextSyncAt(schedule *string) *time.Time {
	if schedule == nil || *schedule == "" {
		return nil
	}
	return catalog.ComputeNextSyncAtFrom(*schedule, time.Now())
}
