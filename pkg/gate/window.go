package gate

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// Allowed reports whether a Passage may start at t, and why not if it may not.
//
// Semantics:
//   - No windows: always allowed.
//   - Any deny window open: refused. Deny beats allow, because a change freeze
//     that an allow window can override is not a change freeze.
//   - Allow windows present: at least one must be open.
//
// A refusal is not a failure. Eligible Bundles queue and cross when the window
// opens, which is the entire point of having windows.
func Allowed(windows []v1alpha1.Window, t time.Time) (bool, string) {
	if len(windows) == 0 {
		return true, ""
	}

	var haveAllow, insideAllow bool

	for i, w := range windows {
		open, err := windowOpenAt(w, t)
		if err != nil {
			// A malformed window must not silently widen access. Refuse and say
			// so, rather than treating an unparseable freeze as "not in force".
			return false, fmt.Sprintf("windows[%d] is invalid: %s", i, err)
		}
		if w.Deny {
			if open {
				return false, fmt.Sprintf("blocked by deny window %q", w.Schedule)
			}
			continue
		}
		haveAllow = true
		if open {
			insideAllow = true
		}
	}

	if haveAllow && !insideAllow {
		return false, "outside every promotion window"
	}
	return true, ""
}

// windowOpenAt reports whether a window is open at t.
//
// A window opens at each cron activation and stays open for Duration. The only
// activation that could still have it open is one within the last Duration, so
// we ask for the first activation after t-Duration: if that is at or before t,
// the window opened then and has not yet closed.
func windowOpenAt(w v1alpha1.Window, t time.Time) (bool, error) {
	if w.Duration.Duration <= 0 {
		return false, fmt.Errorf("duration must be positive")
	}

	loc := time.UTC
	if w.TimeZone != "" {
		var err error
		if loc, err = time.LoadLocation(w.TimeZone); err != nil {
			return false, fmt.Errorf("unknown time zone %q: %w", w.TimeZone, err)
		}
	}

	schedule, err := cron.ParseStandard(w.Schedule)
	if err != nil {
		return false, fmt.Errorf("invalid schedule %q: %w", w.Schedule, err)
	}

	local := t.In(loc)
	next := schedule.Next(local.Add(-w.Duration.Duration))
	return !next.After(local), nil
}
