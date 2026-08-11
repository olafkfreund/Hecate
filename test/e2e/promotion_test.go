//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// The git server scripts/dev-cluster.sh installs and seeds.
const (
	gitURL      = "http://gitea.hecate-git.svc.cluster.local:3000/hecate/fleet.git"
	gitUser     = "hecate"
	gitPassword = "hecate-dev"
)

var gitRepoGVK = schema.GroupVersionKind{
	Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "GitRepository",
}

// TestDeployedControllerPromotesThroughGit is the product's central claim,
// proven rather than asserted: Hecate writes to git, Flux reads git, and the
// cluster ends up running what the Bundle pinned.
//
// Everything before this test exercised the git steps against local
// repositories in a unit test, and the deployed controller only ever ran
// flux-wait. That left the most important sentence in the README —
// "a Passage writes to git; Flux syncs; Hecate reads the result back" —
// demonstrated nowhere. This runs it: six steps, a real git host with real
// credentials, real Flux, and an assertion on the applied state rather than on
// Hecate's own account of itself.
func TestDeployedControllerPromotesThroughGit(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)
	requireHecateInstalled(t, c)
	requireGitServer(t, c)
	freshNamespace(t, c)

	mustCreate(t, c, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "git", Namespace: namespace},
		StringData: map[string]string{"username": gitUser, "password": gitPassword},
	})

	// Flux watches the same repository Hecate is about to write to. This is the
	// rendezvous: neither side talks to the other.
	applyGitSource(t, c)

	// The seeded value. If this is already what Flux applied, the promotion
	// below has to change it for the test to mean anything.
	if got := payloadTag(t, c); got != "0.0.0" {
		t.Logf("the repository is not freshly seeded (tag is %q) — a previous run promoted it", got)
	}

	mustCreate(t, c, &v1alpha1.Beacon{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: namespace},
		Spec: v1alpha1.BeaconSpec{
			Interval: metav1.Duration{Duration: 30 * time.Second},
			Watch: []v1alpha1.WatchSource{{Image: &v1alpha1.ImageWatch{
				Repo: "ghcr.io/stefanprodan/podinfo", Constraint: "^6.0.0",
			}}},
		},
	})

	// The README's example, as a running system.
	mustCreate(t, c, &v1alpha1.Gate{
		ObjectMeta: metav1.ObjectMeta{Name: "staging", Namespace: namespace},
		Spec: v1alpha1.GateSpec{
			Auto:   true,
			Admits: []v1alpha1.Admission{{From: v1alpha1.BundleOrigin{Beacon: "app"}}},
			Passage: &v1alpha1.PassageTemplate{Steps: []v1alpha1.Step{
				{
					Uses: "git-clone",
					With: jsonOf(t, map[string]any{
						"repo":           gitURL,
						"credentialsRef": map[string]any{"name": "git"},
					}),
				},
				{
					// Repinned by repository name, from the Bundle's own digest.
					Uses: "set-image",
					With: jsonOf(t, map[string]any{
						"path":  "repo/apps/staging/kustomization.yaml",
						"image": "ghcr.io/stefanprodan/podinfo",
					}),
				},
				{
					// So the *applied* state changes visibly, and the assertion
					// can be about the cluster rather than about a commit.
					Uses: "edit-yaml",
					With: jsonOf(t, map[string]any{
						"path": "repo/apps/staging/configmap.yaml",
						"edits": []any{map[string]any{
							"key":   "data.tag",
							"value": "${{ bundle.image('ghcr.io/stefanprodan/podinfo').tag }}",
						}},
					}),
				},
				{
					Uses: "git-commit",
					As:   "commit",
					With: jsonOf(t, map[string]any{
						"message": "promote ${{ bundle.image('ghcr.io/stefanprodan/podinfo').tag }} to staging",
					}),
				},
				{
					Uses: "git-push",
					With: jsonOf(t, map[string]any{"credentialsRef": map[string]any{"name": "git"}}),
				},
				{
					// Without this the promotion waits out the source interval.
					Uses: "flux-reconcile",
					With: jsonOf(t, map[string]any{
						"resources": []any{
							map[string]any{"kind": "GitRepository", "name": "fleet"},
							map[string]any{"kind": "Kustomization", "name": "fleet"},
						},
					}),
				},
				{
					// The join: wait for Flux to have applied *this* commit, not
					// merely to be Ready from the previous one.
					Uses: "flux-wait",
					With: jsonOf(t, map[string]any{
						"resources":        []any{map[string]any{"kind": "Kustomization", "name": "fleet"}},
						"expectedRevision": "${{ steps.commit.sha }}",
					}),
				},
			}},
		},
	})

	var bundle v1alpha1.Bundle
	waitFor(t, 3*time.Minute, "the deployed Beacon to emit a Bundle", func() bool {
		var list v1alpha1.BundleList
		if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil || len(list.Items) == 0 {
			return false
		}
		bundle = list.Items[0]
		return true
	}, func() string { return beaconStatus(t, c) })

	promoted := bundle.Spec.Artifacts[0].Image.Tag
	if promoted == "" {
		t.Fatalf("the Bundle pinned no tag: %+v", bundle.Spec.Artifacts[0].Image)
	}
	t.Logf("promoting podinfo %s (%s)", promoted, bundle.Name)

	var passage v1alpha1.Passage
	waitFor(t, 5*time.Minute, "the Passage to cross", func() bool {
		var list v1alpha1.PassageList
		if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil || len(list.Items) == 0 {
			return false
		}
		passage = list.Items[0]
		if passage.Status.Phase == v1alpha1.PassageFailed {
			t.Fatalf("the Passage failed: %s\n%s", passage.Status.Message, stepReport(passage))
		}
		return passage.Status.Phase == v1alpha1.PassageSucceeded
	}, func() string { return passageReport(t, c) })

	// Every step ran in the deployed controller: a read-only root filesystem, a
	// scratch volume, and RBAC that had to allow reading the credentials Secret.
	t.Logf("crossed:\n%s", stepReport(passage))

	sha := stepOutput(t, passage, "git-commit", "sha")
	if sha == "" {
		t.Fatal("git-commit published no sha")
	}

	// The proof. Flux — which has never heard of Hecate — applied the commit
	// Hecate pushed, and the cluster now holds what the Bundle pinned.
	if got := payloadTag(t, c); got != promoted {
		t.Errorf("the applied ConfigMap has tag %q, want %q — "+
			"Flux did not apply the commit Hecate pushed", got, promoted)
	}

	var ks unstructured.Unstructured
	ks.SetGroupVersionKind(kustomizationGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: "fleet", Namespace: namespace}, &ks); err != nil {
		t.Fatal(err)
	}
	applied, _, _ := unstructured.NestedString(ks.Object, "status", "lastAppliedRevision")
	if !strings.Contains(applied, sha) {
		t.Errorf("Flux applied revision %q, want one containing the pushed commit %s", applied, sha)
	}
}

