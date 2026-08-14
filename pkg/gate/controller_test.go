package gate

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/health"
	"github.com/olafkfreund/hecate/pkg/metrics"
	"github.com/olafkfreund/hecate/pkg/verify"
)

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// autoGate is a Gate that crosses eligible Bundles without being asked.
func autoGate(name string, admissions ...v1alpha1.Admission) *v1alpha1.Gate {
	g := gateAdmitting(name, admissions...)
	g.Spec.Auto = true
	g.Spec.Passage = &v1alpha1.PassageTemplate{
		Steps: []v1alpha1.Step{{Uses: "flux-wait"}},
	}
	return g
}

func newReconciler(t *testing.T, objs ...client.Object) (*Reconciler, client.Client, *record.FakeRecorder) {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(scheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.Gate{}, &v1alpha1.Bundle{}, &v1alpha1.Passage{}).
		Build()
	rec := record.NewFakeRecorder(20)
	return &Reconciler{
		Client:   c,
		Recorder: rec,
		Now:      func() time.Time { return base },
	}, c, rec
}

func reconcileGate(t *testing.T, r *Reconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: "acme"},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

func getGate(t *testing.T, c client.Client, name string) *v1alpha1.Gate {
	t.Helper()
	var g v1alpha1.Gate
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "acme"}, &g); err != nil {
		t.Fatal(err)
	}
	return &g
}

func listPassages(t *testing.T, c client.Client) []v1alpha1.Passage {
	t.Helper()
	var l v1alpha1.PassageList
	if err := c.List(context.Background(), &l, client.InNamespace("acme")); err != nil {
		t.Fatal(err)
	}
	return l.Items
}

func ready(t *testing.T, g *v1alpha1.Gate) *metav1.Condition {
	t.Helper()
	return meta.FindStatusCondition(g.Status.Conditions, v1alpha1.ConditionReady)
}

func TestAutoGateStartsAPassage(t *testing.T) {
	g := autoGate("dev", admits("podinfo"))
	b := bundle("b1", "podinfo", 0)
	r, c, rec := newReconciler(t, g, &b)

	reconcileGate(t, r, "dev")

	passages := listPassages(t, c)
	if len(passages) != 1 {
		t.Fatalf("got %d Passages, want 1", len(passages))
	}
	p := passages[0]
	if p.Spec.Gate != "dev" || p.Spec.Bundle != "b1" {
		t.Errorf("Passage targets %s/%s, want dev/b1", p.Spec.Gate, p.Spec.Bundle)
	}
	// Steps are copied, not referenced: editing the Gate must not rewrite history.
	if len(p.Spec.Steps) != 1 || p.Spec.Steps[0].Uses != "flux-wait" {
		t.Errorf("Passage did not copy the Gate's steps: %+v", p.Spec.Steps)
	}
	if p.Labels[LabelGate] != "dev" || p.Labels[LabelBundle] != "b1" {
		t.Errorf("Passage labels = %v", p.Labels)
	}
	if got := getGate(t, c, "dev").Status.ActivePassage; got != p.Name {
		t.Errorf("activePassage = %q, want %q", got, p.Name)
	}

	select {
	case e := <-rec.Events:
		if !strings.Contains(e, "PassageStarted") {
			t.Errorf("event = %q", e)
		}
	default:
		t.Error("starting a Passage should record an event")
	}
}

// One crossing at a time. A second Passage while one is running would have two
// writers racing over the same environment.
func TestOnlyOnePassageAtATime(t *testing.T) {
	g := autoGate("dev", admits("podinfo"))
	b1, b2 := bundle("b1", "podinfo", 0), bundle("b2", "podinfo", 10)
	r, c, _ := newReconciler(t, g, &b1, &b2)

	reconcileGate(t, r, "dev")
	reconcileGate(t, r, "dev")
	reconcileGate(t, r, "dev")

	if n := len(listPassages(t, c)); n != 1 {
		t.Fatalf("got %d Passages, want exactly 1 while one is in flight", n)
	}
	if cond := ready(t, getGate(t, c, "dev")); cond.Reason != "Crossing" {
		t.Errorf("reason = %q, want Crossing", cond.Reason)
	}
}

// A Gate without `auto` lists what could cross but never starts it itself.
func TestManualGateListsButDoesNotCross(t *testing.T) {
	g := gateAdmitting("production", admits("podinfo"))
	b := bundle("b1", "podinfo", 0)
	r, c, _ := newReconciler(t, g, &b)

	reconcileGate(t, r, "production")

	if n := len(listPassages(t, c)); n != 0 {
		t.Fatalf("a manual Gate created %d Passages, want 0", n)
	}
	got := getGate(t, c, "production")
	if len(got.Status.Eligible) != 1 || got.Status.Eligible[0] != "b1" {
		t.Errorf("eligible = %v, want [b1]", got.Status.Eligible)
	}
	if cond := ready(t, got); !strings.Contains(cond.Message, "must be requested") {
		t.Errorf("message = %q, should say a crossing must be requested", cond.Message)
	}
}

