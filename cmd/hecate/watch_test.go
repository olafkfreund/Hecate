package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/ops"
)

func watchOps(t *testing.T, objs ...client.Object) *ops.Ops {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return ops.New(fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build())
}

func passage(phase v1alpha1.PassagePhase, message string, steps ...v1alpha1.StepStatus) *v1alpha1.Passage {
	return &v1alpha1.Passage{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "acme"},
		Spec:       v1alpha1.PassageSpec{Gate: "production", Bundle: "app-abc"},
		Status:     v1alpha1.PassageStatus{Phase: phase, Message: message, Steps: steps},
	}
}

func step(uses string, phase v1alpha1.StepPhase, msg string) v1alpha1.StepStatus {
	return v1alpha1.StepStatus{Uses: uses, Phase: phase, Message: msg}
}

// The exit code is the point of --watch: a CI job needs to know whether the
// promotion landed, and `hecate promote` alone can only say a Passage opened.
func TestWatchExitsOnTheOutcome(t *testing.T) {
	for _, tc := range []struct {
		name  string
		phase v1alpha1.PassagePhase
		want  int
		says  string
	}{
		{"a crossing that landed", v1alpha1.PassageSucceeded, exitOK, "crossed production"},
		{"one that did not", v1alpha1.PassageFailed, exitCrossingFailed, "did not cross"},
		{"one that was aborted", v1alpha1.PassageAborted, exitCrossingFailed, "aborted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := watchOps(t, passage(tc.phase, "the cluster said no"))
			var code int
			out := capture(t, func() {
				code = watchPassage(context.Background(), o, "acme", "p1", time.Minute)
			})
			if code != tc.want {
				t.Errorf("exit = %d, want %d", code, tc.want)
			}
			if !strings.Contains(out, tc.says) {
				t.Errorf("output = %q, want it to mention %q", out, tc.says)
			}
		})
	}
}

// A failed crossing must not be exitError. "The promotion failed" and "I could
// not tell you whether it did" call for different things in a pipeline, and
// only the first is a deployment failure.
func TestAFailedCrossingIsNotAnError(t *testing.T) {
	o := watchOps(t, passage(v1alpha1.PassageFailed, "no"))
	var code int
	capture(t, func() { code = watchPassage(context.Background(), o, "acme", "p1", time.Minute) })
	if code == exitError {
		t.Errorf("a failed crossing exited %d, which a pipeline cannot tell apart "+
			"from losing the cluster", code)
	}
}

// Steps are announced as they settle, so a crossing that waits ten minutes on
// Flux is not ten minutes of silence.
func TestWatchPrintsSettledSteps(t *testing.T) {
	o := watchOps(t, passage(v1alpha1.PassageSucceeded, "",
		step("git-clone", v1alpha1.StepSucceeded, "checked out fleet"),
		step("flux-wait", v1alpha1.StepSucceeded, "Flux applied it"),
	))
	out := capture(t, func() { watchPassage(context.Background(), o, "acme", "p1", time.Minute) })

	for _, want := range []string{"git-clone", "checked out fleet", "flux-wait", "Flux applied it"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}
}

// A step still running is not announced — a Passage read once a second would
// otherwise print the same line six hundred times.
func TestWatchDoesNotAnnounceRunningSteps(t *testing.T) {
	p := passage(v1alpha1.PassageSucceeded, "",
		step("flux-wait", v1alpha1.StepRunning, "waiting for Flux"))
	o := watchOps(t, p)
	out := capture(t, func() { watchPassage(context.Background(), o, "acme", "p1", time.Minute) })

	if strings.Contains(out, "waiting for Flux") {
		t.Errorf("announced a step that has not settled:\n%s", out)
	}
}

// Ctrl-C stops the watching, not the crossing, and the message has to say so —
// otherwise the obvious reading is that the promotion was cancelled.
func TestWatchStoppingSaysTheCrossingContinues(t *testing.T) {
	o := watchOps(t, passage(v1alpha1.PassageRunning, "still going",
		step("flux-wait", v1alpha1.StepRunning, "waiting")))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var code int
	out := capture(t, func() { code = watchPassage(ctx, o, "acme", "p1", time.Minute) })
	if code == exitOK {
		t.Error("stopping early reported success")
	}
	if !strings.Contains(out, "still crossing") {
		t.Errorf("output = %q, want it to say the crossing continues", out)
	}
}

// A Passage that has been collected is a real answer rather than a transient,
// so it must not be retried until the timeout — and the message has to explain
// itself, because a bare NotFound reads like a typo in the name rather than
// like Gate retention having tidied a finished crossing away (D40).
func TestWatchReportsAVanishedPassage(t *testing.T) {
	o := watchOps(t)

	var code int
	done := make(chan struct{})
	var stderr string
	go func() {
		defer close(done)
		stderr = captureStderr(t, func() {
			code = watchPassage(context.Background(), o, "acme", "p1", time.Minute)
		})
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a Passage that is gone was retried rather than reported")
	}

	if code != exitError {
		t.Errorf("exit = %d, want exitError for a Passage that is gone", code)
	}
	if !strings.Contains(stderr, "collected") {
		t.Errorf("message = %q, want it to explain that the Passage may have been "+
			"collected rather than surfacing a bare NotFound", stderr)
	}
}

// captureStderr is capture's twin: fail() writes there, not to stdout.
func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	f()
	_ = w.Close()
	os.Stderr = old

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}
