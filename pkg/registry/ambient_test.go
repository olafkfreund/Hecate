package registry

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// A Secret the author explicitly named must never lose to ambient identity —
// D56 composes cloud keychains into the ambient side of the chain, and this is
// what keeps a referenced Secret first regardless.
func TestKeychainSecretBeatsAmbient(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "pull", Namespace: "ns"},
		Data:       map[string][]byte{"username": []byte("secret-user"), "password": []byte("secret-pass")},
	}
	c := fake.NewClientBuilder().WithObjects(secret).Build()

	kc, err := Keychain(context.Background(), c, "ns", &v1alpha1.LocalSecretRef{Name: "pull"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Any registry: the Secret in this test carries no registry key, so it
	// applies to whatever is asked for (KeychainFromSecret's anyRegistryKeychain)
	// — including a host the ambient ECR keychain would otherwise also answer
	// for, which is exactly the case D56 must not regress.
	reg, err := name.NewRegistry("111111111111.dkr.ecr.us-east-1.amazonaws.com")
	if err != nil {
		t.Fatal(err)
	}
	auth, err := kc.Resolve(reg)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := auth.Authorization()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Username != "secret-user" || cfg.Password != "secret-pass" {
		t.Errorf("got %+v, want the referenced Secret's credentials", cfg)
	}
}

// No cluster, no service account, no AWS/GCP identity: the fallback must still
// resolve to Anonymous rather than error or hang. This is the shape both the
// CLI (cmd/hecate) and the unit test suite run in every time.
func TestKeychainAmbientWithNoCloudIdentity(t *testing.T) {
	// A private registry ambientKeychain does not recognise as any cloud's.
	reg, err := name.NewRegistry("registry.example.internal:5000")
	if err != nil {
		t.Fatal(err)
	}

	kc, err := Keychain(context.Background(), nil, "", nil)
	if err != nil {
		t.Fatalf("unexpected error with no client and no ref: %v", err)
	}

	auth, err := kc.Resolve(reg)
	if err != nil {
		t.Fatalf("unexpected error resolving with no ambient identity: %v", err)
	}
	if auth != authn.Anonymous {
		cfg, _ := auth.Authorization()
		t.Errorf("got %+v, want Anonymous", cfg)
	}
}

// ecrKeychain and the Google keychain are host-gated: an ECR-shaped host never
// reaches the Google keychain's logic and vice versa, and neither reaches out
// for a host that is neither. This is what keeps resolving credentials for an
// ordinary registry free of any cloud SDK call, and so free of any risk of
// blocking on a metadata-server timeout that only matters on ECR/GCR hosts.
func TestIsAmbientCloudRegistry(t *testing.T) {
	for host, want := range map[string]bool{
		"111111111111.dkr.ecr.us-east-1.amazonaws.com": true,
		"public.ecr.aws":       true,
		"gcr.io":               true,
		"us-docker.pkg.dev":    true,
		"ghcr.io":              false,
		"index.docker.io":      false,
		"registry.internal:80": false,
	} {
		if got := IsAmbientCloudRegistry(host); got != want {
			t.Errorf("IsAmbientCloudRegistry(%q) = %v, want %v", host, got, want)
		}
	}
}

// KeychainFromSecret's dockerconfigjson path, exercised with a registry key
// that also happens to be ECR-shaped — a regression here would mean a
// dockerconfigjson Secret naming an ECR registry silently lost to the ambient
// AWS keychain instead of answering from the Secret.
func TestKeychainFromSecretBeatsAmbientForNamedRegistry(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"auths": map[string]any{
			"111111111111.dkr.ecr.us-east-1.amazonaws.com": map[string]string{
				"username": "secret-user", "password": "secret-pass",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "pull", Namespace: "ns"},
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: raw},
	}
	c := fake.NewClientBuilder().WithObjects(secret).Build()

	kc, err := Keychain(context.Background(), c, "ns", &v1alpha1.LocalSecretRef{Name: "pull"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	reg, err := name.NewRegistry("111111111111.dkr.ecr.us-east-1.amazonaws.com")
	if err != nil {
		t.Fatal(err)
	}
	auth, err := kc.Resolve(reg)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := auth.Authorization()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Username != "secret-user" || cfg.Password != "secret-pass" {
		t.Errorf("got %+v, want the referenced Secret's credentials", cfg)
	}
}
