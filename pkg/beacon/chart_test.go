package beacon

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// chartIndex serves a Helm repository index, in the shape Helm publishes it:
// entries keyed by chart name, each a list of versions. Taken from a real
// index.yaml rather than invented, because a hand-written fixture agrees with
// whatever the parser expects.
func chartIndex(t *testing.T, seen *http.Request, entries map[string][]string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("apiVersion: v1\nentries:\n")
	for chart, versions := range entries {
		fmt.Fprintf(&b, "  %s:\n", chart)
		for _, v := range versions {
			fmt.Fprintf(&b, "    - apiVersion: v2\n      name: %s\n      version: %s\n"+
				"      urls:\n        - https://example.invalid/%s-%s.tgz\n", chart, v, chart, v)
		}
	}
	body := b.String()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			*seen = *r.Clone(r.Context())
		}
		if r.URL.Path != "/index.yaml" {
			// Helm looks for index.yaml at the repository root and nowhere
			// else, so a request anywhere else is a bug worth failing on.
			t.Errorf("unexpected request to %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func chartResolver(objs ...runtime.Object) *Resolver {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	return &Resolver{Client: fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(objs...).Build()}
}

func resolveChartWatch(t *testing.T, r *Resolver, w v1alpha1.ChartWatch) (*v1alpha1.ChartArtifact, error) {
	t.Helper()
	got, err := r.Resolve(context.Background(), "acme", v1alpha1.WatchSource{Chart: &w})
	if err != nil {
		return nil, err
	}
	if got.Chart == nil {
		t.Fatalf("resolved to %+v, which is not a chart", got)
	}
	return got.Chart, nil
}

func TestChartResolvesTheNewestVersionInTheIndex(t *testing.T) {
	repo := chartIndex(t, nil, map[string][]string{
		"podinfo": {"6.9.0", "6.14.1", "6.10.0"},
		// A second chart in the same index, because that is the whole reason
		// an HTTPS repository needs a name: picking the newest of everything
		// would resolve podinfo to 9.9.9.
		"nginx": {"9.9.9"},
	})

	got, err := resolveChartWatch(t, chartResolver(), v1alpha1.ChartWatch{Repo: repo, Name: "podinfo"})
	if err != nil {
		t.Fatal(err)
	}
	// 6.14.1 over 6.9.0: a chart version is semantic by specification, so
	// lexical ordering here would pick 6.9.0 and quietly downgrade.
	if got.Version != "6.14.1" {
		t.Errorf("version = %q, want 6.14.1", got.Version)
	}
	if got.Name != "podinfo" || got.Repo != repo {
		t.Errorf("artifact = %+v", got)
	}
}

func TestChartHonoursTheConstraint(t *testing.T) {
	repo := chartIndex(t, nil, map[string][]string{"podinfo": {"6.14.1", "7.0.0"}})

	got, err := resolveChartWatch(t, chartResolver(),
		v1alpha1.ChartWatch{Repo: repo, Name: "podinfo", Constraint: "^6.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "6.14.1" {
		t.Errorf("version = %q — the constraint exists to stop a major arriving unasked", got.Version)
	}
}

func TestAChartMissingFromTheIndexSaysSo(t *testing.T) {
	repo := chartIndex(t, nil, map[string][]string{"nginx": {"1.0.0"}})

	_, err := resolveChartWatch(t, chartResolver(), v1alpha1.ChartWatch{Repo: repo, Name: "podinfo"})
	if err == nil {
		t.Fatal("resolving a chart that is not in the repository succeeded")
	}
	// Distinct from "nothing matched the constraint": this is a typo or the
	// wrong repository, and telling the two apart is the difference between
	// waiting and fixing.
	if !strings.Contains(err.Error(), "not in the index") {
		t.Errorf("error = %v, want it to say the chart is absent", err)
	}
}

func TestAnHTTPSChartRepositoryNeedsAName(t *testing.T) {
	repo := chartIndex(t, nil, map[string][]string{"podinfo": {"6.14.1"}})

	_, err := resolveChartWatch(t, chartResolver(), v1alpha1.ChartWatch{Repo: repo})
	if err == nil {
		t.Fatal("a nameless watch against an index of many charts resolved to something")
	}
	if !strings.Contains(err.Error(), "name is required") {
		t.Errorf("error = %v", err)
	}
}

// A credentialsRef that silently did nothing would turn a private repository
// into an unexplained 401 on a Beacon that looks correctly configured.
func TestChartCredentialsAreSent(t *testing.T) {
	var seen http.Request
	repo := chartIndex(t, &seen, map[string][]string{"podinfo": {"6.14.1"}})
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "helm", Namespace: "acme"},
		Data:       map[string][]byte{"username": []byte("olaf"), "password": []byte("hunter2")},
	}

	_, err := resolveChartWatch(t, chartResolver(secret), v1alpha1.ChartWatch{
		Repo: repo, Name: "podinfo", CredentialsRef: &v1alpha1.LocalSecretRef{Name: "helm"},
	})
	if err != nil {
		t.Fatal(err)
	}
	user, password, ok := seen.BasicAuth()
	if !ok {
		t.Fatal("the index was fetched with no credentials, so a private repository would 401")
	}
	if user != "olaf" || password != "hunter2" {
		t.Errorf("sent %q/%q", user, password)
	}
}

// A docker config is the wrong shape here and would otherwise be sent as an
// empty username and password, which reads as "the server rejected us".
func TestAChartSecretWithoutBasicCredentialsIsRefused(t *testing.T) {
	repo := chartIndex(t, nil, map[string][]string{"podinfo": {"6.14.1"}})
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "helm", Namespace: "acme"},
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`)},
	}

	_, err := resolveChartWatch(t, chartResolver(secret), v1alpha1.ChartWatch{
		Repo: repo, Name: "podinfo", CredentialsRef: &v1alpha1.LocalSecretRef{Name: "helm"},
	})
	if err == nil || !strings.Contains(err.Error(), "no username and password") {
		t.Errorf("error = %v, want it to name what the Secret is missing", err)
	}
}

func TestOCIChartResolvesTheNewestTag(t *testing.T) {
	// The same in-memory registry the image resolver is tested against: an OCI
	// chart is an artifact in a registry, and versions are tags.
	repo := newTestRepo(t, "charts/podinfo", "6.9.0", "6.14.1", "6.10.0")

	got, err := resolveChartWatch(t, chartResolver(), v1alpha1.ChartWatch{Repo: "oci://" + repo.Repo})
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "6.14.1" {
		t.Errorf("version = %q, want 6.14.1", got.Version)
	}
	if got.Name != "" {
		t.Errorf("name = %q — an OCI repository already identifies the chart", got.Name)
	}
}

func TestAnOCIChartRefusesAName(t *testing.T) {
	repo := newTestRepo(t, "charts/podinfo", "6.14.1")

	_, err := resolveChartWatch(t, chartResolver(),
		v1alpha1.ChartWatch{Repo: "oci://" + repo.Repo, Name: "podinfo"})
	// Not merely redundant: the author expected the name to select something,
	// and honouring the repository instead would resolve a different chart
	// than the one they wrote down.
	if err == nil || !strings.Contains(err.Error(), "must be empty") {
		t.Errorf("error = %v, want the contradiction named", err)
	}
}
