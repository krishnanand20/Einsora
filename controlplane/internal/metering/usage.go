// Package metering is usage accounting: it turns workload lifecycle events into hourly usage
// records for quotas, fair share, and reports. It never produces money (ADR-0011). Usage is based on
// allocation (time in a metered state), not utilization (ADR-0006).
package metering

import (
	"time"

	"github.com/krishnanand20/einsora/controlplane/internal/domain"
)

type Interval struct{ Start, End time.Time }

// MeteredIntervals turns a workload's time-ordered events into the periods it spent in a
// metered state, clipped to until. An interval still open at until ends there.
func MeteredIntervals(events []domain.Event, until time.Time) []Interval {
	var out []Interval
	var start time.Time
	open := false
	for _, e := range events {
		if !e.At.Before(until) {
			break
		}
		if e.From == e.To {
			continue // a note: never a state change, even if it was written against a stale state
		}
		switch metered := e.To.Metered(); {
		case metered && !open:
			start, open = e.At, true
		case !metered && open:
			out = append(out, Interval{start, e.At})
			open = false
		}
	}
	if open && start.Before(until) {
		out = append(out, Interval{start, until})
	}
	return out
}

// SplitHourly returns whole metered seconds per UTC clock hour.
func SplitHourly(intervals []Interval) map[time.Time]int64 {
	out := map[time.Time]int64{}
	for _, iv := range intervals {
		end := iv.End.UTC()
		for t := iv.Start.UTC(); t.Before(end); {
			hour := t.Truncate(time.Hour)
			next := hour.Add(time.Hour)
			if end.Before(next) {
				next = end
			}
			out[hour] += int64(next.Sub(t) / time.Second)
			t = next
		}
	}
	return out
}
