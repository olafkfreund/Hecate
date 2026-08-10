package gate

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/health"
)

const (
	// LabelGate associates a Passage with the Gate being crossed.
	LabelGate = "hecate.dev/gate"
	// LabelBundle associates a Passage with the Bundle being moved.
	LabelBundle = "hecate.dev/bundle"

	// ReconcileInterval is how often a Gate re-assesses health and eligibility.
	//
	// ponytail: a flat interval rather than scheduling a wake-up at the next
	// window opening. One reconcile per Gate per minute costs nothing, and the
	// precise version needs its own cron evaluation to decide when to wake.
	// Revisit if someone runs thousands of Gates.
	ReconcileInterval = time.Minute

	// HistoryLimit caps GateStatus.History. An unbounded list inside a status
	// object is rewritten on every reconcile and grows for ever; the durable
	// record lives on the Bundle and in the evidence store. See D13.
	HistoryLimit = 10
)

// Reconciler drives one Gate: assess health, record crossings, decide what is
// eligible, and start automatic Passages.
type Reconciler struct {
	client.Client
	// Health assesses the Gate's watches. May be nil, in which case health is
	// reported as NotApplicable rather than silently omitted.
	Health   *health.Registry
	Recorder record.EventRecorder
	// Now is the clock, injectable for tests.
	Now func() time.Time
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// +kubebuilder:rbac:groups=hecate.dev,resources=gates,verbs=get;list;watch
// +kubebuilder:rbac:groups=hecate.dev,resources=gates/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=hecate.dev,resources=bundles,verbs=get;list;watch
// +kubebuilder:rbac:groups=hecate.dev,resources=bundles/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=hecate.dev,resources=passages,verbs=get;list;watch;create

// Reconcile brings one Gate up to date.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var gate v1alpha1.Gate
	if err := r.Get(ctx, req.NamespacedName, &gate); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	gate.Status.ObservedGeneration = gate.Generation

	if gate.Spec.Suspend {
		gate.Status.Eligible = nil
		r.setReady(&gate, metav1.ConditionFalse, "Suspended", "Gate is suspended")
		return ctrl.Result{}, r.Status().Update(ctx, &gate)
	}

	// Record finished crossings before judging eligibility, so a Passage that
	// just succeeded is reflected in `current` rather than leaving its Bundle
	// looking eligible for a Gate it is already in.
	active, err := r.observePassages(ctx, &gate)
	if err != nil {
		return ctrl.Result{}, err
	}

	gate.Status.Health = r.assess(ctx, &gate)

	var bundles v1alpha1.BundleList
	if err := r.List(ctx, &bundles, client.InNamespace(gate.Namespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing Bundles: %w", err)
	}

	candidates := Evaluate(&gate, bundles.Items)
	eligible := Eligible(candidates)
	gate.Status.Eligible = names(eligible)

	reason, message := r.advance(ctx, &gate, candidates, bundles.Items, active)
	r.setReady(&gate, metav1.ConditionTrue, reason, message)

	if err := r.Status().Update(ctx, &gate); err != nil {
		return ctrl.Result{}, err
	}
	logger.V(1).Info("gate reconciled", "eligible", len(eligible), "reason", reason)
	return ctrl.Result{RequeueAfter: ReconcileInterval}, nil
}

// advance starts an automatic Passage when one is warranted, and returns the
// condition reason and message describing what the Gate is doing or waiting on.
func (r *Reconciler) advance(
	ctx context.Context,
	gate *v1alpha1.Gate,
	candidates []Candidate,
	bundles []v1alpha1.Bundle,
	active *v1alpha1.Passage,
) (reason, message string) {
	if active != nil {
		gate.Status.ActivePassage = active.Name
		return "Crossing", fmt.Sprintf("Passage %s is in progress", active.Name)
	}
	gate.Status.ActivePassage = ""

	eligible := Eligible(candidates)
	if !gate.Spec.Auto {
		if len(eligible) == 0 {
			return "Idle", waitingOn(candidates)
		}
		return "Idle", fmt.Sprintf("%d Bundle(s) eligible; crossing must be requested", len(eligible))
	}

	// A window closed is not a failure: eligible Bundles queue and cross when it
	// opens. Say which, so the wait is legible.
	if allowed, why := Allowed(gate.Spec.Windows, r.now()); !allowed {
		return "WindowClosed", why
	}

	next := NextAuto(candidates, currentBundle(gate, bundles))
	if next == nil {
		if len(eligible) == 0 {
			return "Idle", waitingOn(candidates)
		}
		return "Idle", "nothing newer than the current Bundle"
	}

	passage, err := r.startPassage(ctx, gate, next)
	if err != nil {
		return "PassageFailed", err.Error()
	}
	gate.Status.ActivePassage = passage.Name
	r.event(gate, corev1.EventTypeNormal, "PassageStarted",
		fmt.Sprintf("started Passage %s to cross Bundle %s", passage.Name, next.Name))
	return "Crossing", fmt.Sprintf("Passage %s is in progress", passage.Name)
}

// startPassage creates a Passage, copying the Gate's steps into it.
//
// Copied rather than referenced: editing a Gate must not retroactively change
// what an in-flight or completed Passage did.
func (r *Reconciler) startPassage(
	ctx context.Context, gate *v1alpha1.Gate, bundle *v1alpha1.Bundle,
) (*v1alpha1.Passage, error) {
	if gate.Spec.Passage == nil || len(gate.Spec.Passage.Steps) == 0 {
		return nil, fmt.Errorf("Gate has no passage steps defined")
	}

	passage := &v1alpha1.Passage{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: gate.Name + "-",
			Namespace:    gate.Namespace,
			Labels: map[string]string{
				LabelGate:   gate.Name,
				LabelBundle: bundle.Name,
			},
		},
		Spec: v1alpha1.PassageSpec{
			Gate:   gate.Name,
			Bundle: bundle.Name,
			Steps:  gate.Spec.Passage.Steps,
			Actor:  "controller",
		},
	}
	if err := r.Create(ctx, passage); err != nil {
		return nil, fmt.Errorf("creating Passage: %w", err)
	}
	return passage, nil
}

