package gate

import (
	"context"
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

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/health"
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
	updated.Status.ApprovedFor = []string{"production"}
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
