package passage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/metrics"
)

// Reconciler drives a Passage's steps to completion.
//
// It is a thin loop around Engine.Advance: the engine decides what happens, the
// controller persists the result and comes back when asked. Keeping the
// decision-making out of the controller is what lets the engine be tested
// exhaustively without a cluster.
type Reconciler struct {
	client.Client
	Engine   *Engine
	Recorder record.EventRecorder
	// WorkRoot is the base directory for Passage scratch space. Empty means a
	// directory under the system temp dir.
	WorkRoot string
	// Now is the clock, injectable for tests.
	Now func() time.Time
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// workRoot is where per-Passage scratch directories live.
func (r *Reconciler) workRoot() string {
	if r.WorkRoot != "" {
		return r.WorkRoot
	}
	return filepath.Join(os.TempDir(), "hecate-passages")
}

// WorkDir is the scratch directory shared by one Passage's steps — where one
// step clones a repository and a later one commits it.
//
// Keyed by UID rather than name so a recreated Passage never inherits a
// previous one's leftovers.
//
// It is deliberately **local and disposable**. A controller restart loses it,
// and that is acceptable: D5 already requires steps to be re-entrant, so a step
// must cope with an empty work dir — `git-clone` clones again. Making it
// durable would mean a PersistentVolume per Passage, which is a great deal of
// machinery to avoid re-running a clone.
func (r *Reconciler) WorkDir(p *v1alpha1.Passage) string {
	return filepath.Join(r.workRoot(), string(p.UID))
}

// +kubebuilder:rbac:groups=hecate.dev,resources=passages,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=hecate.dev,resources=passages/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=hecate.dev,resources=bundles,verbs=get;list;watch

// Reconcile advances one Passage.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var passage v1alpha1.Passage
	if err := r.Get(ctx, req.NamespacedName, &passage); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Already finished. Clean up any scratch left behind and stop — a Passage is
	// a permanent record, so we never re-run one.
	if passage.Status.Phase.Terminal() {
		r.cleanup(ctx, &passage)
		return ctrl.Result{}, nil
	}

	// The Bundle carries the artifact versions steps interpolate. A Passage
	// whose Bundle has been deleted cannot be completed honestly, so it fails
	// rather than running against nothing.
	bundle, err := r.bundle(ctx, &passage)
	if err != nil {
		return ctrl.Result{}, err
	}
	if bundle == nil {
		return ctrl.Result{}, r.failMissingBundle(ctx, &passage)
	}

	if err := os.MkdirAll(r.WorkDir(&passage), 0o700); err != nil {
		return ctrl.Result{}, fmt.Errorf("creating work directory: %w", err)
	}

	before := passage.Status.Phase
	outcome := r.Engine.Advance(ctx, &passage, bundle, r.WorkDir(&passage))

	passage.Status = outcome.Status
	passage.Status.ObservedGeneration = passage.Generation
	if len(outcome.Watch) > 0 {
		// Hand the Gate what the steps asked it to keep watching. Without this
		// the Gate goes blind the moment the Passage ends.
		passage.Status.Watch = outcome.Watch
	}

	// Emitted before the status write, because the trace ID has to be persisted
	// in the same update that records the terminal phase. A Passage is never
	// reconciled again once terminal, so a second update afterwards is a second
	// chance to lose it permanently.
	if passage.Status.Phase.Terminal() {
		passage.Status.TraceID = recordTrace(&passage)
	}

	if err := r.Status().Update(ctx, &passage); err != nil {
		return ctrl.Result{}, err
	}

	if passage.Status.Phase.Terminal() {
		r.announce(&passage, before)
		recordMetrics(&passage)
		r.cleanup(ctx, &passage)
		logger.Info("passage finished",
			"phase", passage.Status.Phase, "gate", passage.Spec.Gate, "bundle", passage.Spec.Bundle)
		return ctrl.Result{}, nil
	}

	return ctrl.Result{RequeueAfter: outcome.RequeueAfter}, nil
}

