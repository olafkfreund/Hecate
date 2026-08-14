package git

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/githubapp"
)

func appSecret(t *testing.T, baseURL string) *corev1.Secret {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet", Namespace: "acme"},
		Data: map[string][]byte{
			githubapp.KeyClientID:       []byte("Iv1.abc123"),
			githubapp.KeyInstallationID: []byte("42"),
			githubapp.KeyPrivateKey:     pemKey,
			"baseURL":                   []byte(baseURL),
		},
	}
}

func fakeGitHub(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_installation_token",
			"expires_at": "2099-01-01T00:00:00Z",
		})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func clientWith(t *testing.T, objs ...*corev1.Secret) *fake.ClientBuilder {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	b := fake.NewClientBuilder().WithScheme(s)
	for _, o := range objs {
		b = b.WithObjects(o)
	}
	return b
}

// The point of #118: one Secret, one credential, both paths. A push and a pull
// request resolving different credentials would leave the permanent one in
// place and change nothing.
func TestAGitHubAppSecretAuthenticatesAGitPush(t *testing.T) {
	secret := appSecret(t, fakeGitHub(t))
	c := clientWith(t, secret).Build()

	auth, err := Auth(context.Background(), c, "acme", &v1alpha1.LocalSecretRef{Name: "fleet"})
	if err != nil {
		t.Fatal(err)
	}
	basic, ok := auth.(*githttp.BasicAuth)
	if !ok {
		t.Fatalf("auth = %T, want basic auth carrying the installation token", auth)
	}
	if basic.Password != "ghs_installation_token" {
		t.Errorf("password = %q, want the minted installation token", basic.Password)
	}
	// GitHub ignores the username but rejects an empty one.
	if basic.Username == "" {
		t.Error("no username: GitHub rejects basic auth without one")
	}
}

// A Secret with a plain password must keep working: the App path is an
// addition, not a replacement, and breaking it would break every existing
// installation.
func TestAPasswordSecretStillWorks(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet", Namespace: "acme"},
		Data:       map[string][]byte{"username": []byte("olaf"), "password": []byte("hunter2")},
	}
	c := clientWith(t, secret).Build()

	auth, err := Auth(context.Background(), c, "acme", &v1alpha1.LocalSecretRef{Name: "fleet"})
	if err != nil {
		t.Fatal(err)
	}
	basic, ok := auth.(*githttp.BasicAuth)
	if !ok {
		t.Fatalf("auth = %T", auth)
	}
	if basic.Password != "hunter2" || basic.Username != "olaf" {
		t.Errorf("auth = %q/%q, want the Secret's own credentials", basic.Username, basic.Password)
	}
}

// An App Secret missing a field must say which, not fail later as a git
// authentication error that points at the wrong thing.
func TestAnIncompleteAppSecretNamesTheMissingField(t *testing.T) {
	secret := appSecret(t, "")
	delete(secret.Data, githubapp.KeyInstallationID)
	c := clientWith(t, secret).Build()

	_, err := Auth(context.Background(), c, "acme", &v1alpha1.LocalSecretRef{Name: "fleet"})
	if err == nil {
		t.Fatal("an App Secret with no installation id was accepted")
	}
	if got := err.Error(); !strings.Contains(got, "installationID") {
		t.Errorf("error %q does not name the missing field", got)
	}
}