// observePassages finds this Gate's Passages, records any completed crossing
// that has not been recorded yet, and returns the one still running, if any.
func (r *Reconciler) observePassages(ctx context.Context, gate *v1alpha1.Gate) (*v1alpha1.Passage, error) {
	var passages v1alpha1.PassageList
	if err := r.List(ctx, &passages,
		client.InNamespace(gate.Namespace),
		client.MatchingLabels{LabelGate: gate.Name},
	); err != nil {
		return nil, fmt.Errorf("listing Passages: %w", err)
	}

	var active *v1alpha1.Passage
	var newest *v1alpha1.Passage

	for i := range passages.Items {
		p := &passages.Items[i]
		if !p.Status.Phase.Terminal() {
			active = p
			continue
		}
		if newest == nil || p.CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = p
		}
	}

	if newest != nil {
		if err := r.recordOutcome(ctx, gate, newest); err != nil {
			return nil, err
		}
	}
	return active, nil
}

// recordOutcome writes a finished Passage into the Gate's status and the
// Bundle's history, once.
//
// The Gate controller owns this write rather than the Passage controller: it is
// the component that knows whether verification passed, and one writer per
// field avoids two controllers racing over the same status.
func (r *Reconciler) recordOutcome(ctx context.Context, gate *v1alpha1.Gate, passage *v1alpha1.Passage) error {
	if gate.Status.Current != nil && gate.Status.Current.Passage == passage.Name {
		return nil // already recorded
	}

	var bundle v1alpha1.Bundle
	if err := r.Get(ctx, client.ObjectKey{Namespace: gate.Namespace, Name: passage.Spec.Bundle}, &bundle); err != nil {
		return client.IgnoreNotFound(err)
	}

	crossing := v1alpha1.GateCrossing{
		Gate:    gate.Name,
		Passage: passage.Name,
		At:      metav1.Time{Time: r.now()},
		Actor:   passage.Spec.Actor,
	}

	if passage.Status.Phase != v1alpha1.PassageSucceeded {
		crossing.Reason = passage.Status.Message
		if !hasCrossing(bundle.Status.Blocked, passage.Name) {
			bundle.Status.Blocked = append(bundle.Status.Blocked, crossing)
			if err := r.Status().Update(ctx, &bundle); err != nil {
				return fmt.Errorf("recording blocked crossing on Bundle %s: %w", bundle.Name, err)
			}
		}
		r.event(gate, corev1.EventTypeWarning, "CrossingFailed",
			fmt.Sprintf("Bundle %s did not cross: %s", bundle.Name, passage.Status.Message))
		return nil
	}

	occupant := v1alpha1.GateOccupant{
		Bundle:    bundle.Name,
		Digest:    bundle.Spec.Digest,
		Passage:   passage.Name,
		EnteredAt: metav1.Time{Time: r.now()},
		Actor:     passage.Spec.Actor,
	}
	gate.Status.Current = &occupant
	gate.Status.History = append([]v1alpha1.GateOccupant{occupant}, gate.Status.History...)
	if len(gate.Status.History) > HistoryLimit {
		gate.Status.History = gate.Status.History[:HistoryLimit]
	}

	// `cleared` means crossed *and* verified. Verification is not implemented
	// yet (#21); when it is, this write is the single place it gates. Until
	// then a successful crossing is treated as cleared, which is correct for a
	// Gate that declares no verification.
	if !hasCrossing(bundle.Status.Cleared, passage.Name) {
		bundle.Status.Cleared = append(bundle.Status.Cleared, crossing)
		if err := r.Status().Update(ctx, &bundle); err != nil {
			return fmt.Errorf("recording crossing on Bundle %s: %w", bundle.Name, err)
		}
	}

	r.event(gate, corev1.EventTypeNormal, "BundleCrossed",
		fmt.Sprintf("Bundle %s crossed via Passage %s", bundle.Name, passage.Name))
	return nil
}

