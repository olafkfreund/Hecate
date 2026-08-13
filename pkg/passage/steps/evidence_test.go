package steps

import (
	"context"
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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/fides"
	"github.com/olafkfreund/hecate/pkg/passage"
)

const (
	testEnv    = "7f3a1c2e-0000-4000-8000-000000009b04"
	testDigest = "sha256:abcdef0123456789"
	testTrail  = "91b2ffff-0000-4000-8000-00000000abcd"
)

// fidesServer answers the four gates, each configurable, and records which were
// asked.
type fidesServer struct {
	compliance string
	allowlist  string
	policy     string
	change     string
	artifacts  string
	asked      map[string]int
	status     int
	// reported holds the bodies POSTed to /artifacts.
	reported []map[string]any
	// approvals holds the bodies POSTed to a trail's /approvals.
	approvals []map[string]any
	// order is every call in the order it arrived, because some of these are
	// only correct in a particular order.
	order []string
}

func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("decoding a reported artifact: %v", err)
	}
	return body
}

func (f *fidesServer) start(t *testing.T) string {
	t.Helper()
	f.asked = map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.EscapedPath()
		if f.status != 0 {
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte("refused"))
			return
		}
		var body string
		switch {
		case strings.HasSuffix(path, "/compliance"):
			f.asked[GateAssert]++
			body = or(f.compliance, `{"compliant":true}`)
		case strings.HasSuffix(path, "/allowlist"):
			f.asked[GateAllowlist]++
			body = or(f.allowlist, `{"approved":true}`)
		case strings.HasSuffix(path, "/policy-check"):
			f.asked[GatePolicy]++
			body = or(f.policy, `{"compliant":true}`)
		case strings.HasSuffix(path, "/change-gate"):
			f.asked[GateChange]++
			f.order = append(f.order, "change")
			body = or(f.change, `{"recommendation":"approve","approved":true,"risk_score":5,"risk_level":"low"}`)
		case strings.HasSuffix(path, "/approvals"):
			f.order = append(f.order, "approval")
			f.approvals = append(f.approvals, decodeBody(t, r))
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"status":"approved"}`))
			return
		case strings.HasSuffix(path, "/artifacts") && r.Method == http.MethodPost:
			// Reporting, not looking up. Same path, different verb — the fake
			// has to tell them apart or a report would be answered with a list.
			f.reported = append(f.reported, decodeBody(t, r))
			w.WriteHeader(http.StatusCreated)
			return
		case strings.HasSuffix(path, "/artifacts"):
			f.asked["artifacts"]++
			body = or(f.artifacts, `[{"sha256":"abcdef0123456789","trail_id":"`+testTrail+`"}]`)
		default:
			t.Errorf("unexpected %s %s", r.Method, path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func or(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// evidenceStep wires the step to a Gate that names an environment, and a fake
// Fides.
func evidenceStep(t *testing.T, server string, evidence *v1alpha1.EvidenceConfig) *EvidenceGate {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if evidence == nil {
		evidence = &v1alpha1.EvidenceConfig{
			FidesEnvironment: testEnv,
			CredentialsRef:   &v1alpha1.LocalSecretRef{Name: "fides"},
		}
	}
	gate := &v1alpha1.Gate{
		ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: "acme"},
		Spec: v1alpha1.GateSpec{
			Admits:   []v1alpha1.Admission{{From: v1alpha1.BundleOrigin{Beacon: "app"}}},
			Evidence: evidence,
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "fides", Namespace: "acme"},
		Data:       map[string][]byte{"token": []byte("fides-key")},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gate, secret).Build()
	return NewEvidenceGate(c, server)
}

func evidenceCtx(t *testing.T, cfg EvidenceGateConfig, digest string) *passage.StepContext {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	artifacts := []v1alpha1.Artifact{}
	if digest != "" {
		artifacts = append(artifacts, v1alpha1.Artifact{Image: &v1alpha1.ImageArtifact{
			Repo: "ghcr.io/acme/api", Tag: "6.5.0", Digest: digest,
		}})
	}
	return &passage.StepContext{
		Namespace: "acme", Gate: "production", Passage: "production-abc123",
		Config: raw, StartedAt: time.Now(),
		Bundle: &v1alpha1.Bundle{
			ObjectMeta: metav1.ObjectMeta{Name: "podinfo-abc123", Namespace: "acme"},
			Spec:       v1alpha1.BundleSpec{Beacon: "app", Artifacts: artifacts},
		},
	}
}

func TestEvidenceGateRunsOnlyTheGatesSelected(t *testing.T) {
	f := &fidesServer{}
	step := evidenceStep(t, f.start(t), nil)

	res := mustRun(t, step, evidenceCtx(t, EvidenceGateConfig{
		Gates: []string{GateAssert, GateAllowlist},
	}, testDigest))

	if f.asked[GateAssert] != 1 || f.asked[GateAllowlist] != 1 {
		t.Errorf("asked = %v", f.asked)
	}
	// A dev Gate that selected only the digest gates must not pay for a trail
	// lookup, nor fail because CI never registered the artifact.
	if f.asked[GatePolicy] != 0 || f.asked[GateChange] != 0 || f.asked["artifacts"] != 0 {
		t.Errorf("unselected gates were consulted: %v", f.asked)
	}
	if res.Phase != v1alpha1.StepSucceeded {
		t.Errorf("phase = %s", res.Phase)
	}
}

func TestEvidenceGateAllFourGates(t *testing.T) {
	f := &fidesServer{}
	step := evidenceStep(t, f.start(t), nil)

	res := mustRun(t, step, evidenceCtx(t, EvidenceGateConfig{
		Gates: []string{GateAssert, GateAllowlist, GatePolicy, GateChange},
	}, testDigest))

	for _, g := range []string{GateAssert, GateAllowlist, GatePolicy, GateChange} {
		if f.asked[g] != 1 {
			t.Errorf("%s was asked %d times", g, f.asked[g])
		}
	}
	// The trail is the one CI built the artifact on, not one Hecate made up.
	if res.Output["trail"] != testTrail {
		t.Errorf("trail = %v", res.Output["trail"])
	}
	if res.Evidence == nil || res.Evidence.Trail != testTrail || res.Evidence.Verdict != "approve" {
		t.Errorf("evidence = %+v", res.Evidence)
	}
	if res.Evidence.Risk == nil || *res.Evidence.Risk != 5 {
		t.Errorf("risk = %v", res.Evidence.Risk)
	}
}

// The heart of the issue: a HOLD is a human not having signed off yet, which is
// the control working. Failing there would make the control unusable.
func TestEvidenceGateWaitsOnAHold(t *testing.T) {
	// Fides' own shape: missing_evidence is a list of objects, not of keys.
	f := &fidesServer{change: `{"recommendation":"hold","approved":false,"risk_score":30,
		"risk_level":"medium","missing_evidence":[{"control":"CC6.1","name":"Change approval",
		"reasons":["missing sbom"]}]}`}
	step := evidenceStep(t, f.start(t), nil)

	res, err := step.Run(context.Background(), evidenceCtx(t, EvidenceGateConfig{
		Gates:        []string{GateChange},
		PollInterval: &metav1.Duration{Duration: 30 * time.Second},
	}, testDigest))
	if err != nil {
		t.Fatalf("a hold is not a failure: %v", err)
	}

	if res.Phase != v1alpha1.StepRunning {
		t.Fatalf("phase = %s, want Running", res.Phase)
	}
	if res.RetryAfter != 30*time.Second {
		t.Errorf("retryAfter = %s", res.RetryAfter)
	}
	// The message has to say what would unblock it, or somebody has to go and
	// read Fides to find out why their promotion is sitting there.
	if !strings.Contains(res.Message, "sbom") {
		t.Errorf("message does not say what is missing: %q", res.Message)
	}
	// Even while waiting, the verdict is on the Passage — that is what a
	// dashboard shows and what an auditor reads.
	if res.Evidence == nil || res.Evidence.Verdict != "hold" {
		t.Errorf("evidence = %+v", res.Evidence)
	}
}

// Waiting forever is as wrong as failing immediately (D6).
func TestEvidenceGateGivesUpOnALongHold(t *testing.T) {
	f := &fidesServer{change: `{"recommendation":"hold","approved":false,"risk_score":30}`}
	step := evidenceStep(t, f.start(t), nil)

	sc := evidenceCtx(t, EvidenceGateConfig{
		Gates:       []string{GateChange},
		HoldTimeout: &metav1.Duration{Duration: time.Hour},
	}, testDigest)
	sc.StartedAt = time.Now().Add(-3 * time.Hour)

	_, err := step.Run(context.Background(), sc)
	if !passage.IsTerminal(err) || passage.ReasonOf(err) != ReasonChangeHeld {
		t.Fatalf("err = %v, reason = %s", err, passage.ReasonOf(err))
	}
	if !strings.Contains(err.Error(), "holdTimeout") {
		t.Errorf("the error does not say why it gave up: %v", err)
	}
}

// A team may be stricter than Fides' own verdict. Waiting cannot lower a score
// that is about evidence which already exists, so this is terminal.
func TestEvidenceGateMaxRisk(t *testing.T) {
	f := &fidesServer{change: `{"recommendation":"approve","approved":true,"risk_score":60,"risk_level":"high"}`}
	step := evidenceStep(t, f.start(t), nil)
	max := int32(40)

	_, err := step.Run(context.Background(), evidenceCtx(t, EvidenceGateConfig{
		Gates: []string{GateChange}, MaxRisk: &max,
	}, testDigest))

	if !passage.IsTerminal(err) || passage.ReasonOf(err) != ReasonNotCompliant {
		t.Fatalf("err = %v, reason = %s", err, passage.ReasonOf(err))
	}
	if !strings.Contains(err.Error(), "60") || !strings.Contains(err.Error(), "40") {
		t.Errorf("the error should name both scores: %v", err)
	}
}

func TestEvidenceGateRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		server fidesServer
		cfg    EvidenceGateConfig
		digest string
		reason string
		says   string
	}{
		{
			"a non-compliant artifact",
			fidesServer{compliance: `{"compliant":false,"violations":["no SBOM","critical CVE"]}`},
			EvidenceGateConfig{Gates: []string{GateAssert}}, testDigest,
			ReasonNotCompliant, "critical CVE",
		},
		{
			"an artifact not approved for the environment",
			fidesServer{allowlist: `{"approved":false}`},
			EvidenceGateConfig{Gates: []string{GateAllowlist}}, testDigest,
			ReasonNotAllowlisted, "fides allowlist add",
		},
		{
			"an environment policy that is not satisfied",
			fidesServer{policy: `{"compliant":false,"results":[
				{"policy":"production-requires-sbom","applies":true,"missing":["sbom"]}]}`},
			EvidenceGateConfig{Gates: []string{GatePolicy}}, testDigest,
			ReasonNotCompliant, "production-requires-sbom",
		},
		{
			// CI never told Fides about this artifact, so there is no evidence.
			// Passing would mean the gate approves exactly what it cannot see.
			"an artifact Fides has never seen",
			fidesServer{artifacts: `[]`},
			EvidenceGateConfig{Gates: []string{GateChange}}, testDigest,
			ReasonNoEvidence, "no record of",
		},
		{
			"a Bundle with no digests",
			fidesServer{},
			EvidenceGateConfig{Gates: []string{GateAssert}}, "",
			ReasonNoEvidence, "no image digests",
		},
		{
			"no gates selected",
			fidesServer{},
			EvidenceGateConfig{}, testDigest,
			ReasonInvalidConfig, "checks nothing",
		},
		{
			"a gate that does not exist",
			fidesServer{},
			EvidenceGateConfig{Gates: []string{"vibes"}}, testDigest,
			ReasonInvalidConfig, "no gate named",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := tc.server
			step := evidenceStep(t, server.start(t), nil)

			_, err := step.Run(context.Background(), evidenceCtx(t, tc.cfg, tc.digest))
			if !passage.IsTerminal(err) {
				t.Fatalf("err = %v, want a terminal refusal", err)
			}
			if passage.ReasonOf(err) != tc.reason {
				t.Errorf("reason = %s, want %s", passage.ReasonOf(err), tc.reason)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("error does not mention %q: %v", tc.says, err)
			}
		})
	}
}

// A compliance system that restarted must not permanently block every
// promotion, but a rejected token will not start working on its own.
func TestEvidenceGateSeparatesOutageFromRefusal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		terminal bool
	}{
		{"an outage", http.StatusServiceUnavailable, false},
		{"a rejected token", http.StatusUnauthorized, true},
		{"a forbidden token", http.StatusForbidden, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fidesServer{status: tc.status}
			step := evidenceStep(t, f.start(t), nil)

			_, err := step.Run(context.Background(),
				evidenceCtx(t, EvidenceGateConfig{Gates: []string{GateAssert}}, testDigest))
			if err == nil {
				t.Fatal("expected a failure")
			}
			if passage.IsTerminal(err) != tc.terminal {
				t.Errorf("terminal = %v, want %v (%v)", passage.IsTerminal(err), tc.terminal, err)
			}
			if passage.ReasonOf(err) != ReasonEvidenceUnavailable {
				t.Errorf("reason = %s", passage.ReasonOf(err))
			}
		})
	}
}

// A Gate with no environment cannot run an environment-scoped check, and
// guessing one would check the wrong environment's policy (D27).
func TestEvidenceGateNeedsAnEnvironment(t *testing.T) {
	f := &fidesServer{}
	step := evidenceStep(t, f.start(t), &v1alpha1.EvidenceConfig{
		CredentialsRef: &v1alpha1.LocalSecretRef{Name: "fides"},
	})

	_, err := step.Run(context.Background(),
		evidenceCtx(t, EvidenceGateConfig{Gates: []string{GateAllowlist}}, testDigest))
	if !passage.IsTerminal(err) || !strings.Contains(err.Error(), "fidesEnvironment") {
		t.Errorf("err = %v", err)
	}
}

func TestEvidenceGateReadsItsTokenFromASecret(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"compliant":true}`))
	}))
	t.Cleanup(srv.Close)

	step := evidenceStep(t, srv.URL, &v1alpha1.EvidenceConfig{
		FidesEnvironment: testEnv,
		CredentialsRef:   &v1alpha1.LocalSecretRef{Name: "fides"},
	})
	mustRun(t, step, evidenceCtx(t, EvidenceGateConfig{Gates: []string{GateAssert}}, testDigest))

	if auth != "Bearer fides-key" {
		t.Errorf("Authorization = %q", auth)
	}
}

