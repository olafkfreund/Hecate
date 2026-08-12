package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=gt
// +kubebuilder:printcolumn:name="Current",type=string,JSONPath=`.status.current.bundle`
// +kubebuilder:printcolumn:name="Health",type=string,JSONPath=`.status.health.status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Gate is an environment, and the threshold a Bundle must cross to enter it.
//
// Gates form the pipeline: a Gate that admits Bundles only after they have
// cleared an upstream Gate is downstream of it. There is no separate pipeline
// object — the graph is implied by what each Gate admits, so it cannot drift
// out of sync with reality.
type Gate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GateSpec   `json:"spec"`
	Status GateStatus `json:"status,omitempty"`
}

// GateSpec describes what this Gate admits and what crossing it involves.
type GateSpec struct {
	// Admits declares which Bundles may cross this Gate.
	//
	// +kubebuilder:validation:MinItems=1
	Admits []Admission `json:"admits"`
	// Passage is the sequence of steps that moves an admitted Bundle into this
	// environment. Typically: render the new state, commit it, push it, and
	// wait for the delivery engine to apply it.
	//
	// +optional
	Passage *PassageTemplate `json:"passage,omitempty"`
	// Watch describes what to monitor to decide whether this Gate is healthy
	// once a Bundle is in place.
	//
	// +optional
	Watch []HealthCheck `json:"watch,omitempty"`
	// Verify describes evidence that must be gathered after crossing before
	// downstream Gates will admit the Bundle. Health says "it is running";
	// verification says "it is working".
	//
	// +optional
	Verify []Verification `json:"verify,omitempty"`
	// Auto admits and crosses eligible Bundles without waiting to be asked.
	// Sensible for dev, rarely for production.
	//
	// +optional
	Auto bool `json:"auto,omitempty"`
	// Windows restricts when Passages may start. Outside every window, eligible
	// Bundles queue rather than fail.
	//
	// +optional
	Windows []Window `json:"windows,omitempty"`
	// Suspend stops new Passages without deleting the Gate. In-flight Passages
	// are unaffected.
	//
	// +optional
	Suspend bool `json:"suspend,omitempty"`
	// Vars are values usable in expressions throughout this Gate's Passage.
	//
	// +optional
	Vars []Var `json:"vars,omitempty"`
	// Retain bounds how many finished Passages this Gate keeps. Older ones
	// beyond the limit are deleted, newest first.
	//
	// Zero means keep everything, which is what an unset-looking field should
	// do. Unfinished Passages, and the one that produced what is currently in
	// the Gate, are never collected whatever this says.
	//
	// A Passage is the record of *how* a crossing happened, so the default is
	// higher than a Beacon's: Gates produce far fewer objects than Beacons, and
	// each is worth more. Long-term history belongs in the evidence store
	// rather than in etcd (D13).
	//
	// +optional
	// +kubebuilder:validation:Minimum=0
	Retain *int32 `json:"retain,omitempty"`
	// Evidence binds this Gate to the compliance system that records what
	// crossed it and decides what may.
	//
	// +optional
	Evidence *EvidenceConfig `json:"evidence,omitempty"`
}

// EvidenceConfig binds a Gate to a Fides environment.
//
// One block rather than a scatter of top-level fields, because the rest of the
// compliance settings — which flow a crossing's trail belongs to, what
// change-gate risk score is tolerable — land here too.
type EvidenceConfig struct {
	// FidesEnvironment is the Fides environment this Gate corresponds to.
	//
	// Explicit, and a UUID, because that is what the environment-scoped Fides
	// checks take: `/api/v1/environments/{uuid}/policy-check` and
	// `/api/v1/environments/{uuid}/allowlist`. There is no convention that
	// could produce one — an environment's name is not its key — and a
	// convention that silently resolved to the wrong environment would check
	// the wrong policy while reporting success, which is the worst failure a
	// compliance control can have.
	//
	// +kubebuilder:validation:Pattern=`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`
	FidesEnvironment string `json:"fidesEnvironment"`
	// ServerURL is the Fides server. Empty falls back to the controller's
	// --fides-server flag, so a fleet with one Fides says it once.
	//
	// +optional
	ServerURL string `json:"serverURL,omitempty"`
	// CredentialsRef names a Secret holding a Fides API key under `token`.
	//
	// +optional
	CredentialsRef *LocalSecretRef `json:"credentialsRef,omitempty"`
}

// Admission declares one class of Bundle this Gate accepts.
type Admission struct {
	// From names the Beacon whose Bundles this admission covers.
	From BundleOrigin `json:"from"`
	// After lists upstream Gates a Bundle must have cleared first. Empty means
	// the Bundle may come straight from the Beacon — the entry point of a
	// pipeline.
	//
	// +optional
	After []string `json:"after,omitempty"`
	// RequireApproval blocks crossing until a human explicitly approves the
	// Bundle for this Gate, regardless of upstream state.
	//
	// +optional
	RequireApproval bool `json:"requireApproval,omitempty"`
}

