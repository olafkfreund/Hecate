package beacon

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/registry"
)

// indexLimit bounds how much of an index.yaml we will read.
//
// A Helm index grows with every version of every chart in the repository:
// prometheus-community's is 5.9MB today, measured. This is a controller with
// other work to do, so a repository having a bad day cannot make it allocate
// without limit. Ten times the largest real one seen is enough headroom to
// never fire by accident and still bound the damage.
//
// ponytail: a flat cap rather than streaming the YAML, because the parser wants
// the whole document anyway. If someone hits it legitimately, the fix is a
// narrower repository, not a bigger number.
const indexLimit = 64 << 20

// resolveChart finds the newest chart version a ChartWatch points at.
//
// Two transports behind one field, because Helm has two: an OCI registry, where
// the repository *is* the chart and versions are tags, and an HTTPS repository,
// where one index.yaml lists every version of every chart. Telling them apart
// by the oci:// scheme is what Helm itself does.
func (r *Resolver) resolveChart(
	ctx context.Context, namespace string, w v1alpha1.ChartWatch,
) (v1alpha1.Artifact, error) {
	// SemVer always. A chart version is defined by the Helm specification to be
	// semantic, so the other strategies would be answering a question nobody
	// asked — and Lexical would happily rank 1.10.0 below 1.9.0.
	pick := Selection{Strategy: v1alpha1.SelectSemVer, Constraint: w.Constraint}

	var (
		version string
		err     error
	)
	if isOCI(w.Repo) {
		version, err = r.ociChartVersion(ctx, namespace, w, pick)
	} else {
		version, err = r.httpChartVersion(ctx, namespace, w, pick)
	}
	if err != nil {
		return v1alpha1.Artifact{}, err
	}

	return v1alpha1.Artifact{Chart: &v1alpha1.ChartArtifact{
		Repo:    w.Repo,
		Name:    w.Name,
		Version: version,
	}}, nil
}

func isOCI(repo string) bool { return strings.HasPrefix(repo, "oci://") }

// ociChartVersion lists the chart's tags. An OCI chart is an artifact in a
// registry like any other, so this is the image path with a different name.
func (r *Resolver) ociChartVersion(
	ctx context.Context, namespace string, w v1alpha1.ChartWatch, pick Selection,
) (string, error) {
	if w.Name != "" {
		// Not merely redundant: the repository already identifies the chart, so
		// a name here means the author expected it to select something and it
		// would be silently ignored.
		return "", fmt.Errorf("chart %s: name must be empty for an OCI repository — "+
			"the repository already identifies the chart", w.Repo)
	}

	repo, err := name.NewRepository(strings.TrimPrefix(w.Repo, "oci://"))
	if err != nil {
		return "", fmt.Errorf("invalid chart repository %q: %w", w.Repo, err)
	}
	keychain, err := r.keychain(ctx, namespace, w.CredentialsRef)
	if err != nil {
		return "", err
	}
	tags, err := remote.List(repo, registry.RemoteOptions(ctx, keychain)...)
	if err != nil {
		return "", fmt.Errorf("listing chart versions for %s: %w", w.Repo, err)
	}
	return pick.Pick(tags)
}

// httpChartVersion reads the repository index and takes the versions listed for
// this chart.
func (r *Resolver) httpChartVersion(
	ctx context.Context, namespace string, w v1alpha1.ChartWatch, pick Selection,
) (string, error) {
	if w.Name == "" {
		// One index lists every chart in the repository, so without a name
		// there is nothing to select between.
		return "", fmt.Errorf("chart repository %s: name is required — an HTTPS repository "+
			"index lists every chart in it", w.Repo)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimSuffix(w.Repo, "/")+"/index.yaml", nil)
	if err != nil {
		return "", fmt.Errorf("chart repository %s: %w", w.Repo, err)
	}
	if err := r.chartAuth(ctx, namespace, w.CredentialsRef, req); err != nil {
		return "", err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("reading the index of %s: %w", w.Repo, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("reading the index of %s: %s", w.Repo, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, indexLimit))
	if err != nil {
		return "", fmt.Errorf("reading the index of %s: %w", w.Repo, err)
	}

	var index struct {
		Entries map[string][]struct {
			Version string `json:"version"`
		} `json:"entries"`
	}
	if err := yaml.Unmarshal(body, &index); err != nil {
		return "", fmt.Errorf("parsing the index of %s: %w", w.Repo, err)
	}

	entries, ok := index.Entries[w.Name]
	if !ok {
		// Distinguished from "no version matched": the chart is not there at
		// all, which is a typo or the wrong repository, not a constraint that
		// has yet to be satisfied.
		return "", fmt.Errorf("chart %q is not in the index of %s", w.Name, w.Repo)
	}
	versions := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Version != "" {
			versions = append(versions, e.Version)
		}
	}
	return pick.Pick(versions)
}

// chartAuth puts basic credentials on the index request.
//
// Not the registry keychain: that speaks the Docker credential formats, and an
// HTTPS Helm repository is an ordinary web server behind ordinary basic auth.
// Honoured rather than ignored, because a credentialsRef that silently did
// nothing would turn a private repository into a confusing 401.
func (r *Resolver) chartAuth(
	ctx context.Context, namespace string, ref *v1alpha1.LocalSecretRef, req *http.Request,
) error {
	if ref == nil {
		return nil
	}
	if r.Client == nil {
		return fmt.Errorf("credentialsRef %q set but there is no client to read it with", ref.Name)
	}
	var secret corev1.Secret
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, &secret); err != nil {
		return fmt.Errorf("reading credentials Secret %s/%s: %w", namespace, ref.Name, err)
	}
	user, password := string(secret.Data["username"]), string(secret.Data["password"])
	if user == "" && password == "" {
		return fmt.Errorf("the Secret %s has no username and password — an HTTPS chart "+
			"repository authenticates with basic credentials, not a docker config", ref.Name)
	}
	req.SetBasicAuth(user, password)
	return nil
}