func TestSucceededPassageIsRecorded(t *testing.T) {
	g := autoGate("staging", admits("podinfo"))
	b := bundle("b1", "podinfo", 0)
	r, c, _ := newReconciler(t, g, &b)

	reconcileGate(t, r, "staging")
	p := listPassages(t, c)[0]

	// The Passage controller finishes it.
	p.Status.Phase = v1alpha1.PassageSucceeded
	if err := c.Status().Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	reconcileGate(t, r, "staging")

	got := getGate(t, c, "staging")
	if got.Status.Current == nil || got.Status.Current.Bundle != "b1" {
		t.Fatalf("current = %+v, want b1", got.Status.Current)
	}
	if len(got.Status.History) != 1 {
		t.Errorf("history = %d entries, want 1", len(got.Status.History))
	}
	if got.Status.ActivePassage != "" {
		t.Errorf("activePassage should be cleared once the Passage finishes, got %q", got.Status.ActivePassage)
	}

	// The durable record is on the Bundle — this is what downstream Gates read.
	var updated v1alpha1.Bundle
	if err := c.Get(context.Background(), types.NamespacedName{Name: "b1", Namespace: "acme"}, &updated); err != nil {
		t.Fatal(err)
	}
	if !updated.HasCleared("staging") {
		t.Error("a successful crossing must be recorded on the Bundle as cleared")
	}
}

func TestRecordingACrossingIsIdempotent(t *testing.T) {
	g := autoGate("staging", admits("podinfo"))
	b := bundle("b1", "podinfo", 0)
	r, c, _ := newReconciler(t, g, &b)

	reconcileGate(t, r, "staging")
	p := listPassages(t, c)[0]
	p.Status.Phase = v1alpha1.PassageSucceeded
	if err := c.Status().Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		reconcileGate(t, r, "staging")
	}

	got := getGate(t, c, "staging")
	if len(got.Status.History) != 1 {
		t.Errorf("history = %d entries after 3 reconciles, want 1", len(got.Status.History))
	}
	var updated v1alpha1.Bundle
	if err := c.Get(context.Background(), types.NamespacedName{Name: "b1", Namespace: "acme"}, &updated); err != nil {
		t.Fatal(err)
	}
	if len(updated.Status.Cleared) != 1 {
		t.Errorf("bundle cleared = %d entries, want 1", len(updated.Status.Cleared))
	}
}