// BundleOrigin identifies where admitted Bundles come from.
type BundleOrigin struct {
	// Beacon is the emitting Beacon's name, in this namespace.
	Beacon string `json:"beacon"`
}

// PassageTemplate is the recipe for crossing this Gate.
type PassageTemplate struct {
	// Steps run in order. Each may reference earlier steps' outputs.
	//
	// +kubebuilder:validation:MinItems=1
	Steps []Step `json:"steps"`
}

// HealthCheck asks a registered checker to assess something.
type HealthCheck struct {
	// Uses names a registered health checker, e.g. "flux".
	Uses string `json:"uses"`
	// With is the checker's configuration.
	//
	// +optional
	With *apiextensionsv1.JSON `json:"with,omitempty"`
}

// Verification gathers evidence that a crossing actually worked.
type Verification struct {
	// Uses names a registered verifier, e.g. "flagger" or "fides".
	Uses string `json:"uses"`
	// With is the verifier's configuration.
	//
	// +optional
	With *apiextensionsv1.JSON `json:"with,omitempty"`
}

// Window is a recurring period during which Passages may start.
type Window struct {
	// Schedule is a cron expression marking when the window opens.
	Schedule string `json:"schedule"`
	// Duration is how long it stays open once opened.
	Duration metav1.Duration `json:"duration"`
	// TimeZone is an IANA name, e.g. "Europe/London". Defaults to UTC.
	//
	// Set this deliberately: a window defined in UTC drifts an hour against
	// local working time twice a year, which is exactly when someone is
	// surprised by a deploy.
	//
	// +optional
	TimeZone string `json:"timeZone,omitempty"`
	// Deny inverts the window: instead of the only time Passages may start,
	// it becomes the only time they may not. For change freezes.
	//
	// +optional
	Deny bool `json:"deny,omitempty"`
}

// Var is a named value usable in expressions.
type Var struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// GateStatus reports what is in this Gate and how it is doing.
type GateStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// LastHandledReconcileAt echoes the value of the
	// reconcile.fluxcd.io/requestedAt annotation from the last reconcile that
	// acted on it.
	//
	// Without this a caller cannot tell whether its request landed, only that
	// *a* reconcile happened — so a CI job asking for an immediate poll has
	// nothing to wait for. Echoing the caller's own opaque token also
	// distinguishes it from someone else's request (D44).
	//
	// +optional
	LastHandledReconcileAt string `json:"lastHandledReconcileAt,omitempty"`
	// Current is the Bundle presently in this environment.
	//
	// +optional
	Current *GateOccupant `json:"current,omitempty"`
	// Health is the latest assessment from this Gate's watches.
	//
	// +optional
	Health *HealthReport `json:"health,omitempty"`
	// Eligible lists Bundles that could cross now but have not. This is the
	// queue a human acts on, and the answer to "what am I waiting for?"
	//
	// +optional
	Eligible []string `json:"eligible,omitempty"`
	// ActivePassage is the in-flight Passage, if any.
	//
	// +optional
	ActivePassage string `json:"activePassage,omitempty"`
	// History is the recent record of what crossed, newest first.
	//
	// +optional
	History []GateOccupant `json:"history,omitempty"`
	// Evidence is the change gate's verdict for the crossing in progress.
	//
	// Mirrored from the active Passage and **cleared when there is no
	// crossing**, so it can never be a stale verdict presented as current. The
	// question it answers is "why is this sitting there?", which is asked while
	// the crossing is stuck, not afterwards — a finished one is in `history`
	// and in the Passage that produced it.
	//
	// +optional
	Evidence *EvidenceRef `json:"evidence,omitempty"`
}

// GateOccupant records a Bundle's tenure in a Gate.
type GateOccupant struct {
	// Bundle is the Bundle's object name.
	Bundle string `json:"bundle"`
	// Digest is its content address.
	//
	// +optional
	Digest string `json:"digest,omitempty"`
	// Passage is the Passage that brought it here.
	//
	// +optional
	Passage string `json:"passage,omitempty"`
	// EnteredAt is when the crossing completed.
	EnteredAt metav1.Time `json:"enteredAt"`
	// Actor is who initiated the crossing.
	//
	// +optional
	Actor string `json:"actor,omitempty"`
	// Verified reports whether this Bundle's verification succeeded here.
	// Downstream Gates should refuse to admit an unverified Bundle.
	//
	// +optional
	Verified *bool `json:"verified,omitempty"`
}

// +kubebuilder:object:root=true

// GateList is a list of Gates.
type GateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Gate `json:"items"`
}
