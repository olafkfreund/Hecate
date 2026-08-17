package gate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/health"
	"github.com/olafkfreund/hecate/pkg/metrics"
	"github.com/olafkfreund/hecate/pkg/passage"
	"github.com/olafkfreund/hecate/pkg/verify"
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

	// BlockedLimit caps BundleStatus.Blocked, for the same reason HistoryLimit
	// caps the Gate's history: a list inside a status subresource that only
	// grows will eventually make the object exceed etcd's size limit, at which
	// point nothing can be recorded against it at all.
	//
	// It reached 733KB and 2,065 entries in twenty minutes once, all of them
	// the same DNS failure (#121). The retry loop that produced them is fixed,
	// but the cap stays: an unbounded list is a defect on its own, and this one
	// is on the object a promotion cannot proceed without.
	BlockedLimit = 10
)

// Reconciler drives one Gate: assess health, record crossings, decide what is
// eligible, and start automatic Passages.
type Reconciler struct {
	client.Client
	// Health assesses the Gate's watches. May be nil, in which case health is
	// reported as NotApplicable rather than silently omitted.
	Health *health.Registry
	// Steps validates a Gate's step list. May be nil, in which case a Gate is
	// not checked — which is worse than checking it, and better than a
	// controller that refuses to start because nobody wired a registry.
	Steps    *passage.Registry
	Recorder events.EventRecorder
	// Verifiers is the verifier registry, injectable for tests. Nil uses the
	// built-in set.
	Verifiers map[string]Verifier
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
// +kubebuilder:rbac:groups=hecate.dev,resources=passages,verbs=get;list;watch;create;delete
// Flagger's, read only and optional: the CRD need not exist unless a Gate
// declares a canary verification.
// +kubebuilder:rbac:groups=flagger.app,resources=canaries,verbs=get;list;watch

// Reconcile brings one Gate up to date.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var gate v1alpha1.Gate
	if err := r.Get(ctx, req.NamespacedName, &gate); err != nil {
		if client.IgnoreNotFound(err) == nil {
			// Stop reporting a deleted Gate's health, or an alert on it never
			// clears.
			metrics.ForgetGate(req.Namespace, req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	gate.Status.ObservedGeneration = gate.Generation
	gate.Status.LastHandledReconcileAt = v1alpha1.ReconcileRequestedAt(gate.Annotations)

	if gate.Spec.Suspend {
		gate.Status.Eligible = nil
		r.setReady(&gate, metav1.ConditionFalse, "Suspended", "Gate is suspended")
		return ctrl.Result{}, r.Status().Update(ctx, &gate)
	}

	// Checked before anything else looks at this Gate, and fatal to it: a Gate
	// whose steps are wrong must not open a Passage, because the alternative is
	// discovering the mistake part-way through a crossing with a commit already
	// pushed. Reported rather than retried — nothing here becomes true by
	// waiting (D31).
	if problems := r.checkSteps(&gate); len(problems) > 0 {
		gate.Status.Eligible = nil
		r.setReady(&gate, metav1.ConditionFalse, ReasonInvalidSteps, problems)
		r.event(&gate, corev1.EventTypeWarning, ReasonInvalidSteps, "Validating", problems)
		logger.Info("gate has invalid steps", "problems", problems)
		return ctrl.Result{}, r.Status().Update(ctx, &gate)
	}

	// Record finished crossings before judging eligibility, so a Passage that
	// just succeeded is reflected in `current` rather than leaving its Bundle
	// looking eligible for a Gate it is already in.
	active, latest, unverified, err := r.observePassages(ctx, &gate)
	if err != nil {
		return ctrl.Result{}, err
	}

	// The whole previous report, not just its status: ending a Degraded period
	// needs to know when that period began, and assess is about to replace it.
	previous := gate.Status.Health
	gate.Status.Health = r.assess(ctx, &gate, adopted(latest))
	r.announceHealth(&gate, previous)

	var bundles v1alpha1.BundleList
	if err := r.List(ctx, &bundles, client.InNamespace(gate.Namespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing Bundles: %w", err)
	}

	candidates := Evaluate(&gate, bundles.Items)
	eligible := Eligible(candidates)
	gate.Status.Eligible = names(eligible)

	reason, message := r.advance(ctx, &gate, candidates, bundles.Items, active)
	status := metav1.ConditionTrue
	// A crossing that has not verified outranks whatever advance found to say.
	// Without this the Gate reports "Idle: already in this Gate" — true, and
	// useless, on a Gate whose canary has just rolled back.
	if unverified != "" {
		status, reason, message = metav1.ConditionFalse, "Verifying", unverified
	}
	r.setReady(&gate, status, reason, message)

	// After advance, so a Passage it has just opened is already named in
	// status and protected, and after observePassages, so `current` names the
	// crossing that put what is running where it is.
	if deleted, err := r.collect(ctx, &gate); err != nil {
		// Collection failing must not stop the Gate: one that cannot tidy up
		// should still be promoting.
		logger.Error(err, "collecting finished Passages")
	} else if deleted > 0 {
		logger.Info("collected finished Passages", "count", deleted)
	}

	if err := r.Status().Update(ctx, &gate); err != nil {
		return ctrl.Result{}, err
	}
	logger.V(1).Info("gate reconciled", "eligible", len(eligible), "reason", reason)
	return ctrl.Result{RequeueAfter: ReconcileInterval}, nil
}

// ReasonInvalidSteps marks a Gate whose Passage template will not run.
const ReasonInvalidSteps = "InvalidSteps"

// checkSteps renders everything wrong with the Gate's step list, or "".
//
// The whole list rather than the first problem: a Gate is edited as a unit, and
// reporting one mistake per apply turns a single bad edit into several rounds.
func (r *Reconciler) checkSteps(gate *v1alpha1.Gate) string {
	if r.Steps == nil || gate.Spec.Passage == nil {
		return ""
	}
	problems := r.Steps.Validate(gate.Spec.Passage.Steps)
	if len(problems) == 0 {
		return ""
	}
	rendered := make([]string, len(problems))
	for i, p := range problems {
		rendered[i] = p.Error()
	}
	return strings.Join(rendered, "; ")
}

// crossingMessage says what the crossing is doing, and in particular why it is
// not finishing.
//
// "Passage x is in progress" is true of a crossing that is thirty seconds old
// and of one that has been waiting six hours for a human to sign off, and an
// operator asking "why is this sitting there?" needs those to read differently.
func crossingMessage(active *v1alpha1.Passage) string {
	ev := active.Status.Evidence
	if ev == nil || ev.Verdict == "" || ev.Verdict == "approve" {
		return fmt.Sprintf("Passage %s is in progress", active.Name)
	}
	msg := fmt.Sprintf("Passage %s is held by the change gate", active.Name)
	if ev.Risk != nil {
		msg += fmt.Sprintf(" (risk %d)", *ev.Risk)
	}
	if len(ev.Blockers) > 0 {
		msg += ": " + strings.Join(ev.Blockers, "; ")
	}
	return msg
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
		// Mirrored while the crossing runs and cleared below when it ends, so
		// the Gate never shows a verdict that has stopped being true.
		gate.Status.Evidence = active.Status.Evidence
		return "Crossing", crossingMessage(active)
	}
	gate.Status.ActivePassage = ""
	gate.Status.Evidence = nil

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

	next := NextAuto(gate.Name, candidates, currentBundle(gate, bundles))
	if next == nil {
		if len(eligible) == 0 {
			return "Idle", waitingOn(candidates)
		}
		// Which of the two reasons it is. "Nothing newer" on a Gate that
		// stopped because a crossing failed sends an operator looking for a
		// missing Bundle, and the failure — the thing they need — is on an
		// object they have not thought to read (#121).
		if blocked := blockedNewest(gate.Name, eligible); blocked != nil {
			return "CrossingFailed", fmt.Sprintf(
				"%s did not cross and will not be retried automatically: %s — "+
					"retry with `hecate promote %s --bundle %s`",
				blocked.Name, blockedReason(gate.Name, blocked), gate.Name, blocked.Name)
		}
		return "Idle", "nothing newer than the current Bundle"
	}

	passage, err := r.startPassage(ctx, gate, next)
	if err != nil {
		return "PassageFailed", err.Error()
	}
	gate.Status.ActivePassage = passage.Name
	r.event(gate, corev1.EventTypeNormal, "PassageStarted", "Crossing",
		fmt.Sprintf("started Passage %s to cross Bundle %s", passage.Name, next.Name))
	return "Crossing", fmt.Sprintf("Passage %s is in progress", passage.Name)
}

// blockedNewest returns the newest eligible Bundle whose crossing of this Gate
// already failed, or nil.
func blockedNewest(gateName string, eligible []*v1alpha1.Bundle) *v1alpha1.Bundle {
	for _, b := range eligible {
		if b.WasBlockedBy(gateName) {
			return b
		}
	}
	return nil
}

// blockedReason is why the most recent crossing failed. Blocked is newest
// first, so the first match is the one worth reporting.
func blockedReason(gateName string, b *v1alpha1.Bundle) string {
	for _, c := range b.Status.Blocked {
		if c.Gate == gateName && c.Reason != "" {
			return c.Reason
		}
	}
	return "no reason recorded"
}

// verify runs the Gate's verifiers and reports whether the crossing is proven.
//
// All of them must pass, and the first that has not is what the Gate reports:
// a Gate that verified against three things and mentioned one is a Gate whose
// operator fixes one problem at a time and is surprised twice.
func (r *Reconciler) verify(ctx context.Context, gate *v1alpha1.Gate) (bool, string, error) {
	for _, v := range gate.Spec.Verify {
		verifier, ok := r.verifiers()[v.Uses]
		if !ok {
			// Named but unknown is a refusal, not a pass. Skipping it would
			// clear the Bundle on the strength of a verifier nobody ran.
			return false, "", fmt.Errorf("no verifier named %q", v.Uses)
		}
		var raw []byte
		if v.With != nil {
			raw = v.With.Raw
		}
		res, err := verifier.Verify(ctx, gate.Namespace, raw)
		if err != nil {
			return false, "", err
		}
		if !res.Verified {
			return false, res.Reason, nil
		}
	}
	return true, "", nil
}

// verifiers is the registry, defaulted so a Reconciler built without one still
// verifies rather than silently clearing everything.
func (r *Reconciler) verifiers() map[string]Verifier {
	if r.Verifiers != nil {
		return r.Verifiers
	}
	return map[string]Verifier{
		verify.VerifierFlagger: &verify.Flagger{Client: r.Client},
	}
}

// startPassage creates a Passage, copying the Gate's steps into it.
//
// Copied rather than referenced: editing a Gate must not retroactively change
// what an in-flight or completed Passage did.
func (r *Reconciler) startPassage(
	ctx context.Context, gate *v1alpha1.Gate, bundle *v1alpha1.Bundle,
) (*v1alpha1.Passage, error) {
	if gate.Spec.Passage == nil || len(gate.Spec.Passage.Steps) == 0 {
		return nil, errors.New("no passage steps defined")
	}

	passage := NewPassage(gate, bundle, ActorController)
	if err := r.Create(ctx, passage); err != nil {
		return nil, fmt.Errorf("creating Passage: %w", err)
	}
	return passage, nil
}

// ActorController is what a crossing the controller started records as having
// asked for it.
//
// Defined in pkg/passage because the steps have to recognise it as well — an
// automatic crossing has no human to record as the deployer — and pkg/passage
// is the package both sides can see.
const ActorController = passage.ActorController

// NewPassage builds the Passage that crosses a Bundle through a Gate.
//
// Exported so that a crossing requested by a human is constructed identically
// to one the controller starts: the labels other components select on, the
// copied steps and vars, and the generated name are all part of the contract,
// and a second construction elsewhere would drift from this one silently.
//
// Steps and vars are copied rather than referenced, so editing a Gate does not
// retroactively change what an in-flight or completed Passage did.
func NewPassage(gate *v1alpha1.Gate, bundle *v1alpha1.Bundle, actor string) *v1alpha1.Passage {
	return &v1alpha1.Passage{
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
			Vars:   gate.Spec.Vars,
			Actor:  actor,
		},
	}
}

// observePassages finds this Gate's Passages, records any completed crossing
// that has not been recorded yet, and returns the one still running (if any)
// together with the most recent finished one.
func (r *Reconciler) observePassages(
	ctx context.Context, gate *v1alpha1.Gate,
) (active, latest *v1alpha1.Passage, unverified string, err error) {
	var passages v1alpha1.PassageList
	if err := r.List(ctx, &passages,
		client.InNamespace(gate.Namespace),
		client.MatchingLabels{LabelGate: gate.Name},
	); err != nil {
		return nil, nil, "", fmt.Errorf("listing Passages: %w", err)
	}

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
		reason, err := r.recordOutcome(ctx, gate, newest)
		if err != nil {
			return nil, nil, "", err
		}
		unverified = reason
	}
	return active, newest, unverified, nil
}

// adopted returns the health checks a finished Passage asked the Gate to keep
// watching.
//
// Only from a *successful* crossing: a Passage that failed may have got no
// further than cloning a repository, and adopting whatever it managed to emit
// would have the Gate reporting on resources the crossing never reached.
func adopted(latest *v1alpha1.Passage) []v1alpha1.HealthCheck {
	if latest == nil || latest.Status.Phase != v1alpha1.PassageSucceeded {
		return nil
	}
	return latest.Status.Watch
}

// recordOutcome writes a finished Passage into the Gate's status and the
// Bundle's history, once.
//
// The Gate controller owns this write rather than the Passage controller: it is
// the component that knows whether verification passed, and one writer per
// field avoids two controllers racing over the same status.
func (r *Reconciler) recordOutcome(ctx context.Context, gate *v1alpha1.Gate, passage *v1alpha1.Passage) (unverified string, err error) {
	// `current` alone is not "already recorded" once verification exists.
	//
	// Current and history are written as soon as the crossing succeeds — that
	// is what is deployed, verified or not — but `cleared` waits for the
	// verdict. Returning here on current alone meant a canary that passed
	// after the first look never cleared the Bundle, because the second
	// reconcile short-circuited before the verifier ran. Caught against a real
	// Canary; every unit test passed.
	recorded := gate.Status.Current != nil && gate.Status.Current.Passage == passage.Name
	if recorded && passage.Status.Phase != v1alpha1.PassageSucceeded {
		return "", nil
	}

	var bundle v1alpha1.Bundle
	if err := r.Get(ctx, client.ObjectKey{Namespace: gate.Namespace, Name: passage.Spec.Bundle}, &bundle); err != nil {
		return "", client.IgnoreNotFound(err)
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
			// Newest first and capped, matching the Gate's history. The oldest
			// failure is the least useful one to keep: what an operator wants
			// is why it failed *this* time.
			bundle.Status.Blocked = append([]v1alpha1.GateCrossing{crossing}, bundle.Status.Blocked...)
			if len(bundle.Status.Blocked) > BlockedLimit {
				bundle.Status.Blocked = bundle.Status.Blocked[:BlockedLimit]
			}
			if err := r.Status().Update(ctx, &bundle); err != nil {
				return "", fmt.Errorf("recording blocked crossing on Bundle %s: %w", bundle.Name, err)
			}
		}
		r.event(gate, corev1.EventTypeWarning, "CrossingFailed", "Crossing",
			fmt.Sprintf("Bundle %s did not cross: %s", bundle.Name, passage.Status.Message))
		return "", nil
	}

	occupant := v1alpha1.GateOccupant{
		Bundle:    bundle.Name,
		Digest:    bundle.Spec.Digest,
		Passage:   passage.Name,
		EnteredAt: metav1.Time{Time: r.now()},
		Actor:     passage.Spec.Actor,
	}
	// Only on the first pass. A succeeded crossing is reconsidered on later
	// reconciles so a verdict that arrives late still clears the Bundle, and
	// re-appending here would grow the history once per reconcile — the
	// unbounded-list shape of #121, in the one list that was already capped.
	if !recorded {
		gate.Status.Current = &occupant
		gate.Status.History = append([]v1alpha1.GateOccupant{occupant}, gate.Status.History...)
		if len(gate.Status.History) > HistoryLimit {
			gate.Status.History = gate.Status.History[:HistoryLimit]
		}
	}

	// `cleared` means crossed *and* verified, and this write is the single
	// place verification gates (#21). A Gate that declares no verification
	// clears on a successful crossing, which is what it is asking for.
	//
	// A crossing that has not verified yet is not recorded as cleared and not
	// recorded as blocked either: it is still being judged, and the Gate says
	// so. Downstream Gates read `cleared`, so nothing is admitted on the
	// strength of a canary that is still running — or one that rolled back,
	// which leaves a perfectly healthy Deployment serving the previous
	// version. That divergence is the whole reason verification is not health.
	verified, unverified, err := r.verify(ctx, gate)
	if err != nil {
		return "", fmt.Errorf("verifying the crossing of %s: %w", bundle.Name, err)
	}
	if !verified {
		// Returned rather than written as a condition here: advance sets the
		// Ready condition after this runs and would overwrite it, leaving
		// "Idle: already in this Gate" on a Gate whose canary just rolled
		// back — true, useless, and the same shape of misleading status as
		// #121.
		return unverified, nil
	}
	//
	// **Not capped, unlike Blocked above, and the asymmetry is deliberate.**
	// This list is load-bearing: HasCleared is the upstream-ordering check, so
	// evicting an entry would silently make a Bundle ineligible for a Gate it
	// had genuinely cleared — a correctness bug traded for a size one.
	//
	// It is also bounded in practice, which Blocked was not. A Bundle already
	// in a Gate is ineligible for it, so re-promoting adds nothing; only a
	// rollback that moves the Gate away and back appends another entry.
	// Measured: five promotions in a row produced one entry, and three rollback
	// cycles produced four, at ~145 bytes each. Reaching etcd's object limit
	// needs on the order of ten thousand rollback cycles on one Bundle, one
	// human action at a time. #121's list grew three times a minute on its own.
	if !hasCrossing(bundle.Status.Cleared, passage.Name) {
		bundle.Status.Cleared = append(bundle.Status.Cleared, crossing)
		if err := r.Status().Update(ctx, &bundle); err != nil {
			return "", fmt.Errorf("recording crossing on Bundle %s: %w", bundle.Name, err)
		}
	}

	r.event(gate, corev1.EventTypeNormal, "BundleCrossed", "Crossing",
		fmt.Sprintf("Bundle %s crossed via Passage %s", bundle.Name, passage.Name))
	return "", nil
}

