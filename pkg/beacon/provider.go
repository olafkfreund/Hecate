package beacon

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// providerGVK is Flux Operator's ResourceSetInputProvider.
//
// Read as unstructured, per D4: this is a third-party API on a CRD Hecate does
// not own. Compiling against it would tie every Hecate build to one release of
// an operator that is optional in the first place.
var providerGVK = schema.GroupVersionKind{
	Group: "fluxcd.controlplane.io", Version: "v1", Kind: "ResourceSetInputProvider",
}

// resolveProvider reads the newest input a ResourceSetInputProvider has
// exported and turns it into an Artifact.
//
// **The first exported input is the answer.** The provider filters and sorts
// before exporting — by the semver range when one is configured, reverse
// alphabetically otherwise — so index zero is the newest. Running the list
// through Hecate's own Selection would be a second opinion about the same
// question, computed from different rules, on data that has already been
// narrowed by a limit the operator applied. Verified against a real provider:
// a podinfo chart watch with `semver: ">=6.0.0"` exports 6.14.1 first.
func (r *Resolver) resolveProvider(
	ctx context.Context, namespace string, w v1alpha1.ProviderWatch,
) (v1alpha1.Artifact, error) {
	if r.Client == nil {
		return v1alpha1.Artifact{}, fmt.Errorf(
			"provider watch on %s: no client to read it with", w.Name)
	}

	var obj unstructured.Unstructured
	obj.SetGroupVersionKind(providerGVK)
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: w.Name}, &obj); err != nil {
		return v1alpha1.Artifact{}, fmt.Errorf(
			"reading ResourceSetInputProvider %s/%s: %w", namespace, w.Name, err)
	}

	// A provider that is not Ready has exported nothing, or has exported
	// something it has since failed to refresh. Minting a Bundle from stale
	// inputs pins the wrong artifact and looks exactly like a correct one, so
	// this refuses rather than reading what is there.
	if ready, reason := providerReady(&obj); !ready {
		return v1alpha1.Artifact{}, &ErrNoMatch{Reason: fmt.Sprintf(
			"ResourceSetInputProvider %s is not ready: %s", w.Name, reason)}
	}

	inputs, found, err := unstructured.NestedSlice(obj.Object, "status", "exportedInputs")
	if err != nil {
		return v1alpha1.Artifact{}, fmt.Errorf(
			"reading the inputs of ResourceSetInputProvider %s: %w", w.Name, err)
	}
	if !found || len(inputs) == 0 {
		// Ready and empty is a real state: every tag was filtered out. Not an
		// error — the watch is correctly configured and has nothing to offer.
		return v1alpha1.Artifact{}, &ErrNoMatch{Reason: fmt.Sprintf(
			"ResourceSetInputProvider %s exported no inputs", w.Name)}
	}

	newest, ok := inputs[0].(map[string]any)
	if !ok {
		return v1alpha1.Artifact{}, fmt.Errorf(
			"ResourceSetInputProvider %s exported an input that is not an object", w.Name)
	}
	return artifactFrom(&obj, newest, w.Name)
}

// artifactFrom maps an exported input onto an Artifact by the keys it carries.
//
// The keys are the provider's contract, and they differ by kind: the four
// OCI-family providers export id/tag/digest, and the git ones export
// id/sha/branch/tag/author/title. Deciding by what is present rather than by
// reading spec.type means a provider kind added upstream works here if it
// exports the same keys, and is refused clearly if it does not.
func artifactFrom(obj *unstructured.Unstructured, input map[string]any, name string) (v1alpha1.Artifact, error) {
	url, _, _ := unstructured.NestedString(obj.Object, "spec", "url")

	// A commit: the git providers are the only ones that export a sha, and it
	// is what a promotion pins.
	if sha := text(input["sha"]); sha != "" {
		return v1alpha1.Artifact{Commit: &v1alpha1.CommitArtifact{
			Repo:    url,
			SHA:     sha,
			Branch:  text(input["branch"]),
			Tag:     text(input["tag"]),
			Message: text(input["title"]),
		}}, nil
	}

	// An image: tag and digest, with the repository on the provider itself.
	if tag := text(input["tag"]); tag != "" {
		return v1alpha1.Artifact{Image: &v1alpha1.ImageArtifact{
			// oci:// is how a provider names a registry and not how an image
			// reference does, so it is stripped here rather than leaking into
			// every Bundle and every rendered manifest.
			Repo:   strings.TrimPrefix(url, "oci://"),
			Tag:    tag,
			Digest: text(input["digest"]),
		}}, nil
	}

	// Static and ExternalService providers export whatever their author chose,
	// which may be nothing Hecate can promote. Said plainly rather than
	// producing an empty Artifact that fails later with less to go on.
	return v1alpha1.Artifact{}, fmt.Errorf(
		"ResourceSetInputProvider %s exported an input with neither sha nor tag, "+
			"so there is nothing to promote: %v", name, keysOf(input))
}

// providerReady reports the provider's Ready condition.
func providerReady(obj *unstructured.Unstructured) (bool, string) {
	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		// No conditions at all means it has not reconciled yet.
		return false, "it has not reported a status yet"
	}
	for _, c := range conditions {
		cond, ok := c.(map[string]any)
		if !ok || text(cond["type"]) != "Ready" {
			continue
		}
		if text(cond["status"]) == "True" {
			return true, ""
		}
		reason := text(cond["message"])
		if reason == "" {
			reason = text(cond["reason"])
		}
		return false, reason
	}
	return false, "it has no Ready condition"
}

func text(v any) string {
	s, _ := v.(string)
	return s
}

// keysOf names what an unusable input did carry, so the error is actionable.
// Sorted, because map order is random and an error message that reorders
// itself between reconciles is one nobody can grep for.
func keysOf(input map[string]any) []string {
	out := make([]string, 0, len(input))
	for k := range input {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