// requireGitServer skips when the cluster has no git server, rather than
// failing: a cluster created before it existed is out of date, not broken.
func requireGitServer(t *testing.T, c client.Client) {
	t.Helper()
	var svc corev1.Service
	err := c.Get(context.Background(),
		types.NamespacedName{Name: "gitea", Namespace: "hecate-git"}, &svc)
	if err != nil {
		t.Skip("no git server in the cluster — run 'make cluster' to install it")
	}
}

// applyGitSource points Flux at the repository the promotion writes to.
func applyGitSource(t *testing.T, c client.Client) {
	t.Helper()
	ctx := context.Background()

	repo := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"interval":  "30s",
			"url":       gitURL,
			"ref":       map[string]any{"branch": "main"},
			"secretRef": map[string]any{"name": "git"},
		},
	}}
	repo.SetGroupVersionKind(gitRepoGVK)
	repo.SetName("fleet")
	repo.SetNamespace(namespace)
	if err := c.Create(ctx, repo); err != nil {
		t.Fatalf("creating GitRepository: %v", err)
	}

	ks := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"interval":        "30s",
			"prune":           true,
			"targetNamespace": namespace,
			"path":            "./apps/staging",
			"sourceRef":       map[string]any{"kind": "GitRepository", "name": "fleet"},
		},
	}}
	ks.SetGroupVersionKind(kustomizationGVK)
	ks.SetName("fleet")
	ks.SetNamespace(namespace)
	if err := c.Create(ctx, ks); err != nil {
		t.Fatalf("creating Kustomization: %v", err)
	}

	waitFor(t, 3*time.Minute, "Flux to apply the seeded manifests", func() bool {
		var cm corev1.ConfigMap
		return c.Get(ctx, types.NamespacedName{Name: "e2e-git-payload", Namespace: namespace}, &cm) == nil
	}, func() string { return fluxStatus(t, c) })
}