func TestFailedPassageIsRecordedAsBlocked(t *testing.T) {
	g := autoGate("production", admits("podinfo"))
	b := bundle("b1", "podinfo", 0)
	r, c, _ := newReconciler(t, g, &b)

	reconcileGate(t, r, "production")
	p := listPassages(t, c)[0]
	p.Status.Phase = v1alpha1.PassageFailed
	p.Status.Message = "flux never converged"
	if err := c.Status().Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	reconcileGate(t, r, "production")

	got := getGate(t, c, "production")
	if got.Status.Current != nil {
		t.Errorf("a failed crossing must not become current, got %+v", got.Status.Current)
	}

	var updated v1alpha1.Bundle
	if err := c.Get(context.Background(), types.NamespacedName{Name: "b1", Namespace: "acme"}, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.HasCleared("production") {
		t.Error("a failed crossing must not be recorded as cleared")
	}
	if len(updated.Status.Blocked) != 1 || updated.Status.Blocked[0].Reason != "flux never converged" {
		t.Errorf("blocked = %+v, want one entry carrying the failure reason", updated.Status.Blocked)
	}
}

// A list inside a status subresource that only grows eventually makes the
// object exceed etcd's size limit, at which point nothing can be recorded
// against it at all — and a Bundle is the object a promotion cannot proceed
// without. This one reached 733KB of identical DNS failures once (#121).
func TestBlockedIsCappedAndNewestFirst(t *testing.T) {
	g := autoGate("production", admits("podinfo"))
	b := bundle("b1", "podinfo", 0)
	// Pre-loaded past the cap, as a Bundle that had been failing for a while
	// would be.
	for i := range BlockedLimit + 5 {
		b.Status.Blocked = append(b.Status.Blocked, v1alpha1.GateCrossing{
			Gate: "production", Passage: fmt.Sprintf("old-%d", i), Reason: "an older failure",
		})
	}
	r, c, _ := newReconciler(t, g, &b)

	// Promote explicitly: an auto Gate no longer retries a blocked Bundle.
	p := NewPassage(g, &b, "olaf@acme.example")
	if err := c.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	p.Status.Phase = v1alpha1.PassageFailed
	p.Status.Message = "the newest failure"
	if err := c.Status().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	reconcileGate(t, r, "production")

	var updated v1alpha1.Bundle
	if err := c.Get(context.Background(), types.NamespacedName{Name: "b1", Namespace: "acme"}, &updated); err != nil {
		t.Fatal(err)
	}
	if n := len(updated.Status.Blocked); n > BlockedLimit {
		t.Errorf("blocked has %d entries, cap is %d — the list grows without limit", n, BlockedLimit)
	}
	// Newest first: the oldest failure is the least useful to keep, and an
	// operator wants to know why it failed *this* time.
	if got := updated.Status.Blocked[0].Reason; got != "the newest failure" {
		t.Errorf("blocked[0] = %q, want the most recent failure", got)
	}
}

// The end-to-end property: a Bundle cannot reach production without clearing
// staging first.
func TestPipelineOrderIsEnforced(t *testing.T) {
	staging := autoGate("staging", admits("podinfo"))
	prod := autoGate("production", admits("podinfo", "staging"))
	b := bundle("b1", "podinfo", 0)
	r, c, _ := newReconciler(t, staging, prod, &b)

	// Production refuses while staging is uncleared.
	reconcileGate(t, r, "production")
	if n := len(listPassages(t, c)); n != 0 {
		t.Fatalf("production started %d Passages before staging cleared, want 0", n)
	}
	if cond := ready(t, getGate(t, c, "production")); !strings.Contains(cond.Message, "has not cleared staging") {
		t.Errorf("message = %q, should explain the missing upstream", cond.Message)
	}

	// Staging crosses and succeeds.
	reconcileGate(t, r, "staging")
	p := listPassages(t, c)[0]
	p.Status.Phase = v1alpha1.PassageSucceeded
	if err := c.Status().Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	reconcileGate(t, r, "staging")

	// Now production admits it.
	reconcileGate(t, r, "production")
	var prodPassages int
	for _, p := range listPassages(t, c) {
		if p.Spec.Gate == "production" {
			prodPassages++
		}
	}
	if prodPassages != 1 {
		t.Errorf("production started %d Passages after staging cleared, want 1", prodPassages)
	}
}

func TestApprovalBlocksAutoCrossing(t *testing.T) {
	a := admits("podinfo")
	a.RequireApproval = true
	g := autoGate("production", a)
	b := bundle("b1", "podinfo", 0)
	r, c, _ := newReconciler(t, g, &b)

	reconcileGate(t, r, "production")
	if n := len(listPassages(t, c)); n != 0 {
		t.Fatalf("crossed without approval, got %d Passages", n)
	}
	if cond := ready(t, getGate(t, c, "production")); !strings.Contains(cond.Message, "awaiting approval") {
		t.Errorf("message = %q", cond.Message)
	}

	// A human approves.
	var updated v1alpha1.Bundle
	if err := c.Get(context.Background(), types.NamespacedName{Name: "b1", Namespace: "acme"}, &updated); err != nil {
		t.Fatal(err)
	}
	updated.Status.ApprovedFor = []v1alpha1.BundleApproval{{Gate: "production", Actor: "olaf@acme.example"}}
	if err := c.Status().Update(context.Background(), &updated); err != nil {
		t.Fatal(err)
	}

	reconcileGate(t, r, "production")
	if n := len(listPassages(t, c)); n != 1 {
		t.Errorf("got %d Passages after approval, want 1", n)
	}
}

func TestClosedWindowQueuesRatherThanFails(t *testing.T) {
	g := autoGate("production", admits("podinfo"))
	// Open weekday mornings only; the clock is fixed at 12:00 Monday, and the
	// window closes at 10:00.
	g.Spec.Windows = []v1alpha1.Window{win("0 9 * * 1-5", 1*time.Hour)}
	b := bundle("b1", "podinfo", 0)
	r, c, _ := newReconciler(t, g, &b)

	reconcileGate(t, r, "production")

	if n := len(listPassages(t, c)); n != 0 {
		t.Fatalf("crossed outside the window, got %d Passages", n)
	}
	got := getGate(t, c, "production")
	cond := ready(t, got)
	if cond.Reason != "WindowClosed" {
		t.Errorf("reason = %q, want WindowClosed", cond.Reason)
	}
	// The Bundle stays eligible — it is queued, not rejected.
	if len(got.Status.Eligible) != 1 {
		t.Errorf("eligible = %v, want the Bundle to remain queued", got.Status.Eligible)
	}
}

func TestSuspendStopsEverything(t *testing.T) {
	g := autoGate("dev", admits("podinfo"))
	g.Spec.Suspend = true
	b := bundle("b1", "podinfo", 0)
	r, c, _ := newReconciler(t, g, &b)

	res := reconcileGate(t, r, "dev")

	if n := len(listPassages(t, c)); n != 0 {
		t.Errorf("suspended Gate started %d Passages, want 0", n)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("suspended Gate should not requeue, got %s", res.RequeueAfter)
	}
	if cond := ready(t, getGate(t, c, "dev")); cond.Reason != "Suspended" {
		t.Errorf("reason = %q, want Suspended", cond.Reason)
	}
}

func TestHistoryIsCapped(t *testing.T) {
	g := autoGate("dev", admits("podinfo"))
	g.Status.Current = &v1alpha1.GateOccupant{Bundle: "seed", Passage: "seed-passage"}
	for i := 0; i < HistoryLimit+5; i++ {
		g.Status.History = append(g.Status.History, v1alpha1.GateOccupant{Bundle: "old"})
	}
	b := bundle("b1", "podinfo", 0)
	r, c, _ := newReconciler(t, g, &b)

	reconcileGate(t, r, "dev")
	p := listPassages(t, c)[0]
	p.Status.Phase = v1alpha1.PassageSucceeded
	if err := c.Status().Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	reconcileGate(t, r, "dev")

	if n := len(getGate(t, c, "dev").Status.History); n != HistoryLimit {
		t.Errorf("history = %d entries, want it capped at %d", n, HistoryLimit)
	}
}

func TestGateWithoutStepsReportsInsteadOfPanicking(t *testing.T) {
	g := gateAdmitting("dev", admits("podinfo"))
	g.Spec.Auto = true // auto, but no passage template
	b := bundle("b1", "podinfo", 0)
	r, c, _ := newReconciler(t, g, &b)

	reconcileGate(t, r, "dev")

	cond := ready(t, getGate(t, c, "dev"))
	if cond.Reason != "PassageFailed" || !strings.Contains(cond.Message, "no passage steps") {
		t.Errorf("want a clear complaint about missing steps, got %+v", cond)
	}
}

func TestMissingGateIsNotAnError(t *testing.T) {
	r, _, _ := newReconciler(t)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "gone", Namespace: "acme"},
	}); err != nil {
		t.Errorf("a deleted Gate should reconcile cleanly, got %v", err)
	}
}

