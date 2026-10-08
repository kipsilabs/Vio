package catalog

import (
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/robfig/cron/v3"
)

// cronParser uses the standard 5-field cron format (minute hour dom month dow).
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// ParseCronExpression validates a cron expression string.
// It accepts the standard 5-field format: minute hour dom month dow.
func ParseCronExpression(expr string) error {
	_, err := cronParser.Parse(expr)
	if err != nil {
		return fmt.Errorf("invalid cron expression: %w", err)
	}
	return nil
}

// computeNextSyncAt parses a cron expression and returns the next scheduled
// time after now, with a small random jitter (0-15 minutes) to prevent
// thundering herd when many collections share the same schedule.
// Returns nil if the schedule is nil or empty.
func computeNextSyncAt(schedule *string) *time.Time {
	if schedule == nil || *schedule == "" {
		return nil
	}
	sched, err := cronParser.Parse(*schedule)
	if err != nil {
		return nil
	}
	next := sched.Next(time.Now())
	jitter := time.Duration(rand.IntN(15*60)) * time.Second
	next = next.Add(jitter)
	return &next
}

// ComputeNextSyncAtFrom parses a cron expression and returns the next
// scheduled time after the given reference time, with a small random jitter
// (0-15 minutes) to prevent thundering herd on subsequent cycles.
func ComputeNextSyncAtFrom(schedule string, after time.Time) *time.Time {
	sched, err := cronParser.Parse(schedule)
	if err != nil {
		return nil
	}
	next := sched.Next(after)
	jitter := time.Duration(rand.IntN(15*60)) * time.Second
	next = next.Add(jitter)
	return &next
}