// assess reports the Gate's health, over both what the operator declared and
// what the last successful crossing asked it to watch.
//
// The adopted checks are what stop a Gate going blind: a `flux-wait` step
// already names the resources it waited for, so restating them in
// `spec.watch` would be duplication that drifts.
// currentHealth reads a report's status, treating "no report yet" as the empty
// status so a first observation is not mistaken for a transition.
func currentHealth(report *v1alpha1.HealthReport) v1alpha1.Health {
	if report == nil {
		return ""
	}
	return report.Status
}

// announceHealth publishes the health gauge, and records an Event when the
// state actually changes.
//
// On the transition only: a Gate is reconciled every minute, and an Event per
// reconcile would drown notification-controller and make the useful ones
// invisible. A Gate going Degraded after a crossing is the alert worth having.
func (r *Reconciler) announceHealth(gate *v1alpha1.Gate, previous *v1alpha1.HealthReport) {
	was, now := currentHealth(previous), currentHealth(gate.Status.Health)
	metrics.RecordGateHealth(gate.Namespace, gate.Name, now)

	if now == was || now == "" {
		return
	}
	// A Gate leaving Degraded ends the only outage Hecate can see. The start
	// comes from the previous report rather than from process memory, so it is
	// still right after a restart or a leader-election handover — which is
	// exactly when an outage is most likely to have begun under a different
	// process (D43).
	if was == v1alpha1.HealthDegraded && previous.Since != nil {
		metrics.RecordGateRecovered(gate.Namespace, gate.Name,
			r.now().Sub(previous.Since.Time).Seconds())
	}
	// First observation is not a transition worth alerting on.
	if was == "" {
		return
	}

	eventType := corev1.EventTypeNormal
	if now == v1alpha1.HealthDegraded || now == v1alpha1.HealthUnknown {
		eventType = corev1.EventTypeWarning
	}
	message := fmt.Sprintf("health changed from %s to %s", was, now)
	if issues := gate.Status.Health.Issues; len(issues) > 0 {
		message = fmt.Sprintf("%s: %s", message, issues[0])
	}
	r.event(gate, eventType, "HealthChanged", "Monitoring", message)
}

