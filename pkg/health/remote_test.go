package health

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

func localClient(t *testing.T, objs ...*corev1.Secret) *Clusters {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	b := fake.NewClientBuilder().WithScheme(s)
	for _, o := range objs {
		b = b.WithObjects(o)
	}
	return &Clusters{Local: b.Build()}
}

func kubeconfigSecret(name, server string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "acme"},
		Data: map[string][]byte{kubeconfigKey: []byte(`apiVersion: v1
kind: Config
clusters:
  - name: remote
    cluster: {server: ` + server + `}
contexts:
  - name: remote
    context: {cluster: remote, user: remote}
current-context: remote
users:
  - name: remote
    user: {token: abc123}
`)},
	}
}

// No clusterRef is the common case and must not go looking for a Secret.
func TestNoClusterRefUsesTheLocalClient(t *testing.T) {
	c := localClient(t)
	got, err := c.For(context.Background(), c.Local, "acme", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != c.Local {
		t.Error("a Gate with no clusterRef was given something other than the local client")
	}
}

func TestAClusterRefResolvesAClient(t *testing.T) {
	c := localClient(t, kubeconfigSecret("remote", "https://remote.example:6443"))
	got, err := c.For(context.Background(), c.Local, "acme", &v1alpha1.LocalSecretRef{Name: "remote"})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got == c.Local {
		t.Error("a clusterRef did not produce a distinct client")
	}
}

// Rotating a kubeconfig must produce a new client rather than keep using
// credentials that have been replaced.
func TestTheClientCacheIsKeyedByContentNotName(t *testing.T) {
	c := localClient(t, kubeconfigSecret("remote", "https://one.example:6443"))
	ctx := context.Background()
	ref := &v1alpha1.LocalSecretRef{Name: "remote"}

	first, err := c.For(ctx, c.Local, "acme", ref)
	if err != nil {
		t.Fatal(err)
	}
	again, err := c.For(ctx, c.Local, "acme", ref)
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Error("the same kubeconfig produced two clients — each is a connection worth reusing")
	}

	// The Secret is replaced under the same name, as a rotation does.
	rotated := kubeconfigSecret("remote", "https://two.example:6443")
	if err := c.Local.Update(ctx, rotated); err != nil {
		t.Fatal(err)
	}
	third, err := c.For(ctx, c.Local, "acme", ref)
	if err != nil {
		t.Fatal(err)
	}
	if third == first {
		t.Error("a rotated kubeconfig reused the old client, so replaced credentials stay in use")
	}
}

func TestAMissingOrEmptySecretSaysWhich(t *testing.T) {
	empty := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "blank", Namespace: "acme"},
		Data:       map[string][]byte{},
	}
	c := localClient(t, empty)
	ctx := context.Background()

	_, err := c.For(ctx, c.Local, "acme", &v1alpha1.LocalSecretRef{Name: "nope"})
	if err == nil {
		t.Error("a clusterRef naming no Secret was accepted")
	}

	_, err = c.For(ctx, c.Local, "acme", &v1alpha1.LocalSecretRef{Name: "blank"})
	if err == nil || !strings.Contains(err.Error(), kubeconfigKey) {
		t.Errorf("error = %v, want it to name the missing key", err)
	}
}

// The trap this issue warns about: a remote cluster reference must not become
// a way around the namespace rule. A Gate in `team-a` pointing at another
// cluster is still `team-a` there — otherwise adding a kubeconfig is how a
// tenant escapes its boundary (#85, D11).
func TestAClusterRefDoesNotRelaxTheNamespaceRule(t *testing.T) {
	cfg := FluxConfig{
		ClusterRef: &v1alpha1.LocalSecretRef{Name: "remote"},
		Resources: []FluxResource{{
			Kind: "Kustomization", Name: "app", Namespace: "team-b",
		}},
	}

	err := cfg.Validate("team-a", false)
	if err == nil {
		t.Fatal("a Gate in team-a watched team-b on a remote cluster — a cluster " +
			"reference must not be a way past the tenant boundary")
	}
	if !strings.Contains(err.Error(), "cross-namespace") {
		t.Errorf("error = %v, want the namespace rule named", err)
	}

	// And it is the same rule, not a separate one: allowing cross-namespace
	// allows it remotely too, which is what an operator who set the flag means.
	if err := cfg.Validate("team-a", true); err != nil {
		t.Errorf("cross-namespace was allowed but a remote reference was still refused: %v", err)
	}
}