func TestHealthIsNotApplicableWithoutARegistry(t *testing.T) {
	g := gateAdmitting("dev", admits("podinfo"))
	r, c, _ := newReconciler(t, g)

	reconcileGate(t, r, "dev")

	h := getGate(t, c, "dev").Status.Health
	if h == nil || h.Status != v1alpha1.HealthNotApplicable {
		t.Errorf("health = %+v, want NotApplicable reported rather than omitted", h)
	}
}

// recordingChecker captures what it was asked to assess.
type recordingChecker struct{ seen []health.Request }

func (c *recordingChecker) Name() string { return "flux" }
func (c *recordingChecker) Check(_ context.Context, req health.Request) v1alpha1.HealthReport {
	c.seen = append(c.seen, req)
	return v1alpha1.HealthReport{Status: v1alpha1.HealthHealthy}
}

// A `flux-wait` step already names the resources it waited on. The Gate must
// adopt them, or it goes blind the moment the Passage ends.
func TestGateAdoptsWatchFromSucceededPassage(t *testing.T) {
	g := autoGate("staging", admits("podinfo"))
	b := bundle("b1", "podinfo", 0)
	r, c, _ := newReconciler(t, g, &b)

	checker := &recordingChecker{}
	reg := health.NewRegistry()
	reg.MustRegister(checker)
	r.Health = reg

	reconcileGate(t, r, "staging")
	p := listPassages(t, c)[0]
	p.Status.Phase = v1alpha1.PassageSucceeded
	p.Status.Watch = []v1alpha1.HealthCheck{{
		Uses: "flux",
		With: &apiextensionsv1.JSON{Raw: []byte(`{"resources":[{"kind":"Kustomization","name":"podinfo"}]}`)},
	}}
	if err := c.Status().Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}

	checker.seen = nil
	reconcileGate(t, r, "staging")

	if len(checker.seen) != 1 {
		t.Fatalf("checker invoked %d times, want 1 — the Passage's watch was not adopted", len(checker.seen))
	}
	if !strings.Contains(string(checker.seen[0].Config), "podinfo") {
		t.Errorf("adopted config = %s, want the Kustomization the step waited on", checker.seen[0].Config)
	}
	if got := getGate(t, c, "staging").Status.Health; got == nil || got.Status != v1alpha1.HealthHealthy {
		t.Errorf("health = %+v, want Healthy", got)
	}
}

// A crossing that failed may not have got past cloning a repository; adopting
// whatever it emitted would report on resources it never reached.
func TestGateIgnoresWatchFromFailedPassage(t *testing.T) {
	g := autoGate("staging", admits("podinfo"))
	b := bundle("b1", "podinfo", 0)
	r, c, _ := newReconciler(t, g, &b)

	checker := &recordingChecker{}
	reg := health.NewRegistry()
	reg.MustRegister(checker)
	r.Health = reg

	reconcileGate(t, r, "staging")
	p := listPassages(t, c)[0]
	p.Status.Phase = v1alpha1.PassageFailed
	p.Status.Watch = []v1alpha1.HealthCheck{{Uses: "flux"}}
	if err := c.Status().Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}

	checker.seen = nil
	reconcileGate(t, r, "staging")

	if len(checker.seen) != 0 {
		t.Errorf("adopted a watch from a failed crossing: %+v", checker.seen)
	}
}

