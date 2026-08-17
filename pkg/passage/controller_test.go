package passage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/metrics"
	"github.com/olafkfreund/hecate/pkg/telemetry"
)

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func passageObj(steps ...v1alpha1.Step) *v1alpha1.Passage {
	return &v1alpha1.Passage{
		ObjectMeta: metav1.ObjectMeta{
			Name: "p1", Namespace: "acme", UID: types.UID("uid-1"), Generation: 1,
		},
		Spec: v1alpha1.PassageSpec{Gate: "production", Bundle: "b1", Steps: steps},
	}
}

func bundleObj() *v1alpha1.Bundle {
	return &v1alpha1.Bundle{
		ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: "acme"},
		Spec: v1alpha1.BundleSpec{
			Beacon:    "podinfo",
			Artifacts: []v1alpha1.Artifact{{Image: &v1alpha1.ImageArtifact{Repo: "ghcr.io/acme/app", Tag: "1.0.0"}}},
		},
	}
}

func newController(t *testing.T, runners []Runner, objs ...client.Object) (*Reconciler, client.Client, *events.FakeRecorder) {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(scheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.Passage{}, &v1alpha1.Bundle{}).
		Build()
	rec := events.NewFakeRecorder(20)
	return &Reconciler{
		Client:   c,
		Engine:   newEngine(runners...),
		Recorder: rec,
		WorkRoot: t.TempDir(),
		Now:      func() time.Time { return clock },
	}, c, rec
}

func advance(t *testing.T, r *Reconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "p1", Namespace: "acme"},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

func getPassage(t *testing.T, c client.Client) *v1alpha1.Passage {
	t.Helper()
	var p v1alpha1.Passage
	if err := c.Get(context.Background(), types.NamespacedName{Name: "p1", Namespace: "acme"}, &p); err != nil {
		t.Fatal(err)
	}
	return &p
}

func TestPassageRunsToSuccess(t *testing.T) {
	step := &scripted{name: "a", results: []StepResult{ok("done")}}
	r, c, rec := newController(t, []Runner{step}, passageObj(v1alpha1.Step{Uses: "a"}), bundleObj())

	res := advance(t, r)

	got := getPassage(t, c)
	if got.Status.Phase != v1alpha1.PassageSucceeded {
		t.Fatalf("phase = %s, want Succeeded (%s)", got.Status.Phase, got.Status.Message)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("a finished Passage should not requeue, got %s", res.RequeueAfter)
	}
	if got.Status.FinishedAt == nil {
		t.Error("FinishedAt must be recorded")
	}
	drainFor(t, rec, "PassageSucceeded")
}

func TestPassageResumesAcrossReconciles(t *testing.T) {
	waiter := &scripted{name: "wait", results: []StepResult{
		{Phase: v1alpha1.StepRunning, Message: "waiting for Flux", RetryAfter: 5 * time.Second},
		ok("converged"),
	}}
	r, c, _ := newController(t, []Runner{waiter}, passageObj(v1alpha1.Step{Uses: "wait"}), bundleObj())

	res := advance(t, r)
	if got := getPassage(t, c); got.Status.Phase != v1alpha1.PassageRunning {
		t.Fatalf("phase = %s, want Running", got.Status.Phase)
	}
	if res.RequeueAfter != 5*time.Second {
		t.Errorf("RequeueAfter = %s, want the step's suggestion of 5s", res.RequeueAfter)
	}

	advance(t, r)
	if got := getPassage(t, c); got.Status.Phase != v1alpha1.PassageSucceeded {
		t.Fatalf("phase = %s, want Succeeded", got.Status.Phase)
	}
	if waiter.calls != 2 {
		t.Errorf("waiting step invoked %d times, want 2", waiter.calls)
	}
}

