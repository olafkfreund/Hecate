package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=bcn
// +kubebuilder:printcolumn:name="Interval",type=string,JSONPath=`.spec.interval`
// +kubebuilder:printcolumn:name="Latest",type=string,JSONPath=`.status.latestBundle`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Beacon watches artifact sources and emits a Bundle whenever it sees a new
// combination worth promoting.
//
// One Beacon per set of artifacts that move together. Two services released on
// independent schedules want two Beacons, so a change to one does not drag the
// other through the pipeline with it.
type Beacon struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BeaconSpec   `json:"spec"`
	Status BeaconStatus `json:"status,omitempty"`
}

// BeaconSpec describes what to watch and when to emit.
type BeaconSpec struct {
	// Interval is how often to poll the watched sources. Sources that support
	// webhooks will also emit on push; the interval is the safety net, not the
	// primary mechanism.
	//
	// +kubebuilder:default="5m"
	// +optional
	Interval metav1.Duration `json:"interval,omitempty"`
	// Watch lists the artifact sources to observe.
	//
	// +kubebuilder:validation:MinItems=1
	Watch []WatchSource `json:"watch"`
	// Emit controls when Bundles are created.
	//
	// +kubebuilder:default=Automatic
	// +optional
	Emit EmitPolicy `json:"emit,omitempty"`
	// Retain bounds how many *unreferenced* Bundles from this Beacon are kept.
	//
	// A Beacon polling every couple of minutes emits indefinitely; unbounded,
	// that is etcd growth with no ceiling and a `kubectl get bundles` nobody can
	// read. Zero disables collection entirely.
	//
	// Bundles in use are never collected regardless of this value — see the
	// safety rule in D13. The long-term record belongs in the evidence store,
	// not in etcd.
	//
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=0
	// +optional
	Retain *int32 `json:"retain,omitempty"`
	// Suspend stops this Beacon from discovering anything new, without
	// deleting it. Existing Bundles are unaffected.
	//
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// EmitPolicy controls Bundle creation.
//
// +kubebuilder:validation:Enum={Automatic,Manual}
type EmitPolicy string

const (
	// EmitAutomatic creates a Bundle as soon as a new artifact combination is
	// discovered.
	EmitAutomatic EmitPolicy = "Automatic"
	// EmitManual discovers artifacts but never creates Bundles on its own.
	// Someone must ask for one.
	EmitManual EmitPolicy = "Manual"
)

// WatchSource is one artifact source. Exactly one field must be set.
//
// +kubebuilder:validation:MaxProperties=1
// +kubebuilder:validation:MinProperties=1
type WatchSource struct {
	// +optional
	Image *ImageWatch `json:"image,omitempty"`
	// +optional
	Chart *ChartWatch `json:"chart,omitempty"`
	// +optional
	Git *GitWatch `json:"git,omitempty"`
}

// ImageWatch watches a container image repository.
type ImageWatch struct {
	// Repo is the image repository, e.g. ghcr.io/acme/podinfo.
	Repo string `json:"repo"`
	// Select is how to pick the newest tag.
	//
	// +kubebuilder:default=SemVer
	// +optional
	Select TagSelection `json:"select,omitempty"`
	// Constraint narrows the selection. For SemVer this is a range such as
	// "^6.0.0"; for Lexical and NewestBuild it is ignored.
	//
	// +optional
	Constraint string `json:"constraint,omitempty"`
	// Allow is a regular expression tags must match. Use it to exclude the
	// mutable tags that otherwise cause surprise promotions — `latest`,
	// `main`, nightly builds.
	//
	// +optional
	Allow string `json:"allow,omitempty"`
	// Ignore lists exact tags to never select.
	//
	// +optional
	Ignore []string `json:"ignore,omitempty"`
	// Platform restricts digest resolution to one platform for multi-arch
	// images, e.g. "linux/amd64".
	//
	// +optional
	Platform string `json:"platform,omitempty"`
	// CredentialsRef names a Secret holding registry credentials. Omit it to
	// use the controller's ambient cloud identity (IRSA, Workload Identity,
	// Managed Identity).
	//
	// +optional
	CredentialsRef *LocalSecretRef `json:"credentialsRef,omitempty"`
	// Insecure allows a plain-HTTP registry: one that terminates TLS elsewhere,
	// an air-gapped one, or a local development registry.
	//
	// False unless asked for, and deliberately so — downgrading to HTTP without
	// being told to would send the registry credentials above in clear. Matches
	// Flux's own `OCIRepository.spec.insecure`.
	//
	// +optional
	Insecure bool `json:"insecure,omitempty"`
}

// TagSelection is how to choose among available tags.
//
// +kubebuilder:validation:Enum={SemVer,Lexical,NewestBuild,Digest}
type TagSelection string

const (
	// SelectSemVer picks the highest semantic version satisfying Constraint.
	SelectSemVer TagSelection = "SemVer"
	// SelectLexical picks the lexically greatest tag. For zero-padded date or
	// build-number schemes.
	SelectLexical TagSelection = "Lexical"
	// SelectNewestBuild picks the most recently pushed tag. Convenient, but it
	// trusts registry timestamps, which are not always what you think.
	SelectNewestBuild TagSelection = "NewestBuild"
	// SelectDigest tracks one fixed tag and reacts when its digest changes.
	// For teams that deliberately move a tag like `stable`.
	SelectDigest TagSelection = "Digest"
)

// ChartWatch watches a Helm chart repository.
type ChartWatch struct {
	// Repo is an HTTPS Helm repository or an OCI registry reference.
	Repo string `json:"repo"`
	// Name is the chart name. Omit for OCI, where Repo identifies the chart.
	//
	// +optional
	Name string `json:"name,omitempty"`
	// Constraint is a semantic version range, e.g. "^6.0.0".
	//
	// +optional
	Constraint string `json:"constraint,omitempty"`
	// +optional
	CredentialsRef *LocalSecretRef `json:"credentialsRef,omitempty"`
}

// GitWatch watches a git repository.
type GitWatch struct {
	// Repo is the repository URL.
	Repo string `json:"repo"`
	// Branch to follow. Mutually exclusive with Tags.
	//
	// +optional
	Branch string `json:"branch,omitempty"`
	// Tags selects by tag instead of by branch head.
	//
	// +optional
	Tags *TagWatch `json:"tags,omitempty"`
	// Paths restricts which changes count as new. A commit touching nothing in
	// these paths produces no Bundle — this is what keeps a monorepo from
	// promoting every service on every commit.
	//
	// +optional
	Paths []string `json:"paths,omitempty"`
	// IgnorePaths is the inverse, applied after Paths.
	//
	// +optional
	IgnorePaths []string `json:"ignorePaths,omitempty"`
	// +optional
	CredentialsRef *LocalSecretRef `json:"credentialsRef,omitempty"`
}

// TagWatch selects git tags.
type TagWatch struct {
	// +kubebuilder:default=SemVer
	// +optional
	Select TagSelection `json:"select,omitempty"`
	// +optional
	Constraint string `json:"constraint,omitempty"`
	// +optional
	Allow string `json:"allow,omitempty"`
	// +optional
	Ignore []string `json:"ignore,omitempty"`
}

// LocalSecretRef names a Secret in the same namespace.
type LocalSecretRef struct {
	Name string `json:"name"`
}

// BeaconStatus reports what the Beacon has seen.
type BeaconStatus struct {
	// ObservedGeneration is the spec generation this status describes. If it
	// lags metadata.generation, this status is about the previous spec.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions follow the standard Kubernetes convention.
	//
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// LastPolled is when the sources were last checked.
	//
	// +optional
	LastPolled *metav1.Time `json:"lastPolled,omitempty"`
	// LatestBundle is the most recent Bundle this Beacon emitted.
	//
	// +optional
	LatestBundle string `json:"latestBundle,omitempty"`
	// Discovered is the newest artifact version seen per source, whether or not
	// it produced a Bundle. Useful for answering "why has nothing been emitted?"
	//
	// +optional
	Discovered []Artifact `json:"discovered,omitempty"`
}

// +kubebuilder:object:root=true

// BeaconList is a list of Beacons.
type BeaconList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Beacon `json:"items"`
}
