package passage

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

const (
	attestEnv    = "7f3a1c2e-0000-4000-8000-000000009b04"
	attestDigest = "sha256:abcdef0123456789"
	attestTrail  = "91b2ffff-0000-4000-8000-00000000abcd"
)

// fakeFides answers the two calls attesting makes: the artifact lookup that
// finds the trail, and the attestation itself.
type fakeFides struct {
	// trail is what the artifact lookup returns. Empty means Fides has never
	// heard of the digest.
	trail string
	// status, when set, is returned for every request — an outage.
	status int
	// attestStatus fails only the attestation, leaving the lookup working:
	// the trail is found and the record still does not get written.
	attestStatus int

	posted  []map[string]any
	lookups int
}

func (f *fakeFides) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.status != 0 {
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte("refused"))
			return
		}
		switch {
		case strings.HasSuffix(r.URL.EscapedPath(), "/attestations"):
			if f.attestStatus != 0 {
				w.WriteHeader(f.attestStatus)
				_, _ = w.Write([]byte("refused"))
				return
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decoding the attestation: %v", err)
			}
			f.posted = append(f.posted, body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		case strings.HasSuffix(r.URL.EscapedPath(), "/artifacts"):
			f.lookups++
			if f.trail == "" {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[{"sha256":"` +
				strings.TrimPrefix(attestDigest, "sha256:") + `","trail_id":"` + f.trail + `"}]`))
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// payload decodes the attestation body, which Fides stores as a JSON string.
func (f *fakeFides) payload(t *testing.T) map[string]any {
	t.Helper()
	if len(f.posted) != 1 {
		t.Fatalf("want exactly one attestation, got %d", len(f.posted))
	}
	raw, ok := f.posted[0]["payload"].(string)
	if !ok {
		t.Fatalf("the payload is %T, want a JSON string", f.posted[0]["payload"])
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decoding the payload: %v", err)
	}
	return out
}

func gateWithEvidence(server string) *v1alpha1.Gate {
	return &v1alpha1.Gate{
		ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: "acme"},
		Spec: v1alpha1.GateSpec{
			Evidence: &v1alpha1.EvidenceConfig{
				FidesEnvironment: attestEnv,
				ServerURL:        server,
				CredentialsRef:   &v1alpha1.LocalSecretRef{Name: "fides"},
			},
		},
	}
}

func fidesSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "fides", Namespace: "acme"},
		Data:       map[string][]byte{"token": []byte("t0ken")},
	}
}

// digestBundle is the Bundle attesting can actually work from: one with a
// pinned digest, which is what ties the record to what shipped.
func digestBundle() *v1alpha1.Bundle {
	b := bundleObj()
	b.Spec.Digest = "sha256:bbbb"
	b.Spec.Artifacts = []v1alpha1.Artifact{{Image: &v1alpha1.ImageArtifact{
		Repo: "ghcr.io/acme/app", Tag: "1.0.0", Digest: attestDigest,
	}}}
	return b
}