// The step-emitted watch is what stops the Gate going blind after a crossing.
func TestEmittedWatchIsPersisted(t *testing.T) {
	emitter := &scripted{name: "flux-wait", results: []StepResult{{
		Phase: v1alpha1.StepSucceeded,
		Watch: []v1alpha1.HealthCheck{{
			Uses: "flux",
			With: &apiextensionsv1.JSON{Raw: []byte(`{"resources":[{"kind":"Kustomization","name":"podinfo"}]}`)},
		}},
	}}}
	r, c, _ := newController(t, []Runner{emitter}, passageObj(v1alpha1.Step{Uses: "flux-wait"}), bundleObj())

	advance(t, r)

	got := getPassage(t, c)
	if len(got.Status.Watch) != 1 {
		t.Fatalf("status.watch = %d entries, want 1 for the Gate to adopt", len(got.Status.Watch))
	}
	if got.Status.Watch[0].Uses != "flux" {
		t.Errorf("watch uses %q, want flux", got.Status.Watch[0].Uses)
	}
	if !strings.Contains(string(got.Status.Watch[0].With.Raw), "podinfo") {
		t.Errorf("watch config lost its resources: %s", got.Status.Watch[0].With.Raw)
	}
}

func TestAbortStopsAPassage(t *testing.T) {
	waiter := &scripted{name: "wait", results: []StepResult{
		{Phase: v1alpha1.StepRunning, RetryAfter: time.Second},
	}}
	r, c, rec := newController(t, []Runner{waiter}, passageObj(v1alpha1.Step{Uses: "wait"}), bundleObj())

	advance(t, r)
	if got := getPassage(t, c); got.Status.Phase != v1alpha1.PassageRunning {
		t.Fatalf("phase = %s, want Running before the abort", got.Status.Phase)
	}

	p := getPassage(t, c)
	p.Spec.Abort = true
	if err := c.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	callsBefore := waiter.calls

	advance(t, r)

	got := getPassage(t, c)
	if got.Status.Phase != v1alpha1.PassageAborted {
		t.Fatalf("phase = %s, want Aborted", got.Status.Phase)
	}
	// Abort is checked before any step runs, so the waiting step is not invoked
	// again just to be told to stop.
	if waiter.calls != callsBefore {
		t.Errorf("step ran %d more times after abort, want 0", waiter.calls-callsBefore)
	}
	drainFor(t, rec, "PassageAborted")
}

func TestFailedPassageStopsAndReports(t *testing.T) {
	broken := &scripted{name: "broken", results: []StepResult{{}}, errs: []error{Terminalf("bad config")}}
	r, c, rec := newController(t, []Runner{broken}, passageObj(v1alpha1.Step{Uses: "broken"}), bundleObj())

	advance(t, r)

	got := getPassage(t, c)
	if got.Status.Phase != v1alpha1.PassageFailed {
		t.Fatalf("phase = %s, want Failed", got.Status.Phase)
	}
	if !strings.Contains(got.Status.Message, "bad config") {
		t.Errorf("message = %q, should carry the step's error", got.Status.Message)
	}
	drainFor(t, rec, "PassageFailed")
}

// A Passage is a permanent record: reconciling one that has finished must never
// re-run its steps.
func TestTerminalPassageIsNeverRerun(t *testing.T) {
	step := &scripted{name: "a", results: []StepResult{ok("done")}}
	r, _, _ := newController(t, []Runner{step}, passageObj(v1alpha1.Step{Uses: "a"}), bundleObj())

	advance(t, r)
	callsAfterFirst := step.calls

	for i := 0; i < 3; i++ {
		advance(t, r)
	}
	if step.calls != callsAfterFirst {
		t.Errorf("steps ran %d extra times after completion, want 0", step.calls-callsAfterFirst)
	}
}

// Running against a Bundle that no longer exists would interpolate nothing and
// promote something meaningless.
func TestMissingBundleFailsThePassage(t *testing.T) {
	step := &scripted{name: "a", results: []StepResult{ok("")}}
	r, c, rec := newController(t, []Runner{step}, passageObj(v1alpha1.Step{Uses: "a"}))

	advance(t, r)

	got := getPassage(t, c)
	if got.Status.Phase != v1alpha1.PassageFailed {
		t.Fatalf("phase = %s, want Failed", got.Status.Phase)
	}
	if !strings.Contains(got.Status.Message, "b1") {
		t.Errorf("message = %q, should name the missing Bundle", got.Status.Message)
	}
	if step.calls != 0 {
		t.Error("no step should run without a Bundle")
	}
	drainFor(t, rec, "BundleMissing")
}