func (r *Reconciler) assess(
	ctx context.Context, gate *v1alpha1.Gate, adopted []v1alpha1.HealthCheck,
) *v1alpha1.HealthReport {
	if r.Health == nil {
		return &v1alpha1.HealthReport{Status: v1alpha1.HealthNotApplicable}
	}
	checks := append(append([]v1alpha1.HealthCheck{}, gate.Spec.Watch...), adopted...)
	report := r.Health.Assess(ctx, gate.Namespace, gate.Name, checks)
	now := metav1.Time{Time: r.now()}
	report.ObservedAt = &now
	// Since tracks when the status *changed*, not when it was last looked at,
	// so it is carried forward across every reconcile that finds the same
	// status. Restamping it each time would make every Gate permanently
	// "Degraded for 0 seconds".
	report.Since = &now
	if prev := gate.Status.Health; prev != nil && prev.Status == report.Status && prev.Since != nil {
		report.Since = prev.Since
	}
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

// ownStatusWrites keeps a Gate from being woken by its own status write.
//
// Every reconcile stamps status.health.observedAt with the current time, so the
// write always differs, so it triggers the watch, so the Gate reconciles again.
// The same shape as the Beacon's lastPolled, and hidden by the same accident:
// metav1.Time has one-second resolution, so two reconciles inside one second
// write an identical value and the loop stops. A Gate reconciles in well under
// a second today — but a health check is a network call, and the day one takes
// longer than a second is the day this becomes a hot loop against whatever is
// already slow.
//
// Fixed before it bites rather than after, because the Passage controller and
// the Beacon both reached this state on their own and neither was visible to
// the unit suite.
//
// Spec changes and reconcile requests still wake it; the interval requeue is
// what makes a Gate notice the world otherwise.
func ownStatusWrites() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() ||
				v1alpha1.ReconcileRequestedAt(e.ObjectOld.GetAnnotations()) !=
					v1alpha1.ReconcileRequestedAt(e.ObjectNew.GetAnnotations())
		},
	}
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