// payloadTag reads the tag from the ConfigMap Flux applied.
func payloadTag(t *testing.T, c client.Client) string {
	t.Helper()
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(),
		types.NamespacedName{Name: "e2e-git-payload", Namespace: namespace}, &cm); err != nil {
		return ""
	}
	return cm.Data["tag"]
}

// stepOutput reads one value a step published.
func stepOutput(t *testing.T, p v1alpha1.Passage, uses, key string) string {
	t.Helper()
	for _, st := range p.Status.Steps {
		if st.Uses != uses || st.Output == nil {
			continue
		}
		var out map[string]any
		if err := json.Unmarshal(st.Output.Raw, &out); err != nil {
			return ""
		}
		if v, ok := out[key].(string); ok {
			return v
		}
	}
	return ""
}

// stepReport renders a Passage's steps for a failure message, because "the
// Passage failed" without saying which step is a message that wastes a run.
func stepReport(p v1alpha1.Passage) string {
	var b strings.Builder
	for _, st := range p.Status.Steps {
		fmt.Fprintf(&b, "  %-16s %-10s %s", st.Uses, st.Phase, st.Message)
		if st.Reason != "" {
			fmt.Fprintf(&b, " [%s]", st.Reason)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func passageReport(t *testing.T, c client.Client) string {
	t.Helper()
	var list v1alpha1.PassageList
	if err := c.List(context.Background(), &list, client.InNamespace(namespace)); err != nil {
		return "listing Passages: " + err.Error()
	}
	if len(list.Items) == 0 {
		return "no Passages exist yet"
	}
	var b strings.Builder
	for _, p := range list.Items {
		fmt.Fprintf(&b, "%s: %s %s\n%s", p.Name, p.Status.Phase, p.Status.Message, stepReport(p))
	}
	return b.String()
}

// fluxStatus reports what Flux makes of the git source, for the same reason.
func fluxStatus(t *testing.T, c client.Client) string {
	t.Helper()
	ctx := context.Background()
	var b strings.Builder
	for _, r := range []struct {
		gvk  schema.GroupVersionKind
		name string
	}{{gitRepoGVK, "fleet"}, {kustomizationGVK, "fleet"}} {
		var got unstructured.Unstructured
		got.SetGroupVersionKind(r.gvk)
		if err := c.Get(ctx, types.NamespacedName{Name: r.name, Namespace: namespace}, &got); err != nil {
			fmt.Fprintf(&b, "%s %s: %s\n", r.gvk.Kind, r.name, err)
			continue
		}
		conditions, _, _ := unstructured.NestedSlice(got.Object, "status", "conditions")
		fmt.Fprintf(&b, "%s %s:", r.gvk.Kind, r.name)
		for _, cond := range conditions {
			m, ok := cond.(map[string]any)
			if !ok {
				continue
			}
			fmt.Fprintf(&b, " %v=%v (%v)", m["type"], m["status"], m["message"])
		}
		b.WriteString("\n")
	}
	return b.String()
}
