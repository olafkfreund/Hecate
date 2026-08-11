package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=psg
// +kubebuilder:printcolumn:name="Gate",type=string,JSONPath=`.spec.gate`
// +kubebuilder:printcolumn:name="Bundle",type=string,JSONPath=`.spec.bundle`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Passage is one attempt to move a Bundle through a Gate.
//
// A Passage is created, runs its steps to completion, and is then a permanent
// record of what happened. It is never reused: a second attempt is a second
// Passage. That is what makes the history trustworthy — nothing is overwritten.
type Passage struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PassageSpec   `json:"spec"`
	Status PassageStatus `json:"status,omitempty"`
}

// PassageSpec is what this Passage was asked to do. It is immutable once
// created; the only mutable field is Abort.
type PassageSpec struct {
	// Gate is the Gate being crossed.
	Gate string `json:"gate"`
	// Bundle is the Bundle being moved.
	Bundle string `json:"bundle"`
	// Steps is the resolved step list, copied from the Gate at creation time.
	//
	// Copied rather than referenced on purpose: editing a Gate must not
	// retroactively change what an in-flight or completed Passage did.
	//
	// +kubebuilder:validation:MinItems=1
	Steps []Step `json:"steps"`
	// Actor is who initiated this Passage — a username, or "controller" for
	// automatic crossings.
	//
	// +optional
	Actor string `json:"actor,omitempty"`
	// Abort requests that a running Passage stop. Set it to true to cancel;
	// the controller will mark remaining steps Aborted.
	//
	// +optional
	Abort bool `json:"abort,omitempty"`
}

// PassagePhase is the overall outcome of a Passage.
//
// +kubebuilder:validation:Enum={Pending,Running,Succeeded,Failed,Aborted}
type PassagePhase string

const (
	// PassagePending means it has not started — usually waiting on a window,
	// an approval, or a verification gate.
	PassagePending PassagePhase = "Pending"
	// PassageRunning means steps are executing.
	PassageRunning PassagePhase = "Running"
	// PassageSucceeded means every step completed and the Bundle is in the Gate.
	PassageSucceeded PassagePhase = "Succeeded"
	// PassageFailed means a step failed terminally.
	PassageFailed PassagePhase = "Failed"
	// PassageAborted means a human stopped it.
	PassageAborted PassagePhase = "Aborted"
)

// Terminal reports whether the Passage has finished for good.
func (p PassagePhase) Terminal() bool {
	switch p {
	case PassageSucceeded, PassageFailed, PassageAborted:
		return true
	default:
		return false
	}
}

// PassageStatus is what actually happened.
type PassageStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Phase is the overall outcome.
	//
	// +kubebuilder:default=Pending
	// +optional
	Phase PassagePhase `json:"phase,omitempty"`
	// Message explains the phase, especially a non-success one.
	//
	// +optional
	Message string `json:"message,omitempty"`
	// Steps records each step's outcome, in spec order.
	//
	// +optional
	Steps []StepStatus `json:"steps,omitempty"`
	// CurrentStep indexes the step being executed.
	//
	// +optional
	CurrentStep int32 `json:"currentStep,omitempty"`
	// StartedAt is when the first step ran.
	//
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// FinishedAt is when the Passage reached a terminal phase.
	//
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
	// Watch are the health checks this Passage's steps asked the Gate to adopt.
	//
	// A step that waited for something — `flux-wait` waiting on a Kustomization,
	// say — already knows what to watch, and the Gate should keep watching it
	// afterwards. Without this the Gate goes blind the moment the Passage ends,
	// and the operator has to restate the same resources in `gate.spec.watch`.
	//
	// +optional
	Watch []HealthCheck `json:"watch,omitempty"`
	// TraceID is the OpenTelemetry trace this Passage belongs to, so a
	// promotion can be correlated with the CI run that produced the artifact
	// and the reconciliation that applied it.
	//
	// +optional
	TraceID string `json:"traceID,omitempty"`
	// Evidence references the compliance record produced for this Passage.
	//
	// +optional
	Evidence *EvidenceRef `json:"evidence,omitempty"`
}

// EvidenceRef points at the external compliance record for a Passage.
type EvidenceRef struct {
	// Trail is the Fides trail identifier for this Passage.
	//
	// +optional
	Trail string `json:"trail,omitempty"`
	// Verdict is the change-gate outcome, e.g. "approve" or "hold".
	//
	// +optional
	Verdict string `json:"verdict,omitempty"`
	// Risk is the change-gate risk score, 0-100.
	//
	// +optional
	Risk *int32 `json:"risk,omitempty"`
	// URL links to the full record.
	//
	// +optional
	URL string `json:"url,omitempty"`
}

// +kubebuilder:object:root=true

// PassageList is a list of Passages.
type PassageList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Passage `json:"items"`
}