// The Gate's own server wins over the controller default, so one cluster can
// talk to more than one Fides.
func TestEvidenceGatePrefersTheGatesServer(t *testing.T) {
	wrong := (&fidesServer{}).start(t)
	right := &fidesServer{}
	step := evidenceStep(t, wrong, &v1alpha1.EvidenceConfig{
		FidesEnvironment: testEnv, ServerURL: right.start(t),
		CredentialsRef: &v1alpha1.LocalSecretRef{Name: "fides"},
	})
	step.dial = fides.New

	mustRun(t, step, evidenceCtx(t, EvidenceGateConfig{Gates: []string{GateAssert}}, testDigest))
	if right.asked[GateAssert] != 1 {
		t.Errorf("the Gate's own server was not used: %v", right.asked)
	}
}

// Reporting links every image in the Bundle to the trail, so a change gate
// judges the release rather than the one image whose trail happened to be
// looked up.
func TestEvidenceGateReportsTheBundlesArtifacts(t *testing.T) {
	fake := &fidesServer{}
	server := fake.start(t)

	cfg := EvidenceGateConfig{Gates: []string{GateChange}, ReportArtifacts: true}
	sc := evidenceCtx(t, cfg, testDigest)
	// A second image from the same build, which is the case reporting exists
	// for: without it the gate never sees this one.
	sc.Bundle.Spec.Artifacts = append(sc.Bundle.Spec.Artifacts, v1alpha1.Artifact{
		Image: &v1alpha1.ImageArtifact{Repo: "ghcr.io/acme/worker", Digest: "sha256:beef00"},
	})

	res, err := evidenceStep(t, server, nil).Run(context.Background(), sc)
	if err != nil {
		t.Fatal(err)
	}
	if res.Output["artifactsReported"] != 2 {
		t.Errorf("reported = %v, want 2", res.Output["artifactsReported"])
	}
	if len(fake.reported) != 2 {
		t.Fatalf("the server received %d reports", len(fake.reported))
	}
	// Every report carries the trail. One without would detach CI's evidence.
	for _, r := range fake.reported {
		if r["trail_id"] != testTrail {
			t.Errorf("reported %v with trail %v, want %s", r["sha256"], r["trail_id"], testTrail)
		}
		if r["type"] != "container-image" {
			t.Errorf("type = %v", r["type"])
		}
	}
}

