//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// The registry address depends on who is asking, which is not obvious and cost
// an hour the first time:
//
//   - containerd pulls images through k3d's mirror on :5001
//   - Flux's source-controller makes its own HTTP request and must use the
//     container's real port, :5000
const (
	registryFromHost    = "localhost:5001"
	registryFromCluster = "hecate-registry:5000"
)

var (
	ociRepoGVK = schema.GroupVersionKind{
		Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "OCIRepository",
	}
	kustomizationGVK = schema.GroupVersionKind{
		Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: "Kustomization",
	}
)

// publishImages pushes tagged images into the cluster's registry, so the
// deployed Beacon has something to discover without reaching the internet.
//
// Two addresses for one registry, which is not obvious: the test pushes from the
// host on :5001, and the Beacon in the cluster reads it as
// hecate-registry:5000. See the constants above.
func publishImages(t *testing.T, repo string, tags ...string) map[string]string {
	t.Helper()
	digests := map[string]string{}
	for i, tag := range tags {
		img, err := random.Image(int64(64+i), 1)
		if err != nil {
			t.Fatal(err)
		}
		// Insecure because the k3d registry is plain HTTP — the same reason the
		// Beacon needs spec.watch[].image.insecure to read it.
		ref, err := name.NewTag(registryFromHost+"/"+repo+":"+tag, name.Insecure)
		if err != nil {
			t.Fatal(err)
		}
		if err := remote.Write(ref, img); err != nil {
			t.Fatalf("pushing %s: %v", ref, err)
		}
		d, err := img.Digest()
		if err != nil {
			t.Fatal(err)
		}
		digests[tag] = d.String()
	}
	return digests
}

// requireHecateInstalled skips unless the controller is actually deployed. The
// out-of-cluster test in crossing_test.go covers the API contract without it;
// this one is about the deployed system.
func requireHecateInstalled(t *testing.T, c client.Client) {
	t.Helper()
	var deployments unstructured.UnstructuredList
	deployments.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "apps", Version: "v1", Kind: "DeploymentList",
	})
	if err := c.List(context.Background(), &deployments,
		client.InNamespace("hecate-system"),
		client.MatchingLabels{"app.kubernetes.io/name": "hecate"},
	); err != nil || len(deployments.Items) == 0 {
		t.Skip("Hecate is not installed in-cluster — run 'make install'")
	}
	if _, err := exec.LookPath("flux"); err != nil {
		t.Skip("the flux CLI is not on PATH")
	}
}

// publishArtifact pushes a Flux OCI artifact to the cluster-local registry, so
// the test needs no internet and no external registry account.
func publishArtifact(t *testing.T, tag string) {
	t.Helper()
	dir := t.TempDir()
	manifests := filepath.Join(dir, "manifests")
	if err := os.MkdirAll(manifests, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: hecate-e2e-payload\ndata:\n  tag: \"" + tag + "\"\n"
	if err := os.WriteFile(filepath.Join(manifests, "cm.yaml"), []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command("flux", "push", "artifact",
		"oci://"+registryFromHost+"/e2e-manifests:"+tag,
		"--path", manifests, "--source", "e2e", "--revision", tag,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("pushing OCI artifact: %v\n%s", err, out)
	}
}

func applyFluxSource(t *testing.T, c client.Client, tag string) {
	t.Helper()
	ctx := context.Background()

	repo := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"interval": "30s",
			"url":      "oci://" + registryFromCluster + "/e2e-manifests",
			"ref":      map[string]any{"tag": tag},
			"insecure": true, // plain HTTP: a local registry with no certificate
		},
	}}
	repo.SetGroupVersionKind(ociRepoGVK)
	repo.SetName("e2e-manifests")
	repo.SetNamespace(namespace)
	if err := c.Create(ctx, repo); err != nil {
		t.Fatalf("creating OCIRepository: %v", err)
	}

	ks := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"interval":        "30s",
			"prune":           true,
			"targetNamespace": namespace,
			"path":            "./",
			"sourceRef":       map[string]any{"kind": "OCIRepository", "name": "e2e-manifests"},
		},
	}}
	ks.SetGroupVersionKind(kustomizationGVK)
	ks.SetName("e2e-payload")
	ks.SetNamespace(namespace)
	if err := c.Create(ctx, ks); err != nil {
		t.Fatalf("creating Kustomization: %v", err)
	}

	waitFor(t, 3*time.Minute, "Flux to reconcile the Kustomization", func() bool {
		var got unstructured.Unstructured
		got.SetGroupVersionKind(kustomizationGVK)
		if err := c.Get(ctx, types.NamespacedName{Name: "e2e-payload", Namespace: namespace}, &got); err != nil {
			return false
		}
		conds, found, _ := unstructured.NestedSlice(got.Object, "status", "conditions")
		if !found {
			return false
		}
		for _, raw := range conds {
			m, _ := raw.(map[string]any)
			if m["type"] == "Ready" && m["status"] == "True" {
				return true
			}
		}
		return false
	})
}