func (r *Reconciler) bundle(ctx context.Context, p *v1alpha1.Passage) (*v1alpha1.Bundle, error) {
	var bundle v1alpha1.Bundle
	err := r.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: p.Spec.Bundle}, &bundle)
	switch {
	case err == nil:
		return &bundle, nil
	case client.IgnoreNotFound(err) == nil:
		return nil, nil
	default:
		return nil, err
	}
}

func (r *Reconciler) failMissingBundle(ctx context.Context, p *v1alpha1.Passage) error {
	now := metav1.Time{Time: r.now()}
	p.Status.Phase = v1alpha1.PassageFailed
	p.Status.Message = fmt.Sprintf("Bundle %s no longer exists", p.Spec.Bundle)
	p.Status.FinishedAt = &now
	r.event(p, corev1.EventTypeWarning, "BundleMissing", p.Status.Message)
	return r.Status().Update(ctx, p)
}

// cleanup removes the scratch directory. Failure is logged, not returned: a
// leftover temp directory is untidy, whereas failing the reconcile over it
// would wedge a Passage that has already finished.
func (r *Reconciler) cleanup(ctx context.Context, p *v1alpha1.Passage) {
	if err := os.RemoveAll(r.WorkDir(p)); err != nil {
		log.FromContext(ctx).Error(err, "could not remove Passage work directory", "passage", p.Name)
	}
}

// announce records an event when a Passage reaches a terminal phase, but only
// on the transition — a Passage is reconciled repeatedly and must not emit the
// same event each time.
func (r *Reconciler) announce(p *v1alpha1.Passage, before v1alpha1.PassagePhase) {
	if before.Terminal() {
		return
	}
	switch p.Status.Phase {
	case v1alpha1.PassageSucceeded:
		r.event(p, corev1.EventTypeNormal, "PassageSucceeded",
			fmt.Sprintf("Bundle %s crossed %s", p.Spec.Bundle, p.Spec.Gate))
	case v1alpha1.PassageAborted:
		r.event(p, corev1.EventTypeWarning, "PassageAborted",
			fmt.Sprintf("crossing %s was aborted", p.Spec.Gate))
	default:
		r.event(p, corev1.EventTypeWarning, "PassageFailed",
			fmt.Sprintf("crossing %s failed: %s", p.Spec.Gate, p.Status.Message))
	}
}

// recordMetrics publishes the finished crossing and each of its steps.
//
// Called only on the transition to terminal, from the same branch as the event,
// so a Passage reconciled again after finishing is not counted twice.
func recordMetrics(p *v1alpha1.Passage) {
	metrics.RecordPassage(p.Namespace, p.Spec.Gate, p.Status.Phase, span(p.Status.StartedAt, p.Status.FinishedAt))
	for _, st := range p.Status.Steps {
		if st.Phase.Terminal() {
			metrics.RecordStep(p.Namespace, p.Spec.Gate, st.Uses, st.Phase, span(st.StartedAt, st.FinishedAt))
		}
	}
}

// span returns the seconds between two timestamps, or -1 when either is unset
// so the caller can skip rather than record a nonsense observation.
func span(from, to *metav1.Time) float64 {
	if from == nil || to == nil {
		return -1
	}
	return to.Sub(from.Time).Seconds()
}

func (r *Reconciler) event(p *v1alpha1.Passage, eventType, reason, message string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Event(p, eventType, reason, message)
}

// SetupWithManager registers the controller.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		// The new events API changes Eventf's signature — it adds an `action`
		// argument — so this is a user-visible change to the events people alert
		// on rather than a rename. Tracked in #116; the old API is deprecated but
		// not removed, and controller-runtime suppresses it the same way itself.
		r.Recorder = mgr.GetEventRecorderFor("passage-controller") //nolint:staticcheck // see #116
	}
	if r.Engine == nil {
		return fmt.Errorf("passage Reconciler requires an Engine")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Passage{}).
		Named("passage").
		Complete(r)
}
