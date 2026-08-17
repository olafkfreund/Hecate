package passage

import (
	"os"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// What happens to an in-flight promotion when the control plane is upgraded.
//
// D5 says all progress lives in the Passage object, so a controller restart
// resumes mid-Passage instead of starting over, and the work directory is
// deliberately local and disposable because steps are re-entrant. That is the
// answer #54 needs to give operators about upgrades — and until this test it
// was a design intention with nothing demonstrating it.
//
// TestPassageResumesAcrossReconciles is not the same claim. It calls the same
// Reconciler twice, so the process, its in-memory state and its scratch
// directory all survive. An upgrade destroys all three.

// TestAPassageSurvivesAControlPlaneUpgrade replaces the controller mid-crossing.
//
// The new Reconciler shares only the API server, which is what actually
// persists across a rollout. Everything process-local — the engine, the
// runners, the work directory — is rebuilt from nothing, exactly as it is when
// a new pod starts.
func TestAPassageSurvivesAControlPlaneUpgrade(t *testing.T) {
	var seen []StepContext
	waiter := &scripted{
		name: "wait",
		results: []StepResult{
			{Phase: v1alpha1.StepRunning, Message: "waiting for Flux", RetryAfter: 5 * time.Second},
			ok("converged"),
		},
		observe: func(sc *StepContext) { seen = append(seen, *sc) },
	}

	before, c, _ := newController(t, []Runner{waiter},
		passageObj(v1alpha1.Step{Uses: "wait"}), bundleObj())

	advance(t, before)
	if got := getPassage(t, c); got.Status.Phase != v1alpha1.PassageRunning {
		t.Fatalf("phase = %s, want Running before the upgrade", got.Status.Phase)
	}

	// The upgrade. The old pod's scratch space goes with it — removed rather
	// than merely abandoned, because a test that left it in place would pass
	// even if a step depended on it.
	workDir := before.WorkDir(getPassage(t, c))
	if _, err := os.Stat(workDir); err != nil {
		t.Fatalf("expected a work directory to exist before the upgrade: %v", err)
	}
	if err := os.RemoveAll(before.workRoot()); err != nil {
		t.Fatal(err)
	}

	after := restarted(t, c, waiter)

	advance(t, after)

	got := getPassage(t, c)
	if got.Status.Phase != v1alpha1.PassageSucceeded {
		t.Fatalf("phase = %s (%s), want Succeeded — the crossing did not survive the upgrade",
			got.Status.Phase, got.Status.Message)
	}

	// Resumed, not restarted. Two invocations total: if the new controller had
	// started the Passage over, the waiting step would have run from the top
	// and the crossing would have taken three.
	if waiter.calls != 2 {
		t.Errorf("the step ran %d times across the upgrade, want 2 — "+
			"more than that means the new controller started over", waiter.calls)
	}

	// And it was told it was a resumption. A step that cannot tell a first
	// invocation from a second cannot be re-entrant on purpose, only by luck.
	if len(seen) != 2 {
		t.Fatalf("observed %d invocations, want 2", len(seen))
	}
	if seen[0].Attempt != 0 {
		t.Errorf("first attempt = %d, want 0", seen[0].Attempt)
	}
	if seen[1].Attempt != 1 {
		t.Errorf("attempt after the upgrade = %d, want 1 — the new controller "+
			"did not carry the attempt count, so a step cannot tell it is resuming",
			seen[1].Attempt)
	}

	// The new controller works in its own directory and made one.
	if newDir := after.WorkDir(got); newDir == workDir {
		t.Errorf("the new controller reused the old work directory %s", newDir)
	}
}

// TestAFinishedPassageIsNotRerunAfterAnUpgrade is the other half, and the more
// dangerous one: a Passage that completed just before the rollout must not be
// crossed a second time by the controller that replaces it. A promotion run
// twice is a deployment nobody asked for.
func TestAFinishedPassageIsNotRerunAfterAnUpgrade(t *testing.T) {
	step := &scripted{name: "a", results: []StepResult{ok("done")}}

	before, c, _ := newController(t, []Runner{step},
		passageObj(v1alpha1.Step{Uses: "a"}), bundleObj())

	advance(t, before)
	finished := getPassage(t, c)
	if finished.Status.Phase != v1alpha1.PassageSucceeded {
		t.Fatalf("phase = %s, want Succeeded before the upgrade", finished.Status.Phase)
	}
	calls := step.calls

	if err := os.RemoveAll(before.workRoot()); err != nil {
		t.Fatal(err)
	}
	after := restarted(t, c, step)

	// A rollout re-lists everything, so every Passage is reconciled again —
	// including the ones already done.
	advance(t, after)

	if step.calls != calls {
		t.Errorf("the step ran %d more time(s) after the upgrade, want 0 — "+
			"a finished Passage is a permanent record, not work to redo",
			step.calls-calls)
	}
	got := getPassage(t, c)
	if got.Status.Phase != v1alpha1.PassageSucceeded {
		t.Errorf("phase = %s, want the terminal phase left alone", got.Status.Phase)
	}
	if !got.Status.FinishedAt.Equal(finished.Status.FinishedAt) {
		t.Errorf("FinishedAt moved from %s to %s — the record was rewritten",
			finished.Status.FinishedAt, got.Status.FinishedAt)
	}
}

// restarted builds the Reconciler a newly-rolled-out pod would have: same API
// server, everything else fresh.
func restarted(t *testing.T, c client.Client, runners ...Runner) *Reconciler {
	t.Helper()
	return &Reconciler{
		Client:   c,
		Engine:   newEngine(runners...),
		WorkRoot: t.TempDir(),
		Now:      func() time.Time { return clock },
	}
}