// A Gate going Degraded after a crossing is the alert worth having — but only
// on the transition. A Gate reconciles every minute, and an event per reconcile
// would drown notification-controller and hide the useful ones.
func TestHealthTransitionRecordsOneEvent(t *testing.T) {
	g := gateAdmitting("production", admits("podinfo"))
	r, c, rec := newReconciler(t, g)

	swing := &swingingChecker{status: v1alpha1.HealthHealthy}
	reg := health.NewRegistry()
	reg.MustRegister(swing)
	r.Health = reg
	g.Spec.Watch = []v1alpha1.HealthCheck{{Uses: "flux"}}
	if err := c.Update(context.Background(), g); err != nil {
		t.Fatal(err)
	}

	// First observation establishes a baseline; it is not a transition.
	reconcileGate(t, r, "production")
	drain(rec)

	// Steady state must stay quiet.
	reconcileGate(t, r, "production")
	if e := next(rec); e != "" {
		t.Errorf("unchanged health recorded an event: %q", e)
	}

	// Now it breaks.
	swing.status = v1alpha1.HealthDegraded
	reconcileGate(t, r, "production")

	e := next(rec)
	if !strings.Contains(e, "HealthChanged") || !strings.Contains(e, "Degraded") {
		t.Errorf("event = %q, want a HealthChanged event naming Degraded", e)
	}
	if !strings.Contains(e, "Warning") {
		t.Errorf("event = %q, want Warning severity for a Degraded Gate", e)
	}

	// And staying broken must not re-fire.
	reconcileGate(t, r, "production")
	if e := next(rec); e != "" {
		t.Errorf("still-Degraded recorded a repeat event: %q", e)
	}
}

type swingingChecker struct{ status v1alpha1.Health }

func (s *swingingChecker) Name() string { return "flux" }
func (s *swingingChecker) Check(context.Context, health.Request) v1alpha1.HealthReport {
	return v1alpha1.HealthReport{Status: s.status}
}

func drain(rec *record.FakeRecorder) {
	for {
		select {
		case <-rec.Events:
		default:
			return
		}
	}
}

func next(rec *record.FakeRecorder) string {
	select {
	case e := <-rec.Events:
		return e
	default:
		return ""
	}
}

// "Degraded for 40 minutes" is a different fact from "Degraded, checked 20
// seconds ago", and only the first tells you whether to worry. Since must
// therefore be carried forward while the status holds, not restamped every
// reconcile.
func TestHealthSinceMarksTheChangeNotTheCheck(t *testing.T) {
	g := gateAdmitting("production", admits("podinfo"))
	r, c, _ := newReconciler(t, g)

	now := base
	r.Now = func() time.Time { return now }
	swing := &swingingChecker{status: v1alpha1.HealthHealthy}
	reg := health.NewRegistry()
	reg.MustRegister(swing)
	r.Health = reg
	g.Spec.Watch = []v1alpha1.HealthCheck{{Uses: "flux"}}
	if err := c.Update(context.Background(), g); err != nil {
		t.Fatal(err)
	}

	reconcileGate(t, r, "production")
	first := getGate(t, c, "production").Status.Health.Since
	if first == nil {
		t.Fatal("the first assessment must record when the status began")
	}

	// Same status, later check. Since must not move; ObservedAt must.
	now = base.Add(10 * time.Minute)
	reconcileGate(t, r, "production")
	after := getGate(t, c, "production").Status.Health
	if !after.Since.Equal(first) {
		t.Errorf("Since moved from %s to %s without the status changing",
			first, after.Since)
	}
	if !after.ObservedAt.Time.Equal(now) {
		t.Errorf("ObservedAt = %s, want the time of the check (%s)", after.ObservedAt, now)
	}

	// A real change restamps it.
	now = base.Add(20 * time.Minute)
	swing.status = v1alpha1.HealthDegraded
	reconcileGate(t, r, "production")
	if got := getGate(t, c, "production").Status.Health.Since; !got.Time.Equal(now) {
		t.Errorf("Since = %s after a real change, want %s", got, now)
	}
}

// Time-to-restore is measured from status, not from process memory, so it
// survives the restart or leader handover that an outage is most likely to
// straddle. This drives a whole Degraded period through a *second* Reconciler
// to prove the surviving controller still gets the duration right.
func TestGateRecoveryIsMeasuredAcrossARestart(t *testing.T) {
	metrics.GateDegraded.Reset()

	g := gateAdmitting("production", admits("podinfo"))
	r, c, _ := newReconciler(t, g)

	now := base
	r.Now = func() time.Time { return now }
	swing := &swingingChecker{status: v1alpha1.HealthDegraded}
	reg := health.NewRegistry()
	reg.MustRegister(swing)
	r.Health = reg
	g.Spec.Watch = []v1alpha1.HealthCheck{{Uses: "flux"}}
	if err := c.Update(context.Background(), g); err != nil {
		t.Fatal(err)
	}

	// The Gate breaks under one controller.
	reconcileGate(t, r, "production")

	// That controller goes away. A fresh one, with no memory of anything,
	// picks the Gate up half an hour later and sees it recover.
	successor := &Reconciler{Client: c, Health: reg, Now: func() time.Time { return now }}
	now = base.Add(30 * time.Minute)
	swing.status = v1alpha1.HealthHealthy
	reconcileGate(t, successor, "production")

	count, sum := degradedObservations(t)
	if count != 1 {
		t.Fatalf("recovery observations = %d, want 1", count)
	}
	if sum != 1800 {
		t.Errorf("degraded for %.0fs, want 1800 — the duration must come from "+
			"status.health.since, not from the controller's memory", sum)
	}
}