func (r *Reconciler) assess(ctx context.Context, gate *v1alpha1.Gate) *v1alpha1.HealthReport {
	if r.Health == nil {
		return &v1alpha1.HealthReport{Status: v1alpha1.HealthNotApplicable}
	}
	report := r.Health.Assess(ctx, gate.Namespace, gate.Name, gate.Spec.Watch)
	report.ObservedAt = &metav1.Time{Time: r.now()}
	return &report
}

// waitingOn explains why nothing is eligible, using the most common reason
// among the candidates — the one an operator most likely needs to act on.
func waitingOn(candidates []Candidate) string {
	if len(candidates) == 0 {
		return "no Bundles from any admitted Beacon"
	}
	counts := map[string]int{}
	best, bestN := "", 0
	for _, c := range candidates {
		if c.Eligible {
			continue
		}
		counts[c.Reason]++
		if counts[c.Reason] > bestN {
			best, bestN = c.Reason, counts[c.Reason]
		}
	}
	if best == "" {
		return "nothing to do"
	}
	if bestN == len(candidates) {
		return best
	}
	return fmt.Sprintf("%s (%d of %d Bundles)", best, bestN, len(candidates))
}

func currentBundle(gate *v1alpha1.Gate, bundles []v1alpha1.Bundle) *v1alpha1.Bundle {
	if gate.Status.Current == nil {
		return nil
	}
	for i := range bundles {
		if bundles[i].Name == gate.Status.Current.Bundle {
			return &bundles[i]
		}
	}
	return nil
}

func hasCrossing(crossings []v1alpha1.GateCrossing, passage string) bool {
	for _, c := range crossings {
		if c.Passage == passage {
			return true
		}
	}
	return false
}

func names(bundles []*v1alpha1.Bundle) []string {
	if len(bundles) == 0 {
		return nil
	}
	out := make([]string, len(bundles))
	for i, b := range bundles {
		out[i] = b.Name
	}
	return out
}

func (r *Reconciler) setReady(gate *v1alpha1.Gate, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&gate.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: gate.Generation,
		LastTransitionTime: metav1.Time{Time: r.now()},
	})
}

func (r *Reconciler) event(gate *v1alpha1.Gate, eventType, reason, message string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Event(gate, eventType, reason, message)
}

// SetupWithManager registers the controller.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("gate-controller")
	}
	// Watches rather than Owns: Passages carry no owner reference, because an
	// owner reference would cascade-delete the record of every crossing when a
	// Gate is removed. Mapping by spec.gate gives the same immediate wake-up
	// without that cost — otherwise a finished crossing would sit unrecorded
	// until the next flat-interval reconcile.
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Gate{}).
		Watches(&v1alpha1.Passage{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, obj client.Object) []ctrl.Request {
				passage, ok := obj.(*v1alpha1.Passage)
				if !ok || passage.Spec.Gate == "" {
					return nil
				}
				return []ctrl.Request{{NamespacedName: client.ObjectKey{
					Namespace: passage.Namespace, Name: passage.Spec.Gate,
				}}}
			})).
		Named("gate").
		Complete(r)
}