// event records a Kubernetes event against the Gate. See the Beacon's
// equivalent for what `action` is for.
func (r *Reconciler) event(gate *v1alpha1.Gate, eventType, reason, action, message string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(gate, nil, eventType, reason, action, "%s", message)
}

// SetupWithManager registers the controller.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		// The new events API changes Eventf's signature — it adds an `action`
		// argument — so this is a user-visible change to the events people alert
		// on rather than a rename. Tracked in #116; the old API is deprecated but
		// not removed, and controller-runtime suppresses it the same way itself.
		r.Recorder = mgr.GetEventRecorder("gate-controller")
	}
	// Watches rather than Owns: Passages carry no owner reference, because an
	// owner reference would cascade-delete the record of every crossing when a
	// Gate is removed. Mapping by spec.gate gives the same immediate wake-up
	// without that cost — otherwise a finished crossing would sit unrecorded
	// until the next flat-interval reconcile.
	return ctrl.NewControllerManagedBy(mgr).
		// The predicate is on this watch alone, not WithEventFilter.
		//
		// A Gate learns that a crossing finished from the *status* of a
		// Passage, so filtering status changes globally would leave every
		// crossing hanging until the next interval — which is the mistake the
		// convenient spelling invites.
		For(&v1alpha1.Gate{}, builder.WithPredicates(ownStatusWrites())).
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

// Verifier answers whether a crossing actually worked, as opposed to whether
// what it deployed is running. See pkg/verify.
type Verifier interface {
	Name() string
	Verify(ctx context.Context, namespace string, config []byte) (verify.Result, error)
}