// A Gate that is still Degraded has not recovered, and recording it would
// report an outage as over while it is still happening.
func TestGateStillDegradedRecordsNoRecovery(t *testing.T) {
	metrics.GateDegraded.Reset()

	g := gateAdmitting("production", admits("podinfo"))
	r, c, _ := newReconciler(t, g)
	swing := &swingingChecker{status: v1alpha1.HealthDegraded}
	reg := health.NewRegistry()
	reg.MustRegister(swing)
	r.Health = reg
	g.Spec.Watch = []v1alpha1.HealthCheck{{Uses: "flux"}}
	if err := c.Update(context.Background(), g); err != nil {
		t.Fatal(err)
	}

	reconcileGate(t, r, "production")
	reconcileGate(t, r, "production")

	if count, _ := degradedObservations(t); count != 0 {
		t.Errorf("a Gate that is still Degraded recorded %d recoveries", count)
	}
}

func degradedObservations(t *testing.T) (uint64, float64) {
	t.Helper()
	ch := make(chan prometheus.Metric, 32)
	metrics.GateDegraded.Collect(ch)
	close(ch)
	var count uint64
	var sum float64
	for m := range ch {
		var got dto.Metric
		if err := m.Write(&got); err != nil {
			t.Fatal(err)
		}
		count += got.GetHistogram().GetSampleCount()
		sum += got.GetHistogram().GetSampleSum()
	}
	return count, sum
}

// The verdict has to be on the Gate, because that is the object an operator
// looks at when a promotion is not moving. Reaching it through the Passage's
// step output means knowing which Passage and which step first.
func TestAHeldChangeGateIsVisibleOnTheGate(t *testing.T) {
	g := autoGate("staging", admits("podinfo"))
	b := bundle("b1", "podinfo", 0)
	r, c, _ := newReconciler(t, g, &b)

	reconcileGate(t, r, "staging")
	p := listPassages(t, c)[0]

	risk := int32(62)
	p.Status.Phase = v1alpha1.PassageRunning
	p.Status.Evidence = &v1alpha1.EvidenceRef{
		Verdict: "hold", Risk: &risk, Blockers: []string{"no approver recorded"},
	}
	if err := c.Status().Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	reconcileGate(t, r, "staging")

	got := getGate(t, c, "staging")
	if got.Status.Evidence == nil {
		t.Fatal("the Gate says nothing about why the crossing is not finishing")
	}
	if got.Status.Evidence.Verdict != "hold" || *got.Status.Evidence.Risk != 62 {
		t.Errorf("evidence = %+v", got.Status.Evidence)
	}
	// "Passage x is in progress" is equally true of a crossing thirty seconds
	// old and one that has waited six hours for a human.
	cond := ready(t, got)
	for _, want := range []string{"held by the change gate", "62", "no approver recorded"} {
		if !strings.Contains(cond.Message, want) {
			t.Errorf("condition does not say %q: %s", want, cond.Message)
		}
	}
}

// A verdict that outlives its crossing is worse than none: it reads as current.
func TestTheGatesVerdictIsClearedWhenTheCrossingEnds(t *testing.T) {
	g := autoGate("staging", admits("podinfo"))
	b := bundle("b1", "podinfo", 0)
	r, c, _ := newReconciler(t, g, &b)

	reconcileGate(t, r, "staging")
	p := listPassages(t, c)[0]
	risk := int32(62)
	p.Status.Phase = v1alpha1.PassageRunning
	p.Status.Evidence = &v1alpha1.EvidenceRef{Verdict: "hold", Risk: &risk}
	if err := c.Status().Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	reconcileGate(t, r, "staging")
	if getGate(t, c, "staging").Status.Evidence == nil {
		t.Fatal("the verdict was never recorded, so clearing it proves nothing")
	}

	p.Status.Phase = v1alpha1.PassageSucceeded
	if err := c.Status().Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	reconcileGate(t, r, "staging")

	if ev := getGate(t, c, "staging").Status.Evidence; ev != nil {
		t.Errorf("evidence = %+v after the crossing finished — a stale verdict presented "+
			"as the current one", ev)
	}
}

// The verdict most crossings carry is "approve", so this is the message an
// operator sees on an ordinary day — and a passing change gate reported as a
// held one teaches people to ignore the message on the held ones too.
func TestAnApprovedVerdictDoesNotReadAsHeld(t *testing.T) {
	g := autoGate("staging", admits("podinfo"))
	b := bundle("b1", "podinfo", 0)
	r, c, _ := newReconciler(t, g, &b)

	reconcileGate(t, r, "staging")
	p := listPassages(t, c)[0]

	risk := int32(5)
	p.Status.Phase = v1alpha1.PassageRunning
	p.Status.Evidence = &v1alpha1.EvidenceRef{Verdict: "approve", Risk: &risk}
	if err := c.Status().Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	reconcileGate(t, r, "staging")

	cond := ready(t, getGate(t, c, "staging"))
	if strings.Contains(cond.Message, "held") {
		t.Errorf("the change gate approved and the Gate reports: %s", cond.Message)
	}
	if !strings.Contains(cond.Message, "in progress") {
		t.Errorf("condition does not say the crossing is running: %s", cond.Message)
	}
}

