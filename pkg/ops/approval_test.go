package ops

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

const (
	sodEnv    = "7f3a1c2e-0000-4000-8000-000000009b04"
	sodDigest = "sha256:abcdef0123456789"
	sodTrail  = "91b2ffff-0000-4000-8000-00000000abcd"
)

// sodFides answers the artifact lookup and records the approvals posted to it.
type sodFides struct {
	// trail is what the lookup returns; empty means Fides has never heard of
	// the digest.
	trail string
	// approvalStatus fails only the approval write.
	approvalStatus int
	// changeGate is the verdict body, and changeGateStatus fails only that read.
	changeGate       string
	changeGateStatus int

	approvals []map[string]any
}

func (f *sodFides) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.EscapedPath(), "/approvals"):
			if f.approvalStatus != 0 {
				w.WriteHeader(f.approvalStatus)
				_, _ = w.Write([]byte("refused"))
				return
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decoding the approval: %v", err)
			}
			body["path"] = r.URL.EscapedPath()
			f.approvals = append(f.approvals, body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"status":"approved"}`))
		case strings.HasSuffix(r.URL.EscapedPath(), "/change-gate"):
			if f.changeGateStatus != 0 {
				w.WriteHeader(f.changeGateStatus)
				_, _ = w.Write([]byte("unavailable"))
				return
			}
			_, _ = w.Write([]byte(f.changeGate))
		case strings.HasSuffix(r.URL.EscapedPath(), "/artifacts"):
			if f.trail == "" {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[{"sha256":"` + strings.TrimPrefix(sodDigest, "sha256:") +
				`","trail_id":"` + f.trail + `"}]`))
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// evidenceOps is newOps with a scheme that knows Secrets, which resolving the
// Fides credentials needs.
func evidenceOps(t *testing.T, objs ...client.Object) (*Ops, client.Client) {
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
		WithStatusSubresource(&v1alpha1.Gate{}, &v1alpha1.Bundle{}, &v1alpha1.Passage{}).
		Build()
	return &Ops{Client: c, Now: func() metav1.Time { return metav1.Time{Time: base} }}, c
}

func withEvidence(server string) func(*v1alpha1.Gate) {
	return func(g *v1alpha1.Gate) {
		g.Spec.Evidence = &v1alpha1.EvidenceConfig{
			FidesEnvironment: sodEnv,
			ServerURL:        server,
			CredentialsRef:   &v1alpha1.LocalSecretRef{Name: "fides"},
		}
	}
}

func withDigest(b *v1alpha1.Bundle) {
	b.Spec.Artifacts = []v1alpha1.Artifact{{Image: &v1alpha1.ImageArtifact{
		Repo: "ghcr.io/acme/app", Tag: "1.0.0", Digest: sodDigest,
	}}}
}

func sodSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "fides", Namespace: "acme"},
		Data:       map[string][]byte{"token": []byte("t0ken")},
	}
}

func TestApprovalRecordsWhoGaveIt(t *testing.T) {
	o, c := newOps(t, testGate("staging"), testBundle("b1", 0))
	ctx := context.Background()

	if err := o.Approve(ctx, "acme", "b1", "staging", "olaf@acme.example"); err != nil {
		t.Fatal(err)
	}

	var b v1alpha1.Bundle
	if err := c.Get(ctx, client.ObjectKey{Namespace: "acme", Name: "b1"}, &b); err != nil {
		t.Fatal(err)
	}
	got := b.ApprovalFor("staging")
	if got == nil {
		t.Fatal("the approval was not recorded")
	}
	if got.Actor != "olaf@acme.example" {
		t.Errorf("actor = %q — an approval that does not say who gave it cannot satisfy "+
			"four-eyes anywhere downstream", got.Actor)
	}
	if got.At.IsZero() {
		t.Error("the approval has no timestamp")
	}
}