// Off by default: reporting claims the other images belong to this trail, and
// only the operator knows whether one CI run built them all.
func TestEvidenceGateDoesNotReportUnlessAsked(t *testing.T) {
	fake := &fidesServer{}
	server := fake.start(t)

	sc := evidenceCtx(t, EvidenceGateConfig{Gates: []string{GateChange}}, testDigest)

	if _, err := evidenceStep(t, server, nil).Run(context.Background(), sc); err != nil {
		t.Fatal(err)
	}
	if len(fake.reported) != 0 {
		t.Errorf("reported %d artifacts without being asked", len(fake.reported))
	}
}

// The third identity Fides needs. Without it the change gate holds on "no
// deployer recorded" — a verdict about missing evidence that reads like a
// policy failure.
func TestTheChangeGateRecordsWhoIsDeploying(t *testing.T) {
	fake := &fidesServer{}
	server := fake.start(t)

	sc := evidenceCtx(t, EvidenceGateConfig{Gates: []string{GateChange}}, testDigest)
	sc.Actor = "olaf@acme.example"

	if _, err := evidenceStep(t, server, nil).Run(context.Background(), sc); err != nil {
		t.Fatal(err)
	}

	if len(fake.approvals) != 1 {
		t.Fatalf("recorded %d deployers, want one", len(fake.approvals))
	}
	a := fake.approvals[0]
	if a["role"] != fides.RoleDeployer {
		t.Errorf("role = %v, want %s", a["role"], fides.RoleDeployer)
	}
	if a["on_behalf_of"] != "olaf@acme.example" {
		t.Errorf("on_behalf_of = %v, want the actor who asked for the crossing", a["on_behalf_of"])
	}
	if fake.order[len(fake.order)-1] != "change" {
		t.Errorf("call order was %v — the deployer must be recorded before the verdict is "+
			"read, or the verdict is the one that says nobody is deploying", fake.order)
	}
}

// Hecate is not a person. Recording it as the deployer would let a change pass
// four-eyes with two humans and a robot.
func TestAnAutomaticCrossingRecordsNoDeployer(t *testing.T) {
	fake := &fidesServer{}
	server := fake.start(t)

	sc := evidenceCtx(t, EvidenceGateConfig{Gates: []string{GateChange}}, testDigest)
	sc.Actor = passage.ActorController

	if _, err := evidenceStep(t, server, nil).Run(context.Background(), sc); err != nil {
		t.Fatal(err)
	}
	if len(fake.approvals) != 0 {
		t.Errorf("an automatic crossing recorded %v as a human deployer", fake.approvals)
	}
}
