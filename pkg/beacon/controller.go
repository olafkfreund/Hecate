package beacon

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// LabelBeacon associates a Bundle with the Beacon that emitted it.
//
// Deliberately a label rather than an owner reference: an owner reference would
// cascade-delete every Bundle when a Beacon is removed, destroying the record of
// what is running in production. For a tool whose product is the audit trail
// that is the wrong default. Cleanup is the garbage collector's job, under the
// safety rule in D13.
const LabelBeacon = "hecate.dev/beacon"

// DefaultInterval is used when a Beacon does not set one.
const DefaultInterval = 5 * time.Minute

// Reconciler polls a Beacon's watched sources and emits Bundles.
type Reconciler struct {
	client.Client
	Resolver *Resolver
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

// +kubebuilder:rbac:groups=hecate.dev,resources=beacons,verbs=get;list;watch
// +kubebuilder:rbac:groups=hecate.dev,resources=beacons/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=hecate.dev,resources=bundles,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile polls one Beacon.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var beacon v1alpha1.Beacon
	if err := r.Get(ctx, req.NamespacedName, &beacon); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if beacon.Spec.Suspend {
		// Nothing to poll, and no requeue: a spec change wakes us up. Reported
		// explicitly so "why has nothing appeared?" has an answer.
		r.setReady(&beacon, metav1.ConditionFalse, "Suspended", "Beacon is suspended")
		return ctrl.Result{}, r.updateStatus(ctx, &beacon)
	}

	artifacts, problems := r.resolveAll(ctx, &beacon)

	// Always record what we saw, even when no Bundle results. This is the field
	// that answers "why has nothing appeared?".
	beacon.Status.Discovered = artifacts
	beacon.Status.LastPolled = &metav1.Time{Time: r.now()}
	beacon.Status.ObservedGeneration = beacon.Generation

	interval := beacon.Spec.Interval.Duration
	if interval <= 0 {
		interval = DefaultInterval
	}

	switch {
	case len(problems) > 0:
		// A partial resolution must not become a Bundle: a Bundle missing one of
		// its artifacts would promote an incomplete set, which is worse than
		// promoting nothing.
		reason, msg := summarise(problems)
		r.setReady(&beacon, metav1.ConditionFalse, reason, msg)
		logger.Info("beacon did not resolve every source", "reason", reason, "detail", msg)

	case beacon.Spec.Emit == v1alpha1.EmitManual:
		r.setReady(&beacon, metav1.ConditionTrue, "Discovered",
			fmt.Sprintf("resolved %d source(s); emit policy is Manual", len(artifacts)))

	default:
		created, name, err := r.emit(ctx, &beacon, artifacts)
		if err != nil {
			r.setReady(&beacon, metav1.ConditionFalse, "EmitFailed", err.Error())
			if updateErr := r.updateStatus(ctx, &beacon); updateErr != nil {
				logger.Error(updateErr, "could not update status after emit failure")
			}
			return ctrl.Result{}, err
		}
		beacon.Status.LatestBundle = name
		if created {
			logger.Info("emitted Bundle", "bundle", name)
			r.event(&beacon, corev1.EventTypeNormal, "BundleEmitted",
				fmt.Sprintf("emitted Bundle %s from %d artifact(s)", name, len(artifacts)))
		}
		r.setReady(&beacon, metav1.ConditionTrue, "Discovered",
			fmt.Sprintf("resolved %d source(s); latest Bundle is %s", len(artifacts), name))
	}

	if err := r.updateStatus(ctx, &beacon); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: interval}, nil
}

// problem records one source that could not be resolved.
type problem struct {
	index  int
	reason string
	err    error
}

// resolveAll resolves every watched source, collecting failures rather than
// stopping at the first: a Beacon watching three images should report all three
// problems, not make the operator fix them one reconcile at a time.
func (r *Reconciler) resolveAll(ctx context.Context, beacon *v1alpha1.Beacon) ([]v1alpha1.Artifact, []problem) {
	artifacts := make([]v1alpha1.Artifact, 0, len(beacon.Spec.Watch))
	var problems []problem

	for i, watch := range beacon.Spec.Watch {
		artifact, err := r.Resolver.Resolve(ctx, beacon.Namespace, watch)
		if err == nil {
			artifacts = append(artifacts, artifact)
			continue
		}

		var noMatch *ErrNoMatch
		var unsupported *ErrUnsupported
		switch {
		case errors.As(err, &noMatch):
			// Correctly configured, nothing published yet. Normal, not broken.
			problems = append(problems, problem{index: i, reason: "NoMatchingArtifact", err: err})
		case errors.As(err, &unsupported):
			problems = append(problems, problem{index: i, reason: "UnsupportedSource", err: err})
		default:
			problems = append(problems, problem{index: i, reason: "ResolveFailed", err: err})
		}
	}
	return artifacts, problems
}

// emit creates the Bundle for a set of artifacts if it does not already exist.
//
// Idempotency is structural rather than checked: the Bundle's name is derived
// from the content digest, so re-emitting an unchanged set collides on the name
// and the API server rejects it. No read-then-write race to lose.
func (r *Reconciler) emit(
	ctx context.Context, beacon *v1alpha1.Beacon, artifacts []v1alpha1.Artifact,
) (created bool, name string, err error) {
	if len(artifacts) == 0 {
		return false, "", fmt.Errorf("no artifacts resolved")
	}

	digest := v1alpha1.ComputeDigest(artifacts)
	name = v1alpha1.BundleName(beacon.Name, digest)

	bundle := &v1alpha1.Bundle{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: beacon.Namespace,
			Labels:    map[string]string{LabelBeacon: beacon.Name},
		},
		Spec: v1alpha1.BundleSpec{
			Beacon:    beacon.Name,
			Digest:    digest,
			Artifacts: artifacts,
		},
	}

	if err := r.Create(ctx, bundle); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// The expected steady state: nothing has changed since the last poll.
			return false, name, nil
		}
		return false, name, fmt.Errorf("creating Bundle %s: %w", name, err)
	}
	return true, name, nil
}

func summarise(problems []problem) (reason, message string) {
	first := problems[0]
	message = fmt.Sprintf("watch[%d]: %s", first.index, first.err)
	if len(problems) > 1 {
		message = fmt.Sprintf("%s (and %d more)", message, len(problems)-1)
	}
	return first.reason, message
}

func (r *Reconciler) setReady(beacon *v1alpha1.Beacon, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&beacon.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: beacon.Generation,
		LastTransitionTime: metav1.Time{Time: r.now()},
	})
}

func (r *Reconciler) updateStatus(ctx context.Context, beacon *v1alpha1.Beacon) error {
	return r.Status().Update(ctx, beacon)
}

func (r *Reconciler) event(beacon *v1alpha1.Beacon, eventType, reason, message string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Event(beacon, eventType, reason, message)
}

// SetupWithManager registers the controller.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Resolver == nil {
		r.Resolver = &Resolver{Client: mgr.GetClient()}
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("beacon-controller")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Beacon{}).
		Named("beacon").
		Complete(r)
}