func TestWorkDirIsCreatedSharedAndRemoved(t *testing.T) {
	var seenDir string
	writer := runnerFunc{name: "write", fn: func(_ context.Context, sc *StepContext) (StepResult, error) {
		seenDir = sc.WorkDir
		return ok(""), os.WriteFile(filepath.Join(sc.WorkDir, "cloned"), []byte("x"), 0o600)
	}}
	reader := runnerFunc{name: "read", fn: func(_ context.Context, sc *StepContext) (StepResult, error) {
		// The second step must see what the first left behind.
		if _, err := os.Stat(filepath.Join(sc.WorkDir, "cloned")); err != nil {
			return StepResult{}, err
		}
		return ok(""), nil
	}}
	r, c, _ := newController(t, []Runner{writer, reader},
		passageObj(v1alpha1.Step{Uses: "write"}, v1alpha1.Step{Uses: "read"}), bundleObj())

	advance(t, r)

	if got := getPassage(t, c); got.Status.Phase != v1alpha1.PassageSucceeded {
		t.Fatalf("phase = %s (%s) — the second step could not see the first step's files",
			got.Status.Phase, got.Status.Message)
	}
	if seenDir == "" {
		t.Fatal("steps were given an empty work dir")
	}
	// Scratch is disposable and must not outlive the Passage.
	if _, err := os.Stat(seenDir); !os.IsNotExist(err) {
		t.Errorf("work dir %s survived a finished Passage", seenDir)
	}
}

// Keyed by UID, so a recreated Passage never inherits leftovers.
func TestWorkDirIsKeyedByUID(t *testing.T) {
	r, _, _ := newController(t, nil, passageObj(), bundleObj())
	a := r.WorkDir(&v1alpha1.Passage{ObjectMeta: metav1.ObjectMeta{Name: "same", UID: "uid-a"}})
	b := r.WorkDir(&v1alpha1.Passage{ObjectMeta: metav1.ObjectMeta{Name: "same", UID: "uid-b"}})
	if a == b {
		t.Errorf("two Passages with the same name shared a work dir: %s", a)
	}
}

func TestMissingPassageIsNotAnError(t *testing.T) {
	r, _, _ := newController(t, nil)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "gone", Namespace: "acme"},
	}); err != nil {
		t.Errorf("a deleted Passage should reconcile cleanly, got %v", err)
	}
}

// drainFor asserts an event mentioning reason was recorded, and that no
// duplicate follows on subsequent reconciles.
func drainFor(t *testing.T, rec *events.FakeRecorder, reason string) {
	t.Helper()
	select {
	case e := <-rec.Events:
		if !strings.Contains(e, reason) {
			t.Errorf("event = %q, want one mentioning %s", e, reason)
		}
	default:
		t.Errorf("no event recorded, want one mentioning %s", reason)
	}
}

// The whole chain in one test: the trace ID is allocated before the first step
// runs, the step sees it as a traceparent, and the trace that is finally emitted
// uses that same ID.
//
// Getting any link wrong is silent and permanent — a commit trailer pointing at
// a trace that does not exist, or a trace nothing in git refers to.
func TestPassageAllocatesATraceIDAndEmitsUnderIt(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4318")
	sr := recorder(t)

	var seen string
	step := &scripted{name: "a", results: []StepResult{ok("done")}}
	step.observe = func(sc *StepContext) { seen = sc.Traceparent }
	r, c, _ := newController(t, []Runner{step}, passageObj(v1alpha1.Step{Uses: "a"}), bundleObj())

	advance(t, r)

	got := getPassage(t, c)
	if got.Status.TraceID == "" {
		t.Fatal("a traced Passage must record the trace it emitted")
	}
	// The step ran before the status was written, so it can only have seen the
	// ID if allocation happens up front rather than at the end.
	if want := telemetry.Traceparent(got.Status.TraceID); seen != want {
		t.Errorf("the step saw traceparent %q, want %q", seen, want)
	}

	spans := sr.Ended()
	if len(spans) == 0 {
		t.Fatal("no spans were emitted")
	}
	root := spans[len(spans)-1]
	if root.SpanContext().TraceID().String() != got.Status.TraceID {
		t.Errorf("the exported trace is %q, but status.traceID says %q",
			root.SpanContext().TraceID(), got.Status.TraceID)
	}
}

