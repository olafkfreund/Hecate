//go:build fides

// Package integration exercises Hecate against a real Fides server.
//
// The unit tests use a fake Fides built from reading its handlers. That catches
// a lot, but not the thing most likely to be wrong: whether the shapes we parse
// are the shapes it actually sends. It has already caught us out once —
// policy-check returns `compliant`, not `passed`, and a client reading the
// wrong key would have failed every check while looking like it worked.
//
// Run against a live server:
//
//	FIDES_SERVER_URL=http://127.0.0.1:18080 FIDES_TOKEN=… \
//	  go test -tags fides ./test/integration/...
//
// Nothing here writes. Every call is a read, so it is safe to point at a real
// compliance system — which is the only kind there is, since a Fides with no
// history has nothing to verify.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
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
	"github.com/olafkfreund/hecate/pkg/passage/steps"
)

const namespace = "hecate-integration"

func client(t *testing.T) *fides.Client {
	t.Helper()
	server, token := os.Getenv("FIDES_SERVER_URL"), os.Getenv("FIDES_TOKEN")
	if server == "" || token == "" {
		t.Skip("set FIDES_SERVER_URL and FIDES_TOKEN to run against a real Fides")
	}
	c, err := fides.New(fides.Config{BaseURL: server, Token: token})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// subject is a real artifact on the server, and the trail it was built on.
//
// Discovered rather than hardcoded: the UUIDs differ per instance, and a test
// pinned to one person's cluster is a test nobody else can run.
type subject struct {
	Digest string
	Trail  string
	Env    string
}

func find(t *testing.T, c *fides.Client) subject {
	t.Helper()
	ctx := context.Background()

	var artifacts []struct {
		SHA256  string  `json:"sha256"`
		TrailID *string `json:"trail_id"`
	}
	if err := get(ctx, t, "api/v1/artifacts", &artifacts); err != nil {
		t.Fatalf("listing artifacts: %v", err)
	}
	var found subject
	for _, a := range artifacts {
		if a.TrailID != nil && *a.TrailID != "" {
			found.Digest, found.Trail = a.SHA256, *a.TrailID
			break
		}
	}
	if found.Digest == "" {
		t.Skip("this Fides has no artifact with a trail, so there is nothing to gate on")
	}

	var environments []struct {
		ID string `json:"id"`
	}
	if err := get(ctx, t, "api/v1/environments", &environments); err != nil {
		t.Fatalf("listing environments: %v", err)
	}
	if len(environments) == 0 {
		t.Skip("this Fides has no environments")
	}
	found.Env = environments[0].ID
	return found
}

// get is a raw read, for the discovery the client deliberately does not expose:
// listing every artifact is a thing this test needs and a promotion does not.
func get(ctx context.Context, t *testing.T, path string, out any) error {
	t.Helper()
	base := strings.TrimSuffix(os.Getenv("FIDES_SERVER_URL"), "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/"+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+os.Getenv("FIDES_TOKEN"))
	req.Header.Set("Accept", "application/json")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// The client's understanding of every response it parses, checked against what
// the server actually sends.
func TestClientParsesWhatFidesSends(t *testing.T) {
	c := client(t)
	s := find(t, c)
	ctx := context.Background()

	t.Run("the artifact's trail is resolvable by digest", func(t *testing.T) {
		got, err := c.TrailForArtifact(ctx, s.Digest)
		if err != nil {
			t.Fatal(err)
		}
		if got != s.Trail {
			t.Errorf("trail = %q, want %q", got, s.Trail)
		}
		// The prefixed form is what a Bundle carries.
		got, err = c.TrailForArtifact(ctx, "sha256:"+s.Digest)
		if err != nil || got != s.Trail {
			t.Errorf("a sha256:-prefixed digest resolved to %q (%v)", got, err)
		}
	})

	t.Run("the chain verifies", func(t *testing.T) {
		chain, err := c.VerifyChain(ctx, s.Trail)
		if err != nil {
			t.Fatal(err)
		}
		if chain.Count == 0 {
			t.Error("count = 0 — the trail has no attestations, or we parsed the wrong field")
		}
		if chain.Valid && chain.BrokenAt != -1 {
			t.Errorf("a valid chain reported brokenAt %d", chain.BrokenAt)
		}
		t.Logf("chain: valid=%v count=%d anchored=%v", chain.Valid, chain.Count, chain.ExternalAnchor != nil)
	})

	t.Run("the change gate returns a verdict and a score", func(t *testing.T) {
		verdict, err := c.ChangeGate(ctx, s.Trail)
		if err != nil {
			t.Fatal(err)
		}
		// The two fields the step branches on. A recommendation we did not
		// parse would read as "hold" forever; a score we did not parse would
		// read as 0 and pass every maxRisk.
		if verdict.Recommendation != "approve" && verdict.Recommendation != "hold" {
			t.Errorf("recommendation = %q, want approve or hold", verdict.Recommendation)
		}
		if verdict.RiskScore < 0 || verdict.RiskScore > 100 {
			t.Errorf("risk score %d is outside 0-100", verdict.RiskScore)
		}
		// A held verdict must always say something actionable, or an operator
		// has to go and read Fides to find out why their promotion is stuck.
		if verdict.Held() && len(verdict.Blockers()) == 0 {
			t.Error("a held verdict named nothing that would unblock it")
		}
		t.Logf("change gate: %s risk=%d blockers=%v",
			verdict.Recommendation, verdict.RiskScore, verdict.Blockers())
	})

	t.Run("the environment policy answers", func(t *testing.T) {
		verdict, err := c.PolicyCheck(ctx, s.Env, s.Trail)
		if err != nil {
			t.Fatal(err)
		}
		// `compliant`, not `passed` — the field name this test exists to pin.
		// A misparse here reads as "not compliant" and refuses every crossing.
		if !verdict.Compliant && len(verdict.Unmet()) == 0 && len(verdict.Results) == 0 {
			t.Error("not compliant, but no policy said why — likely a misparsed field")
		}
		t.Logf("policy: compliant=%v unmet=%v", verdict.Compliant, verdict.Unmet())
	})

	t.Run("the allowlist answers", func(t *testing.T) {
		// Either answer is fine; that it parses is the point.
		approved, err := c.Allowlisted(ctx, s.Env, s.Digest)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("allowlist: approved=%v", approved)
	})

	t.Run("assert answers", func(t *testing.T) {
		out, err := c.Assert(ctx, s.Digest, "")
		if err != nil {
			t.Fatal(err)
		}
		if !out.Compliant && len(out.Violations) == 0 {
			t.Error("non-compliant with no violations listed — the refusal would be unexplainable")
		}
		t.Logf("assert: compliant=%v violations=%v", out.Compliant, out.Violations)
	})
}

func TestErrorsAreClassifiableAgainstARealServer(t *testing.T) {
	c := client(t)
	ctx := context.Background()

	// Fides answers 200 with {"valid":true,"count":0} for a trail it has never
	// heard of, because verifying an empty chain is vacuously true. Anything
	// reading `valid` alone would call a nonexistent trail verified — which is
	// how `hecate verify` reported exit 0 on one until this test caught it.
	chain, err := c.VerifyChain(ctx, "00000000-0000-4000-8000-000000000000")
	switch {
	case err == nil && chain.Count == 0:
		t.Logf("an unknown trail verifies vacuously (valid=%v count=0) — "+
			"callers must not treat that as verified", chain.Valid)
	case err == nil:
		t.Errorf("an unknown trail returned %d attestations", chain.Count)
	case !fides.IsNotFound(err):
		t.Logf("an unknown trail answered: %v", err)
	}

	// A wrong token must be distinguishable from an outage: one needs a new
	// credential, the other needs waiting.
	bad, err := fides.New(fides.Config{BaseURL: os.Getenv("FIDES_SERVER_URL"), Token: "not-a-real-token"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.VerifyChain(ctx, "00000000-0000-4000-8000-000000000000"); !fides.IsAuth(err) {
		t.Errorf("a rejected token was not recognised as an auth failure: %v", err)
	}
}

// The step, end to end, against real verdicts.
//
// The Kubernetes side is faked — the Gate and Secret are objects, and building
// them is not what this test is about — but every Fides answer is real.
func TestEvidenceGateAgainstRealFides(t *testing.T) {
	c := client(t)
	s := find(t, c)
	ctx := context.Background()

	verdict, err := c.ChangeGate(ctx, s.Trail)
	if err != nil {
		t.Fatal(err)
	}
	allowlisted, err := c.Allowlisted(ctx, s.Env, s.Digest)
	if err != nil {
		t.Fatal(err)
	}

	step, sc := gateStep(t, s)

	t.Run("the change gate", func(t *testing.T) {
		res, err := step.Run(ctx, withGates(t, sc, steps.GateChange))

		switch {
		case verdict.Held():
			// D30: a hold is a human not having signed off, which is the
			// control working. The crossing waits.
			if err != nil {
				t.Fatalf("a held change must not fail the crossing: %v", err)
			}
			if res.Phase != v1alpha1.StepRunning {
				t.Fatalf("phase = %s, want Running for a held change", res.Phase)
			}
			if !strings.Contains(res.Message, "holding") {
				t.Errorf("message = %q", res.Message)
			}
			if res.Evidence == nil || res.Evidence.Verdict != verdict.Recommendation {
				t.Errorf("evidence = %+v, want the real verdict recorded while waiting", res.Evidence)
			}
			t.Logf("held as expected: %s", res.Message)

		default:
			if err != nil {
				t.Fatalf("an approved change failed: %v", err)
			}
			if res.Phase != v1alpha1.StepSucceeded {
				t.Errorf("phase = %s", res.Phase)
			}
		}
	})

	t.Run("the allowlist", func(t *testing.T) {
		_, err := step.Run(ctx, withGates(t, sc, steps.GateAllowlist))

		if allowlisted {
			if err != nil {
				t.Fatalf("an allowlisted digest was refused: %v", err)
			}
			return
		}
		// The refusal has to be terminal and has to say how to fix it.
		if !passage.IsTerminal(err) {
			t.Fatalf("err = %v, want a terminal refusal", err)
		}
		if passage.ReasonOf(err) != steps.ReasonNotAllowlisted {
			t.Errorf("reason = %s", passage.ReasonOf(err))
		}
		if !strings.Contains(err.Error(), "fides allowlist add") {
			t.Errorf("the refusal does not say how to approve it: %v", err)
		}
		t.Logf("refused as expected: %v", err)
	})

	t.Run("a digest Fides has never seen", func(t *testing.T) {
		unknown := withGates(t, sc, steps.GateChange)
		unknown.Bundle.Spec.Artifacts[0].Image.Digest =
			"sha256:0000000000000000000000000000000000000000000000000000000000000000"

		_, err := step.Run(ctx, unknown)
		// Approving what cannot be seen is the failure the system exists to
		// prevent, so this is a refusal rather than a pass.
		if !passage.IsTerminal(err) || passage.ReasonOf(err) != steps.ReasonNoEvidence {
			t.Errorf("err = %v, reason = %s", err, passage.ReasonOf(err))
		}
	})
}

// gateStep builds the step with a Gate bound to the discovered environment.
func gateStep(t *testing.T, s subject) (*steps.EvidenceGate, *passage.StepContext) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	gate := &v1alpha1.Gate{
		ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: namespace},
		Spec: v1alpha1.GateSpec{
			Admits: []v1alpha1.Admission{{From: v1alpha1.BundleOrigin{Beacon: "app"}}},
			Evidence: &v1alpha1.EvidenceConfig{
				FidesEnvironment: s.Env,
				ServerURL:        os.Getenv("FIDES_SERVER_URL"),
				CredentialsRef:   &v1alpha1.LocalSecretRef{Name: "fides"},
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "fides", Namespace: namespace},
		Data:       map[string][]byte{"token": []byte(os.Getenv("FIDES_TOKEN"))},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gate, secret).Build()

	sc := &passage.StepContext{
		Namespace: namespace, Gate: "production", Passage: "production-integration",
		StartedAt: time.Now(),
		Bundle: &v1alpha1.Bundle{
			ObjectMeta: metav1.ObjectMeta{Name: "integration-bundle", Namespace: namespace},
			Spec: v1alpha1.BundleSpec{Beacon: "app", Artifacts: []v1alpha1.Artifact{
				{Image: &v1alpha1.ImageArtifact{Repo: "ghcr.io/acme/app", Digest: "sha256:" + s.Digest}},
			}},
		},
	}
	return steps.NewEvidenceGate(kube, ""), sc
}

// withGates copies the context with a fresh config, so subtests do not share one.
func withGates(t *testing.T, sc *passage.StepContext, gates ...string) *passage.StepContext {
	t.Helper()
	raw, err := json.Marshal(steps.EvidenceGateConfig{Gates: gates})
	if err != nil {
		t.Fatal(err)
	}
	copied := *sc
	copied.Config = raw
	copied.Bundle = sc.Bundle.DeepCopy()
	return &copied
}
