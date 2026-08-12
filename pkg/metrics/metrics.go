// Package metrics publishes Hecate's delivery metrics.
//
// controller-runtime already exports reconcile counts, durations, errors and
// workqueue depth — everything needed to answer "is the controller working".
// These answer the different question: "is delivery working". The DORA four
// derive from them, which is why they are shaped around crossings rather than
// reconciles.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// crossingBuckets span a second to about two hours. A crossing that waits on a
// pull request review is not an outlier to be clipped — it is the case the
// histogram most needs to represent honestly.
var crossingBuckets = prometheus.ExponentialBuckets(1, 2.5, 12)

var (
	// PassageDuration measures a whole crossing, start to terminal phase.
	PassageDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "hecate_passage_duration_seconds",
		Help:    "Time from a Passage starting to reaching a terminal phase.",
		Buckets: crossingBuckets,
	}, []string{"namespace", "gate", "outcome"})

	// PassagesTotal counts crossings by outcome. Deployment frequency and
	// change-failure rate both come from this one series.
	PassagesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hecate_passages_total",
		Help: "Passages that reached a terminal phase, by outcome.",
	}, []string{"namespace", "gate", "outcome"})

	// StepDuration measures individual steps.
	//
	// Deliberately generic rather than a bespoke "flux convergence" metric:
	// step="flux-wait" *is* how long Flux took to converge, and every other
	// step gets measured for free. A special case would have cost more code and
	// answered fewer questions.
	StepDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "hecate_step_duration_seconds",
		Help:    "Time a Passage step took, from first invocation to terminal phase.",
		Buckets: crossingBuckets,
	}, []string{"namespace", "gate", "step", "outcome"})

	// GateHealth is one series per state, exactly one of which is 1.
	//
	// The alternative — a single gauge mapping states to numbers — forces every
	// dashboard and alert to hard-code that mapping, and silently means the
	// wrong thing the day a state is added.
	GateHealth = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hecate_gate_health",
		Help: "Gate health, 1 for the current state and 0 for the others.",
	}, []string{"namespace", "gate", "status"})
)

// healthStates is the full set, so every series is published rather than
// appearing only once a Gate first enters that state. A gauge that springs into
// existence mid-incident breaks alerting rules that reference it.
var healthStates = []v1alpha1.Health{
	v1alpha1.HealthHealthy,
	v1alpha1.HealthProgressing,
	v1alpha1.HealthDegraded,
	v1alpha1.HealthUnknown,
	v1alpha1.HealthNotApplicable,
}

// RecordGateHealth publishes a Gate's current health.
func RecordGateHealth(namespace, gate string, current v1alpha1.Health) {
	for _, state := range healthStates {
		value := 0.0
		if state == current {
			value = 1.0
		}
		GateHealth.WithLabelValues(namespace, gate, string(state)).Set(value)
	}
}

// ForgetGate removes a deleted Gate's series. Without this a Gate that is
// removed keeps reporting its last health for ever, and an alert on it never
// clears.
func ForgetGate(namespace, gate string) {
	for _, state := range healthStates {
		GateHealth.DeleteLabelValues(namespace, gate, string(state))
	}
}

// RecordPassage publishes a finished crossing.
func RecordPassage(namespace, gate string, phase v1alpha1.PassagePhase, seconds float64) {
	outcome := string(phase)
	PassagesTotal.WithLabelValues(namespace, gate, outcome).Inc()
	// A negative or absent duration means the timestamps were not both set;
	// recording it would corrupt the histogram with nonsense.
	if seconds >= 0 {
		PassageDuration.WithLabelValues(namespace, gate, outcome).Observe(seconds)
	}
}

// RecordStep publishes a finished step.
func RecordStep(namespace, gate, step string, phase v1alpha1.StepPhase, seconds float64) {
	if seconds < 0 {
		return
	}
	StepDuration.WithLabelValues(namespace, gate, step, string(phase)).Observe(seconds)
}

// LeadTime measures artifact discovery to crossing.
//
// **This is a slice of DORA lead time, not the whole of it.** DORA measures
// commit to production; Hecate first sees an artifact when a Beacon discovers
// it, which is already past the build. What is measured here is the part Hecate
// can observe honestly — and, on most teams, the larger and more actionable
// part. Naming it `lead_time` outright would be a claim we cannot support
// (D43).
var LeadTime = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "hecate_bundle_lead_time_seconds",
	Help:    "Time from a Bundle being emitted to a crossing with it succeeding.",
	Buckets: leadTimeBuckets,
}, []string{"namespace", "gate"})

// GateDegraded measures how long a Gate stayed Degraded, observed when it
// stops being Degraded.
//
// The closest honest analogue of time-to-restore: Hecate knows when a Gate's
// health broke and when it came back, and knows nothing about incidents,
// pages or customers. Named for what it measures rather than for the DORA
// metric it approximates.
var GateDegraded = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "hecate_gate_degraded_seconds",
	Help:    "How long a Gate remained Degraded, observed when it recovered.",
	Buckets: leadTimeBuckets,
}, []string{"namespace", "gate"})

// leadTimeBuckets span a minute to about three weeks. Lead time is measured in
// hours and days on most teams, and an artifact that sat unpromoted for a
// fortnight is the observation a delivery dashboard most needs to show.
var leadTimeBuckets = prometheus.ExponentialBuckets(60, 3, 11)

// Collectors is every metric this package exports.
//
// One list, read by both registration and the dashboard check, so a metric
// cannot be added to the exporter and silently forgotten by the dashboard —
// which is how a delivery dashboard rots into a wall of "No data".
func Collectors() []prometheus.Collector {
	return []prometheus.Collector{
		PassageDuration, PassagesTotal, StepDuration, GateHealth, LeadTime, GateDegraded,
	}
}

func init() {
	ctrlmetrics.Registry.MustRegister(Collectors()...)
}

// RecordLeadTime publishes a successful crossing's lead time. Called only on
// success: a crossing that failed delivered nothing, and counting it would
// flatter the number.
func RecordLeadTime(namespace, gate string, seconds float64) {
	if seconds < 0 {
		return
	}
	LeadTime.WithLabelValues(namespace, gate).Observe(seconds)
}

// RecordGateRecovered publishes how long a Gate was Degraded.
func RecordGateRecovered(namespace, gate string, seconds float64) {
	if seconds < 0 {
		return
	}
	GateDegraded.WithLabelValues(namespace, gate).Observe(seconds)
}