// attestController is newController plus a scheme that knows about Secrets,
// which the credentials lookup needs.
func attestController(t *testing.T, runners []Runner, objs ...client.Object) (*Reconciler, client.Client, *events.FakeRecorder) {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().
		WithScheme(s).
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

func TestFinishedPassageIsAttested(t *testing.T) {
	f := &fakeFides{trail: attestTrail}
	server := f.start(t)

	p := passageObj(v1alpha1.Step{Uses: "a"}, v1alpha1.Step{Uses: "b"})
	p.Spec.Actor = "olaf@acme.example"
	step := &scripted{name: "a", results: []StepResult{ok("done")}}
	// b fails, so the record has to carry an outcome that is not "everything
	// was fine" — the case an audit actually cares about.
	failing := &scripted{name: "b", results: []StepResult{{}}, errs: []error{
		FailTerminal("Nope", "b refused"),
	}}
	r, c, _ := attestController(t, []Runner{step, failing},
		p, digestBundle(), gateWithEvidence(server), fidesSecret())

	advance(t, r)

	body := f.posted
	if len(body) != 1 {
		t.Fatalf("want one attestation, got %d", len(body))
	}
	if got := body[0]["trail_id"]; got != attestTrail {
		t.Errorf("trail_id = %v, want %s", got, attestTrail)
	}
	if got := body[0]["artifact_sha256"]; got != strings.TrimPrefix(attestDigest, "sha256:") {
		t.Errorf("artifact_sha256 = %v, want the digest without its prefix", got)
	}
	if got := body[0]["signed_by"]; got != "olaf@acme.example" {
		t.Errorf("signed_by = %v, want the actor who asked for the crossing", got)
	}
	if got := body[0]["type_name"]; got != "promotion" {
		t.Errorf("type_name = %v — a policy that requires \"promotion\" would never see this", got)
	}

	payload := f.payload(t)
	for field, want := range map[string]any{
		"gate":         "production",
		"bundle":       "b1",
		"actor":        "olaf@acme.example",
		"outcome":      string(v1alpha1.PassageFailed),
		"passage":      "p1",
		"namespace":    "acme",
		"bundleDigest": "sha256:bbbb",
	} {
		if got := payload[field]; got != want {
			t.Errorf("payload[%q] = %v, want %v", field, got, want)
		}
	}

	steps, _ := payload["steps"].([]any)
	if len(steps) != 2 {
		t.Fatalf("the record lists %d steps, want both of them — a record that omits the "+
			"step that failed is worse than none", len(steps))
	}
	second, _ := steps[1].(map[string]any)
	if second["phase"] != string(v1alpha1.StepFailed) || second["reason"] != "Nope" {
		t.Errorf("the failing step is recorded as %v, want its phase and reason", second)
	}

	if got := getPassage(t, c); got.Status.Evidence == nil || got.Status.Evidence.Trail != attestTrail {
		t.Errorf("status.evidence = %+v, want the trail the crossing was recorded on — "+
			"`hecate verify` has nothing to check without it", got.Status.Evidence)
	}
}

func TestAGateWithoutEvidenceIsNotAttested(t *testing.T) {
	f := &fakeFides{trail: attestTrail}
	server := f.start(t)

	gate := gateWithEvidence(server)
	gate.Spec.Evidence = nil
	step := &scripted{name: "a", results: []StepResult{ok("done")}}
	r, _, rec := attestController(t, []Runner{step},
		passageObj(v1alpha1.Step{Uses: "a"}), digestBundle(), gate, fidesSecret())

	advance(t, r)

	if len(f.posted) != 0 || f.lookups != 0 {
		t.Errorf("a Gate that does not use Fides made %d lookups and %d attestations",
			f.lookups, len(f.posted))
	}
	if ev := drain(rec); strings.Contains(ev, "Attestation") {
		t.Errorf("event %q — a cluster that has never heard of Fides should hear nothing about it", ev)
	}
}

func TestAnUnrecordedCrossingSaysSo(t *testing.T) {
	f := &fakeFides{trail: attestTrail, status: http.StatusInternalServerError}
	server := f.start(t)

	step := &scripted{name: "a", results: []StepResult{ok("done")}}
	r, c, rec := attestController(t, []Runner{step},
		passageObj(v1alpha1.Step{Uses: "a"}), digestBundle(), gateWithEvidence(server), fidesSecret())

	advance(t, r)

	// The promotion happened. Failing the Passage over the evidence system
	// being down would be a lie about what the cluster did.
	if got := getPassage(t, c); got.Status.Phase != v1alpha1.PassageSucceeded {
		t.Errorf("phase = %s, want Succeeded — Fides being down does not un-promote", got.Status.Phase)
	}
	drainFor(t, rec, "AttestationFailed")
}

func TestARefusedAttestationSaysSo(t *testing.T) {
	// The lookup works and the write does not, which is the path an outage in
	// the middle of the flow takes — and the one where the crossing looks
	// recorded from Hecate's side unless the failure is reported.
	f := &fakeFides{trail: attestTrail, attestStatus: http.StatusInternalServerError}
	server := f.start(t)

	step := &scripted{name: "a", results: []StepResult{ok("done")}}
	r, c, rec := attestController(t, []Runner{step},
		passageObj(v1alpha1.Step{Uses: "a"}), digestBundle(), gateWithEvidence(server), fidesSecret())

	advance(t, r)

	drainFor(t, rec, "AttestationFailed")
	if got := getPassage(t, c); got.Status.Evidence != nil && got.Status.Evidence.Trail != "" {
		t.Errorf("status.evidence.trail = %q — nothing was written, so pointing `hecate verify` "+
			"at a chain with no record on it would report a clean crossing that was never recorded",
			got.Status.Evidence.Trail)
	}
}

func TestAnArtifactFidesHasNeverSeenIsNotAttested(t *testing.T) {
	f := &fakeFides{} // no trail for the digest
	server := f.start(t)

	step := &scripted{name: "a", results: []StepResult{ok("done")}}
	r, c, rec := attestController(t, []Runner{step},
		passageObj(v1alpha1.Step{Uses: "a"}), digestBundle(), gateWithEvidence(server), fidesSecret())

	advance(t, r)

	if len(f.posted) != 0 {
		t.Errorf("attested %d times against a trail that does not exist", len(f.posted))
	}
	if got := getPassage(t, c); got.Status.Evidence != nil && got.Status.Evidence.Trail != "" {
		t.Errorf("status.evidence.trail = %q, want empty — nothing was recorded",
			got.Status.Evidence.Trail)
	}
	drainFor(t, rec, "AttestationSkipped")
}

func TestAttestingReusesTheTrailTheGatesWereJudgedOn(t *testing.T) {
	f := &fakeFides{trail: "a-different-trail"}
	server := f.start(t)

	// What an evidence-gate step leaves behind. The crossing must be recorded
	// on the trail that permitted it, not on whatever the artifact points at
	// by the time the Passage ends.
	step := &scripted{name: "a", results: []StepResult{{
		Phase:    v1alpha1.StepSucceeded,
		Message:  "cleared",
		Evidence: &v1alpha1.EvidenceRef{Trail: attestTrail, Verdict: "approve"},
	}}}
	r, c, _ := attestController(t, []Runner{step},
		passageObj(v1alpha1.Step{Uses: "a"}), digestBundle(), gateWithEvidence(server), fidesSecret())

	advance(t, r)

	if len(f.posted) != 1 {
		t.Fatalf("want one attestation, got %d", len(f.posted))
	}
	if got := f.posted[0]["trail_id"]; got != attestTrail {
		t.Errorf("trail_id = %v, want the trail the step judged (%s)", got, attestTrail)
	}
	if f.lookups != 0 {
		t.Errorf("looked the trail up %d times when the step had already found it", f.lookups)
	}
	if got := getPassage(t, c); got.Status.Evidence.Verdict != "approve" {
		t.Errorf("verdict = %q, want the step's verdict left alone", got.Status.Evidence.Verdict)
	}
}

// drain returns everything the recorder has, so a test can assert about the
// absence of an event as well as the presence of one.
func drain(rec *events.FakeRecorder) string {
	var all []string
	for {
		select {
		case e := <-rec.Events:
			all = append(all, e)
		default:
			return strings.Join(all, "\n")
		}
	}
}