// A Gate that stopped because a crossing failed must say so. Reporting
// "nothing newer than the current Bundle" sends an operator looking for a
// missing Bundle, while the failure sits on an object they have not thought to
// read (#121).
func TestAGateSaysWhenItStoppedBecauseACrossingFailed(t *testing.T) {
	g := autoGate("staging", admits("podinfo"))
	b := bundle("b1", "podinfo", 0)
	b.Status.Blocked = []v1alpha1.GateCrossing{{
		Gate: "staging", Passage: "staging-abc", Reason: "http: no such host",
	}}
	r, c, _ := newReconciler(t, g, &b)

	reconcileGate(t, r, "staging")

	if n := len(listPassages(t, c)); n != 0 {
		t.Fatalf("started %d Passages after a failure, want 0", n)
	}
	cond := ready(t, getGate(t, c, "staging"))
	if cond.Reason != "CrossingFailed" {
		t.Errorf("reason = %q, want CrossingFailed", cond.Reason)
	}
	for _, want := range []string{"no such host", "hecate promote staging --bundle b1"} {
		if !strings.Contains(cond.Message, want) {
			t.Errorf("message does not say %q: %s", want, cond.Message)
		}
	}
}

// A Gate must not be woken by its own status write.
//
// Every reconcile stamps status.health.observedAt with the current time, so
// the write always differs and triggers the watch. Hidden by metav1.Time's
// one-second resolution — a Gate reconciles in well under a second, so two
// reconciles share a timestamp and the loop dies. A health check is a network
// call, and the day one takes longer than a second is the day this becomes a
// hot loop against whatever is already slow.
func TestAGateIsNotWokenByItsOwnStatusWrite(t *testing.T) {
	p := ownStatusWrites()

	old := &v1alpha1.Gate{ObjectMeta: metav1.ObjectMeta{Name: "staging", Generation: 1}}
	updated := old.DeepCopy()
	updated.Status.Health = &v1alpha1.HealthReport{
		Status:     v1alpha1.HealthHealthy,
		ObservedAt: &metav1.Time{Time: base},
	}
	updated.ResourceVersion = "2"

	if p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: updated}) {
		t.Error("a status-only change woke the Gate, which is how a slow health check " +
			"becomes a hot loop")
	}

	edited := old.DeepCopy()
	edited.Generation = 2
	if !p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: edited}) {
		t.Error("editing a Gate's spec did not wake it")
	}

	poked := old.DeepCopy()
	poked.Annotations = map[string]string{v1alpha1.AnnotationReconcile: "1786620716793895315"}
	if !p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: poked}) {
		t.Error("a reconcile request did not wake the Gate")
	}
}

// stubVerifier answers whatever the test says, so the Gate's behaviour is
// tested rather than Flagger's.
type stubVerifier struct {
	result verify.Result
	err    error
}

func (s stubVerifier) Name() string { return "flagger" }
func (s stubVerifier) Verify(context.Context, string, []byte) (verify.Result, error) {
	return s.result, s.err
}

func verifyingGate(name string, admissions ...v1alpha1.Admission) *v1alpha1.Gate {
	g := autoGate(name, admissions...)
	g.Spec.Verify = []v1alpha1.Verification{{Uses: "flagger"}}
	return g
}

