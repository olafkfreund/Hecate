package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=bnd
// +kubebuilder:printcolumn:name="Beacon",type=string,JSONPath=`.spec.beacon`
// +kubebuilder:printcolumn:name="Digest",type=string,JSONPath=`.spec.digest`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Bundle is an immutable, content-addressed set of artifact versions — for
// example one git commit plus two container images — that move through the
// pipeline together.
//
// A Bundle is never edited. Any change in artifact versions is a different
// Bundle with a different digest. Artifacts that need to move at different
// cadences belong in different Beacons, and therefore different Bundles.
type Bundle struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BundleSpec   `json:"spec"`
	Status BundleStatus `json:"status,omitempty"`
}

// BundleSpec is the immutable content of a Bundle.
type BundleSpec struct {
	// Beacon names the Beacon that produced this Bundle.
	Beacon string `json:"beacon"`
	// Digest is the content address of this Bundle: a deterministic hash over
	// its artifact versions. Two Bundles with identical artifacts always have
	// the same digest, which is what makes "has this exact thing already been
	// through staging?" answerable.
	//
	// Set by the controller; ignored on input.
	//
	// +optional
	Digest string `json:"digest,omitempty"`
	// Alias is an optional short human-readable handle, so people can say
	// "promote wandering-owl" instead of reciting a hash.
	//
	// +optional
	Alias string `json:"alias,omitempty"`
	// Artifacts are the versioned things this Bundle carries.
	//
	// +kubebuilder:validation:MinItems=1
	Artifacts []Artifact `json:"artifacts"`
}

// Artifact is one versioned thing. Exactly one field must be set.
//
// +kubebuilder:validation:MaxProperties=1
// +kubebuilder:validation:MinProperties=1
type Artifact struct {
	// +optional
	Image *ImageArtifact `json:"image,omitempty"`
	// +optional
	Chart *ChartArtifact `json:"chart,omitempty"`
	// +optional
	Commit *CommitArtifact `json:"commit,omitempty"`
}

// ImageArtifact is a container image at a specific version.
type ImageArtifact struct {
	// Repo is the image repository, e.g. ghcr.io/acme/podinfo.
	Repo string `json:"repo"`
	// Tag is the resolved tag.
	//
	// +optional
	Tag string `json:"tag,omitempty"`
	// Digest is the image digest, e.g. sha256:abc... This is what gets reported
	// to Fides as an artifact, and what makes the promotion auditable — a tag
	// can be moved, a digest cannot.
	//
	// +optional
	Digest string `json:"digest,omitempty"`
}

// ChartArtifact is a Helm chart at a specific version.
type ChartArtifact struct {
	// Repo is the chart repository — an HTTPS Helm repo or an OCI registry.
	Repo string `json:"repo"`
	// Name is the chart name. Empty for OCI charts, where the repo already
	// identifies the chart.
	//
	// +optional
	Name string `json:"name,omitempty"`
	// Version is the resolved chart version.
	Version string `json:"version"`
}

// CommitArtifact is a git commit.
type CommitArtifact struct {
	// Repo is the git repository URL.
	Repo string `json:"repo"`
	// SHA is the full commit SHA.
	SHA string `json:"sha"`
	// Branch or Tag records how the commit was found, for display.
	//
	// +optional
	Branch string `json:"branch,omitempty"`
	// +optional
	Tag string `json:"tag,omitempty"`
	// Message is the commit subject, for display.
	//
	// +optional
	Message string `json:"message,omitempty"`
}

// BundleStatus records where this Bundle has been.
type BundleStatus struct {
	// Cleared lists the Gates this Bundle has successfully passed, in order.
	// This is the "how did it get to prod?" record.
	//
	// +optional
	Cleared []GateCrossing `json:"cleared,omitempty"`
	// Blocked lists Gates that rejected this Bundle, with the reason.
	//
	// +optional
	Blocked []GateCrossing `json:"blocked,omitempty"`
	// ApprovedFor lists Gates a human has explicitly approved this Bundle for,
	// letting it skip the normal upstream ordering.
	//
	// +optional
	ApprovedFor []string `json:"approvedFor,omitempty"`
}

// GateCrossing records one Bundle/Gate outcome.
type GateCrossing struct {
	// Gate is the Gate in question.
	Gate string `json:"gate"`
	// Passage is the Passage that produced this outcome.
	//
	// +optional
	Passage string `json:"passage,omitempty"`
	// At is when it happened.
	At metav1.Time `json:"at"`
	// Actor is who caused it — a user, or the controller for automatic passage.
	//
	// +optional
	Actor string `json:"actor,omitempty"`
	// Reason explains a block.
	//
	// +optional
	Reason string `json:"reason,omitempty"`
}

// ComputeDigest returns the content address of a set of artifacts.
//
// Artifacts are canonicalised and sorted before hashing so that ordering in the
// spec never changes the digest — otherwise the same release discovered in a
// different order would look like a different Bundle and get promoted twice.
func ComputeDigest(artifacts []Artifact) string {
	lines := make([]string, 0, len(artifacts))
	for _, a := range artifacts {
		switch {
		case a.Image != nil:
			// Prefer the digest: tags move, digests do not. Two Bundles that
			// differ only by a re-pointed tag must hash the same.
			ver := a.Image.Digest
			if ver == "" {
				ver = a.Image.Tag
			}
			lines = append(lines, fmt.Sprintf("image\x00%s\x00%s", a.Image.Repo, ver))
		case a.Chart != nil:
			lines = append(lines, fmt.Sprintf("chart\x00%s\x00%s\x00%s", a.Chart.Repo, a.Chart.Name, a.Chart.Version))
		case a.Commit != nil:
			lines = append(lines, fmt.Sprintf("commit\x00%s\x00%s", a.Commit.Repo, a.Commit.SHA))
		}
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

// Name returns the object name for a Bundle with the given digest: the Beacon
// name and a short digest, which keeps `kubectl get bundles` readable while
// staying collision-safe in practice.
func BundleName(beacon, digest string) string {
	if len(digest) > 12 {
		digest = digest[:12]
	}
	return fmt.Sprintf("%s-%s", beacon, digest)
}

// HasCleared reports whether this Bundle has successfully passed the named Gate.
func (b *Bundle) HasCleared(gate string) bool {
	for _, c := range b.Status.Cleared {
		if c.Gate == gate {
			return true
		}
	}
	return false
}

// IsApprovedFor reports whether a human has explicitly approved this Bundle for
// the named Gate, bypassing upstream ordering.
func (b *Bundle) IsApprovedFor(gate string) bool {
	for _, g := range b.Status.ApprovedFor {
		if g == gate {
			return true
		}
	}
	return false
}

// +kubebuilder:object:root=true

// BundleList is a list of Bundles.
type BundleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Bundle `json:"items"`
}
