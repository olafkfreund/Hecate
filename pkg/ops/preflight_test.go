package ops

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// policyFides answers the artifact lookup and the policy check.
type policyFides struct {
	// trail is what the artifact lookup returns; empty means Fides has never
	// heard of the digest.
	trail string
	// verdict is the policy-check body.
	verdict string
	// verdictStatus fails only the policy check.
	verdictStatus int
	// asked records the policy-check paths, so a test can assert which
	// environment and trail were actually asked about rather than only that an
	// answer came back.
	asked []string
}

func (f *policyFides) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.EscapedPath()
		switch {
		case strings.Contains(path, "/policy-check"):
			f.asked = append(f.asked, path+"?"+r.URL.RawQuery)
			if f.verdictStatus != 0 {
				w.WriteHeader(f.verdictStatus)
				_, _ = w.Write([]byte("unavailable"))
				return
			}
			_, _ = w.Write([]byte(f.verdict))
		case strings.HasSuffix(path, "/artifacts"):
			if f.trail == "" {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[{"sha256":"` + strings.TrimPrefix(sodDigest, "sha256:") +
				`","trail_id":"` + f.trail + `"}]`))
		default:
			t.Errorf("unexpected request to %s", path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// eligibleGate is a Gate with evidence configured and one Bundle waiting.
func eligibleGate(server string, eligible ...string) *v1alpha1.Gate {
	g := testGate("production")
	withEvidence(server)(g)
	g.Status.Eligible = eligible
	return g
}

func TestPreflightSaysABundleWouldCross(t *testing.T) {
	f := &policyFides{trail: sodTrail, verdict: `{"compliant":true,"results":[]}`}
	o, _ := evidenceOps(t, eligibleGate(f.start(t), "b1"), bundleWithDigest("b1"), sodSecret())

	got, err := o.Preflight(context.Background(), "acme", "production")
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 || !got[0].Compliant {
		t.Fatalf("preflight says %+v, want one compliant Bundle", got)
	}
	if got[0].Unknown != "" {
		t.Errorf("a compliant answer also carries %q", got[0].Unknown)
	}
}

func TestPreflightNamesWhatIsMissing(t *testing.T) {
	// Two policies short of the same attestation, and a third short of another.
	f := &policyFides{trail: sodTrail, verdict: `{"compliant":false,"results":[
		{"policy":"change-control","applies":true,"missing":["servicenow-change"]},
		{"policy":"sox","applies":true,"missing":["servicenow-change","sbom"]}
	]}`}
	o, _ := evidenceOps(t, eligibleGate(f.start(t), "b1"), bundleWithDigest("b1"), sodSecret())

	got, err := o.Preflight(context.Background(), "acme", "production")
	if err != nil {
		t.Fatal(err)
	}

	if got[0].Compliant {
		t.Fatal("a non-compliant verdict was reported as compliant")
	}
	// Deduplicated: one missing attestation failing two policies is one thing
	// to fix, not two.
	if len(got[0].Missing) != 2 ||
		got[0].Missing[0] != "sbom" || got[0].Missing[1] != "servicenow-change" {
		t.Errorf("missing is %v, want [sbom servicenow-change] sorted and deduplicated", got[0].Missing)
	}
	// And which rules stopped it, which is a different question.
	if len(got[0].Policies) != 2 {
		t.Errorf("policies is %v, want both named", got[0].Policies)
	}
}

func TestPreflightIgnoresAPolicyThatDoesNotApply(t *testing.T) {
	f := &policyFides{trail: sodTrail, verdict: `{"compliant":true,"results":[
		{"policy":"pci","applies":false,"missing":["pci-scan"]}
	]}`}
	o, _ := evidenceOps(t, eligibleGate(f.start(t), "b1"), bundleWithDigest("b1"), sodSecret())

	got, err := o.Preflight(context.Background(), "acme", "production")
	if err != nil {
		t.Fatal(err)
	}

	// A policy conditional on a flow tag the trail does not carry is silent in
	// both directions. Listing its requirements would send someone to produce
	// an attestation nothing is asking for.
	if len(got[0].Missing) != 0 || len(got[0].Policies) != 0 {
		t.Errorf("an inapplicable policy leaked into %+v", got[0])
	}
}

func TestPreflightAsksAboutTheGatesEnvironmentAndTheBundlesTrail(t *testing.T) {
	f := &policyFides{trail: sodTrail, verdict: `{"compliant":true,"results":[]}`}
	o, _ := evidenceOps(t, eligibleGate(f.start(t), "b1"), bundleWithDigest("b1"), sodSecret())

	if _, err := o.Preflight(context.Background(), "acme", "production"); err != nil {
		t.Fatal(err)
	}

	// Asking about the wrong environment answers a question nobody asked, and
	// answers it plausibly — every policy would come back satisfied because
	// they are somebody else's policies.
	if len(f.asked) != 1 {
		t.Fatalf("asked %v", f.asked)
	}
	if !strings.Contains(f.asked[0], sodEnv) {
		t.Errorf("asked %q, which does not name the Gate's environment", f.asked[0])
	}
	if !strings.Contains(f.asked[0], sodTrail) {
		t.Errorf("asked %q, which does not name the Bundle's trail", f.asked[0])
	}
}

func TestPreflightDoesNotCallAnyoneWhenTheGateDoesNotGateOnEvidence(t *testing.T) {
	g := testGate("production")
	g.Status.Eligible = []string{"b1"}
	// No Spec.Evidence at all.
	o, _ := evidenceOps(t, g, bundleWithDigest("b1"))

	got, err := o.Preflight(context.Background(), "acme", "production")

	// Most Gates do not gate on evidence, and reporting that as a failure would
	// be wrong about almost every Gate.
	if err != nil {
		t.Fatalf("a Gate without evidence returned an error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want nothing to report", got)
	}
}

func TestPreflightSaysUnknownRatherThanCompliant(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fides *policyFides
		want  string
	}{
		{
			name:  "no trail yet",
			fides: &policyFides{trail: ""},
			want:  "no Fides trail",
		},
		{
			name:  "Fides refused the question",
			fides: &policyFides{trail: sodTrail, verdictStatus: http.StatusInternalServerError},
			want:  "asking Fides",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, _ := evidenceOps(t, eligibleGate(tc.fides.start(t), "b1"), bundleWithDigest("b1"), sodSecret())

			got, err := o.Preflight(context.Background(), "acme", "production")
			if err != nil {
				t.Fatal(err)
			}

			// A Bundle whose evidence could not be checked is not a Bundle that
			// passed. Rendering the two the same way is how a page starts lying,
			// and this is the one it would lie in favour of.
			if got[0].Compliant {
				t.Error("an unanswerable check was reported as compliant")
			}
			if !strings.Contains(got[0].Unknown, tc.want) {
				t.Errorf("unknown is %q, want it to mention %q", got[0].Unknown, tc.want)
			}
		})
	}
}

func TestPreflightReportsABundleWithNoDigest(t *testing.T) {
	f := &policyFides{trail: sodTrail, verdict: `{"compliant":true}`}
	// A Bundle with no image artifact — nothing for evidence to be about.
	o, _ := evidenceOps(t, eligibleGate(f.start(t), "b1"), testBundle("b1", 0), sodSecret())

	got, err := o.Preflight(context.Background(), "acme", "production")
	if err != nil {
		t.Fatal(err)
	}

	if got[0].Compliant || !strings.Contains(got[0].Unknown, "no image digest") {
		t.Errorf("got %+v, want an unknown naming the missing digest", got[0])
	}
}

// bundleWithDigest is testBundle carrying the digest the fake Fides knows.
func bundleWithDigest(name string) *v1alpha1.Bundle {
	b := testBundle(name, 0)
	withDigest(b)
	return b
}