// #21's done-when, end to end: a failed canary in staging stops production
// from admitting the Bundle.
//
// A rolled-back canary leaves a healthy Deployment serving the previous
// version, so the crossing succeeded and every health check passes while
// nothing was delivered. `cleared` is what downstream Gates read, so gating
// that write is what makes the difference visible.
func TestAFailedCanaryStopsTheBundleReachingProduction(t *testing.T) {
	staging := verifyingGate("staging", admits("podinfo"))
	production := autoGate("production", admits("podinfo", "staging"))
	b := bundle("b1", "podinfo", 0)

	r, c, _ := newReconciler(t, staging, production, &b)
	r.Verifiers = map[string]Verifier{"flagger": stubVerifier{
		result: verify.Result{Done: true, Reason: "Canary podinfo Failed after 3 failed check(s)"},
	}}

	// Staging crosses, and the crossing itself succeeds.
	reconcileGate(t, r, "staging")
	p := listPassages(t, c)[0]
	p.Status.Phase = v1alpha1.PassageSucceeded
	if err := c.Status().Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	reconcileGate(t, r, "staging")

	var updated v1alpha1.Bundle
	if err := c.Get(context.Background(),
		types.NamespacedName{Name: "b1", Namespace: "acme"}, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.HasCleared("staging") {
		t.Fatal("a Bundle whose canary rolled back was recorded as having cleared staging")
	}
	// Not blocked either: the crossing did not fail, the verification did, and
	// calling it blocked would report the wrong thing to whoever looks.
	if len(updated.Status.Blocked) != 0 {
		t.Errorf("blocked = %+v, want the crossing recorded as neither cleared nor blocked",
			updated.Status.Blocked)
	}
	if cond := ready(t, getGate(t, c, "staging")); cond.Reason != "Verifying" ||
		!strings.Contains(cond.Message, "3 failed check") {
		t.Errorf("staging says %q: %s", cond.Reason, cond.Message)
	}

	// And the point of the whole thing: production must not admit it.
	reconcileGate(t, r, "production")
	for _, p := range listPassages(t, c) {
		if p.Spec.Gate == "production" {
			t.Errorf("production started Passage %s for a Bundle whose canary failed", p.Name)
		}
	}
}

// The same Gate with a canary that succeeded must clear, or verification is
// just a way to stop everything.
func TestAPassingCanaryClearsTheBundle(t *testing.T) {
	g := verifyingGate("staging", admits("podinfo"))
	b := bundle("b1", "podinfo", 0)
	r, c, _ := newReconciler(t, g, &b)
	r.Verifiers = map[string]Verifier{"flagger": stubVerifier{
		result: verify.Result{Verified: true, Done: true, Reason: "Canary podinfo succeeded"},
	}}

	reconcileGate(t, r, "staging")
	p := listPassages(t, c)[0]
	p.Status.Phase = v1alpha1.PassageSucceeded
	if err := c.Status().Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	reconcileGate(t, r, "staging")

	var updated v1alpha1.Bundle
	if err := c.Get(context.Background(),
		types.NamespacedName{Name: "b1", Namespace: "acme"}, &updated); err != nil {
		t.Fatal(err)
	}
	if !updated.HasCleared("staging") {
		t.Error("a verified crossing did not clear the Bundle")
	}
}

// A verifier named but not registered is a refusal, not a pass: skipping it
// would clear the Bundle on the strength of a verifier nobody ran.
func TestAnUnknownVerifierRefuses(t *testing.T) {
	g := autoGate("staging", admits("podinfo"))
	g.Spec.Verify = []v1alpha1.Verification{{Uses: "nonesuch"}}
	b := bundle("b1", "podinfo", 0)
	r, c, _ := newReconciler(t, g, &b)
	r.Verifiers = map[string]Verifier{}

	reconcileGate(t, r, "staging")
	p := listPassages(t, c)[0]
	p.Status.Phase = v1alpha1.PassageSucceeded
	if err := c.Status().Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "staging", Namespace: "acme"},
	})
	if err == nil {
		t.Fatal("an unknown verifier was treated as a pass")
	}

	var updated v1alpha1.Bundle
	if err := c.Get(context.Background(),
		types.NamespacedName{Name: "b1", Namespace: "acme"}, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.HasCleared("staging") {
		t.Error("the Bundle cleared despite no verifier having run")
	}
}

// A verdict that arrives after the first look must still clear the Bundle.
//
// Found against a real Canary, not here: `current` is written as soon as the
// crossing succeeds, and the "already recorded" guard then short-circuited
// every later reconcile before the verifier ran. A canary that passed a minute
// after the crossing left the Bundle permanently unclear, and every unit test
// passed throughout.
func TestAVerdictThatArrivesLateStillClears(t *testing.T) {
	g := verifyingGate("staging", admits("podinfo"))
	b := bundle("b1", "podinfo", 0)
	r, c, _ := newReconciler(t, g, &b)

	pending := stubVerifier{result: verify.Result{Reason: "Canary podinfo is Progressing"}}
	r.Verifiers = map[string]Verifier{"flagger": pending}

	reconcileGate(t, r, "staging")
	p := listPassages(t, c)[0]
	p.Status.Phase = v1alpha1.PassageSucceeded
	if err := c.Status().Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	reconcileGate(t, r, "staging")

	var mid v1alpha1.Bundle
	if err := c.Get(context.Background(),
		types.NamespacedName{Name: "b1", Namespace: "acme"}, &mid); err != nil {
		t.Fatal(err)
	}
	if mid.HasCleared("staging") {
		t.Fatal("cleared while the canary was still progressing")
	}

	// The canary finishes, and the Gate looks again.
	r.Verifiers = map[string]Verifier{"flagger": stubVerifier{
		result: verify.Result{Verified: true, Done: true, Reason: "Canary podinfo succeeded"},
	}}
	reconcileGate(t, r, "staging")

	var after v1alpha1.Bundle
	if err := c.Get(context.Background(),
		types.NamespacedName{Name: "b1", Namespace: "acme"}, &after); err != nil {
		t.Fatal(err)
	}
	if !after.HasCleared("staging") {
		t.Error("a canary that passed after the first look never cleared the Bundle")
	}

	// And reconsidering must not re-append: the history is capped, and growing
	// it once per reconcile is the #121 shape in a list that was already fixed.
	reconcileGate(t, r, "staging")
	reconcileGate(t, r, "staging")
	if n := len(getGate(t, c, "staging").Status.History); n != 1 {
		t.Errorf("history = %d entries after four reconciles, want 1", n)
	}
	var final v1alpha1.Bundle
	if err := c.Get(context.Background(),
		types.NamespacedName{Name: "b1", Namespace: "acme"}, &final); err != nil {
		t.Fatal(err)
	}
	if n := len(final.Status.Cleared); n != 1 {
		t.Errorf("cleared = %d entries, want 1", n)
	}
}