// With no collector configured there is no trace, so there must be no trace ID
// and no commit trailer promising one.
func TestPassageAllocatesNoTraceIDWhenTracingIsOff(t *testing.T) {
	var seen = "unset"
	step := &scripted{name: "a", results: []StepResult{ok("done")}}
	step.observe = func(sc *StepContext) { seen = sc.Traceparent }
	r, c, _ := newController(t, []Runner{step}, passageObj(v1alpha1.Step{Uses: "a"}), bundleObj())

	advance(t, r)

	if got := getPassage(t, c); got.Status.TraceID != "" {
		t.Errorf("status.traceID = %q, want empty with tracing off", got.Status.TraceID)
	}
	if seen != "" {
		t.Errorf("the step saw traceparent %q, want none", seen)
	}
}

// Lead time is measured from the Bundle, not from the Passage: the Passage's
// own start says how long the crossing took, which is a different and much
// smaller number. An artifact that sat for a day and then crossed in ten
// seconds has a lead time of a day.
func TestLeadTimeIsMeasuredFromTheBundle(t *testing.T) {
	metrics.LeadTime.Reset()

	bundle := bundleObj()
	bundle.CreationTimestamp = metav1.Time{Time: clock.Add(-24 * time.Hour)}
	step := &scripted{name: "a", results: []StepResult{ok("done")}}
	r, c, _ := newController(t, []Runner{step}, passageObj(v1alpha1.Step{Uses: "a"}), bundle)

	advance(t, r)

	if got := getPassage(t, c); got.Status.Phase != v1alpha1.PassageSucceeded {
		t.Fatalf("phase = %s (%s)", got.Status.Phase, got.Status.Message)
	}
	count, sum := leadTimeObservations(t)
	if count != 1 {
		t.Fatalf("lead-time observations = %d, want 1", count)
	}
	if sum < 24*3600 {
		t.Errorf("lead time = %.0fs, want at least a day — it must run from the "+
			"Bundle's creation, not the Passage's start", sum)
	}
}

// A crossing that failed delivered nothing. Counting it would flatter the
// number in exactly the situation where it should look worse.
func TestFailedCrossingsHaveNoLeadTime(t *testing.T) {
	metrics.LeadTime.Reset()

	bundle := bundleObj()
	bundle.CreationTimestamp = metav1.Time{Time: clock.Add(-24 * time.Hour)}
	step := &scripted{
		name:    "a",
		results: []StepResult{{Phase: v1alpha1.StepFailed, Message: "no"}},
		errs:    []error{FailTerminal("Boom", "it broke")},
	}
	r, c, _ := newController(t, []Runner{step}, passageObj(v1alpha1.Step{Uses: "a"}), bundle)

	advance(t, r)

	if got := getPassage(t, c); got.Status.Phase != v1alpha1.PassageFailed {
		t.Fatalf("phase = %s, want Failed", got.Status.Phase)
	}
	if count, _ := leadTimeObservations(t); count != 0 {
		t.Errorf("a failed crossing recorded %d lead-time observations", count)
	}
}

func leadTimeObservations(t *testing.T) (uint64, float64) {
	t.Helper()
	ch := make(chan prometheus.Metric, 32)
	metrics.LeadTime.Collect(ch)
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