func TestApprovalIsRecordedInFides(t *testing.T) {
	f := &sodFides{trail: sodTrail}
	server := f.start(t)
	o, _ := evidenceOps(t,
		testGate("staging", withEvidence(server)), testBundle("b1", 0, withDigest), sodSecret())

	if err := o.Approve(context.Background(), "acme", "b1", "staging", "olaf@acme.example"); err != nil {
		t.Fatal(err)
	}

	if len(f.approvals) != 1 {
		t.Fatalf("want one approval recorded in Fides, got %d — the change gate would go on "+
			"reporting a missing sign-off", len(f.approvals))
	}
	a := f.approvals[0]
	if got := a["role"]; got != "approver" {
		t.Errorf("role = %v, want approver", got)
	}
	if got := a["on_behalf_of"]; got != "olaf@acme.example" {
		t.Errorf("on_behalf_of = %v — without it Fides attributes the approval to Hecate's "+
			"service token, every approver becomes one identity and four-eyes collapses", got)
	}
	if path, _ := a["path"].(string); !strings.Contains(path, sodTrail) {
		t.Errorf("approval posted to %q, want the artifact's trail", path)
	}
}

func TestAnApprovalFidesRefusesIsNotRecordedLocally(t *testing.T) {
	// The order matters: the cluster is written only after Fides accepts. A
	// Bundle marked approved here while Fides never heard of it reads as done
	// and holds forever, with nothing left to retry.
	f := &sodFides{trail: sodTrail, approvalStatus: http.StatusInternalServerError}
	server := f.start(t)
	o, c := evidenceOps(t,
		testGate("staging", withEvidence(server)), testBundle("b1", 0, withDigest), sodSecret())
	ctx := context.Background()

	if err := o.Approve(ctx, "acme", "b1", "staging", "olaf@acme.example"); err == nil {
		t.Fatal("approving succeeded while Fides refused to record it")
	}

	var b v1alpha1.Bundle
	if err := c.Get(ctx, client.ObjectKey{Namespace: "acme", Name: "b1"}, &b); err != nil {
		t.Fatal(err)
	}
	if b.IsApprovedFor("staging") {
		t.Error("the Bundle reads as approved, so retrying does nothing and the crossing " +
			"is held by evidence that will never arrive")
	}
}

func TestApprovingAnArtifactFidesHasNeverSeenIsRefused(t *testing.T) {
	f := &sodFides{} // no trail
	server := f.start(t)
	o, c := evidenceOps(t,
		testGate("staging", withEvidence(server)), testBundle("b1", 0, withDigest), sodSecret())
	ctx := context.Background()

	err := o.Approve(ctx, "acme", "b1", "staging", "olaf@acme.example")
	if !IsRefused(err) {
		t.Fatalf("err = %v, want a refusal naming the missing trail", err)
	}

	var b v1alpha1.Bundle
	if err := c.Get(ctx, client.ObjectKey{Namespace: "acme", Name: "b1"}, &b); err != nil {
		t.Fatal(err)
	}
	if b.IsApprovedFor("staging") {
		t.Error("the approval was recorded locally with nothing in Fides to attach it to")
	}
}

func TestAGateWithoutEvidenceDoesNotTouchFides(t *testing.T) {
	f := &sodFides{trail: sodTrail}
	server := f.start(t)
	// The Gate names no Fides environment, so the server exists only to catch
	// a call that should never be made.
	o, c := evidenceOps(t, testGate("staging"), testBundle("b1", 0, withDigest), sodSecret())
	_ = server

	if err := o.Approve(context.Background(), "acme", "b1", "staging", "olaf@acme.example"); err != nil {
		t.Fatal(err)
	}
	if len(f.approvals) != 0 {
		t.Errorf("a Gate that does not use Fides recorded %d approvals there", len(f.approvals))
	}

	var b v1alpha1.Bundle
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "acme", Name: "b1"}, &b); err != nil {
		t.Fatal(err)
	}
	if !b.IsApprovedFor("staging") {
		t.Error("approving a Gate that does not use Fides must still work")
	}
}