// TestDeployedControllerCrossesAGate is the whole product, in a real cluster:
// the deployed controller discovers an artifact, admits it, runs a Passage, and
// that Passage's flux-wait blocks on genuine Flux reconciliation rather than a
// fixture.
//
// It is the only test that exercises the controllers as a *deployment* — RBAC,
// the hardened pod, the read-only root filesystem and the scratch volume all
// have to be right for it to pass.
func TestDeployedControllerCrossesAGate(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)
	requireHecateInstalled(t, c)
	freshNamespace(t, c)

	publishArtifact(t, "v1")
	applyFluxSource(t, c, "v1")

	// The cluster's own registry, not the internet. The Beacon runs *in the
	// cluster* here so it cannot reach the test process's loopback, and the k3d
	// registry is plain HTTP — which is exactly what watch[].image.insecure is
	// for (#109). Before that field existed this test pulled from ghcr.io and
	// was the only one in the suite that needed the network.
	const app = "e2e/podinfo"
	publishImages(t, app, "6.0.0", "6.1.0")

	// No spec.watch on the Gate: health must come from what flux-wait emits.
	if err := c.Create(ctx, &v1alpha1.Beacon{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: namespace},
		Spec: v1alpha1.BeaconSpec{
			Interval: metav1.Duration{Duration: 30 * time.Second},
			Watch: []v1alpha1.WatchSource{{Image: &v1alpha1.ImageWatch{
				Repo: registryFromCluster + "/" + app, Constraint: "^6.0.0", Insecure: true,
			}}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, &v1alpha1.Gate{
		ObjectMeta: metav1.ObjectMeta{Name: "staging", Namespace: namespace},
		Spec: v1alpha1.GateSpec{
			Auto:   true,
			Admits: []v1alpha1.Admission{{From: v1alpha1.BundleOrigin{Beacon: "app"}}},
			Passage: &v1alpha1.PassageTemplate{Steps: []v1alpha1.Step{{
				Uses: "flux-wait",
				With: jsonOf(t, map[string]any{
					"resources": []any{map[string]any{"kind": "Kustomization", "name": "e2e-payload"}},
				}),
			}}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// The Beacon must discover the image without any help from the test.
	var bundle v1alpha1.Bundle
	waitFor(t, 3*time.Minute, "the deployed Beacon to emit a Bundle", func() bool {
		var list v1alpha1.BundleList
		if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil || len(list.Items) == 0 {
			return false
		}
		bundle = list.Items[0]
		return true
	})
	img := bundle.Spec.Artifacts[0].Image
	if img.Digest == "" || img.Tag == "" {
		t.Errorf("Bundle did not pin a resolved image: %+v", img)
	}

	// The Gate must cross it on its own, and flux-wait must succeed only
	// because a real Kustomization is Ready.
	waitFor(t, 3*time.Minute, "the Passage to succeed", func() bool {
		var list v1alpha1.PassageList
		if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
			return false
		}
		for _, p := range list.Items {
			if p.Status.Phase == v1alpha1.PassageSucceeded {
				return true
			}
			if p.Status.Phase == v1alpha1.PassageFailed {
				t.Fatalf("Passage %s failed: %s", p.Name, p.Status.Message)
			}
		}
		return false
	})

	waitFor(t, 2*time.Minute, "the Gate to record the crossing", func() bool {
		var gate v1alpha1.Gate
		if err := c.Get(ctx, types.NamespacedName{Name: "staging", Namespace: namespace}, &gate); err != nil {
			return false
		}
		return gate.Status.Current != nil && gate.Status.Current.Bundle == bundle.Name
	})

	// The durable record downstream Gates read.
	var cleared v1alpha1.Bundle
	if err := c.Get(ctx, types.NamespacedName{Name: bundle.Name, Namespace: namespace}, &cleared); err != nil {
		t.Fatal(err)
	}
	if !cleared.HasCleared("staging") {
		t.Error("the crossing was not recorded on the Bundle")
	}

	// D20: the Gate declares no spec.watch, so Healthy here can only come from
	// the health check flux-wait handed it. Without that the Gate goes blind
	// the moment the Passage ends.
	waitFor(t, 2*time.Minute, "the Gate to report health adopted from the Passage", func() bool {
		var gate v1alpha1.Gate
		if err := c.Get(ctx, types.NamespacedName{Name: "staging", Namespace: namespace}, &gate); err != nil {
			return false
		}
		return gate.Status.Health != nil && gate.Status.Health.Status == v1alpha1.HealthHealthy
	})
}
