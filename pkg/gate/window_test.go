package gate

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

func win(schedule string, d time.Duration) v1alpha1.Window {
	return v1alpha1.Window{Schedule: schedule, Duration: metav1.Duration{Duration: d}}
}

func at(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestNoWindowsAlwaysAllows(t *testing.T) {
	if ok, why := Allowed(nil, time.Now()); !ok {
		t.Errorf("no windows should always allow, got %q", why)
	}
}

func TestAllowWindow(t *testing.T) {
	// Weekday mornings, 09:00 UTC for six hours.
	windows := []v1alpha1.Window{win("0 9 * * 1-5", 6*time.Hour)}

	tests := []struct {
		name string
		when string
		want bool
	}{
		{"just after opening", "2026-08-10T09:00:01Z", true}, // Monday
		{"mid window", "2026-08-10T12:00:00Z", true},         // Monday
		{"one second before closing", "2026-08-10T14:59:59Z", true},
		{"after closing", "2026-08-10T15:00:01Z", false},
		{"before opening", "2026-08-10T08:00:00Z", false},
		{"weekend", "2026-08-09T12:00:00Z", false}, // Sunday
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, why := Allowed(windows, at(t, tt.when))
			if ok != tt.want {
				t.Errorf("Allowed = %v (%q), want %v", ok, why, tt.want)
			}
			if !ok && why == "" {
				t.Error("a refusal must explain itself")
			}
		})
	}
}

func TestDenyWindowBeatsAllow(t *testing.T) {
	// Open all week, except a Friday afternoon change freeze.
	freeze := win("0 12 * * 5", 12*time.Hour)
	freeze.Deny = true
	windows := []v1alpha1.Window{win("0 0 * * *", 24*time.Hour), freeze}

	if ok, _ := Allowed(windows, at(t, "2026-08-12T14:00:00Z")); !ok {
		t.Error("Wednesday afternoon should be allowed")
	}

	ok, why := Allowed(windows, at(t, "2026-08-14T14:00:00Z")) // Friday
	if ok {
		t.Error("a deny window must override an open allow window")
	}
	if why == "" {
		t.Error("a refusal must explain itself")
	}
}

func TestDenyOnlyAllowsOutsideTheFreeze(t *testing.T) {
	freeze := win("0 12 * * 5", 12*time.Hour)
	freeze.Deny = true
	windows := []v1alpha1.Window{freeze}

	if ok, _ := Allowed(windows, at(t, "2026-08-12T14:00:00Z")); !ok {
		t.Error("with only a deny window, everything outside it is allowed")
	}
	if ok, _ := Allowed(windows, at(t, "2026-08-14T14:00:00Z")); ok {
		t.Error("inside the freeze must be refused")
	}
}

// The reason TimeZone exists: a window defined in UTC drifts an hour against
// local working time twice a year.
func TestTimeZoneIsHonoured(t *testing.T) {
	w := win("0 9 * * *", 1*time.Hour)
	w.TimeZone = "Europe/London"
	windows := []v1alpha1.Window{w}

	// 10 August 2026 is BST (UTC+1), so 09:00 London is 08:00 UTC.
	if ok, why := Allowed(windows, at(t, "2026-08-10T08:30:00Z")); !ok {
		t.Errorf("08:30 UTC is 09:30 BST and should be inside the window: %s", why)
	}
	if ok, _ := Allowed(windows, at(t, "2026-08-10T09:30:00Z")); ok {
		t.Error("09:30 UTC is 10:30 BST, after the window closed")
	}

	// In January the same clock time is GMT (UTC+0), so it shifts by an hour.
	if ok, why := Allowed(windows, at(t, "2026-01-12T09:30:00Z")); !ok {
		t.Errorf("09:30 UTC is 09:30 GMT and should be inside the window: %s", why)
	}
	if ok, _ := Allowed(windows, at(t, "2026-01-12T08:30:00Z")); ok {
		t.Error("08:30 UTC is 08:30 GMT, before the window opens")
	}
}

// A window we cannot evaluate must never widen access: an unparseable freeze
// treated as "not in force" is the dangerous direction to fail.
func TestInvalidWindowRefuses(t *testing.T) {
	for name, w := range map[string]v1alpha1.Window{
		"bad schedule":  win("not a cron expression", time.Hour),
		"bad timezone":  {Schedule: "0 9 * * *", Duration: metav1.Duration{Duration: time.Hour}, TimeZone: "Mars/Olympus"},
		"zero duration": win("0 9 * * *", 0),
	} {
		t.Run(name, func(t *testing.T) {
			ok, why := Allowed([]v1alpha1.Window{w}, time.Now())
			if ok {
				t.Error("an invalid window must refuse, not allow")
			}
			if why == "" {
				t.Error("a refusal must explain itself")
			}
		})
	}
}

func TestOverlappingAllowWindows(t *testing.T) {
	windows := []v1alpha1.Window{
		win("0 9 * * 1-5", 2*time.Hour),  // 09:00-11:00
		win("0 14 * * 1-5", 2*time.Hour), // 14:00-16:00
	}
	if ok, _ := Allowed(windows, at(t, "2026-08-10T10:00:00Z")); !ok {
		t.Error("inside the first window")
	}
	if ok, _ := Allowed(windows, at(t, "2026-08-10T15:00:00Z")); !ok {
		t.Error("inside the second window")
	}
	if ok, _ := Allowed(windows, at(t, "2026-08-10T12:00:00Z")); ok {
		t.Error("between the two windows should be refused")
	}
}
