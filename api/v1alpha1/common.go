package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Health describes how well something is doing. The vocabulary is deliberately
// small: any more states and nobody agrees what they mean.
//
// +kubebuilder:validation:Enum={Healthy,Progressing,Degraded,Unknown,NotApplicable}
type Health string

const (
	// HealthHealthy means it is doing what it is supposed to.
	HealthHealthy Health = "Healthy"
	// HealthProgressing means it has not converged yet, but something is still
	// working on it. This is a state you wait in, not one you act on.
	HealthProgressing Health = "Progressing"
	// HealthDegraded means it has stopped converging and will not recover
	// without intervention.
	HealthDegraded Health = "Degraded"
	// HealthUnknown means we could not tell. Distinct from Degraded on purpose:
	// "I cannot see it" is different from "I can see it and it is broken".
	HealthUnknown Health = "Unknown"
	// HealthNotApplicable means nothing was configured to check.
	HealthNotApplicable Health = "NotApplicable"
)

// healthSeverity orders health states from best to worst so a set of things can
// be reduced to a single answer.
var healthSeverity = map[Health]int{
	HealthHealthy:       0,
	HealthNotApplicable: 1,
	HealthProgressing:   2,
	HealthUnknown:       3,
	HealthDegraded:      4,
}

// Merge returns the worse of two Health values. A set is only as healthy as its
// unhealthiest member.
func (h Health) Merge(other Health) Health {
	if healthSeverity[h] > healthSeverity[other] {
		return h
	}
	return other
}

// HealthReport is the outcome of assessing something's health, with the
// reasoning attached. A bare status with no explanation is not actionable.
type HealthReport struct {
	// Status is the overall assessment.
	Status Health `json:"status"`
	// Issues explain any non-Healthy status, in human-readable form.
	//
	// +optional
	Issues []string `json:"issues,omitempty"`
	// Details is opaque, check-specific output for the UI and CLI.
	//
	// +optional
	Details *apiextensionsv1.JSON `json:"details,omitempty"`
	// ObservedAt is when this assessment was made.
	//
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
}

// Step is one instruction in a Passage.
type Step struct {
	// Uses names a registered step implementation, e.g. "flux-wait".
	Uses string `json:"uses"`
	// As names this step's output so later steps can reference it, e.g. a step
	// with `as: commit` exposes `${{ steps.commit.sha }}`.
	//
	// +optional
	As string `json:"as,omitempty"`
	// With is the step's configuration. Its shape is defined by the step
	// implementation, which publishes a JSON Schema for validation and for
	// generating the UI form.
	//
	// +optional
	With *apiextensionsv1.JSON `json:"with,omitempty"`
	// If is an expression that must evaluate true for the step to run. Skipped
	// steps are recorded as Skipped, not silently omitted.
	//
	// +optional
	If string `json:"if,omitempty"`
	// Timeout bounds how long this step may run before it is abandoned.
	//
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`
	// ContinueOnError lets the Passage proceed even if this step fails. Use
	// sparingly: it is how a broken deploy gets reported as a success.
	//
	// +optional
	ContinueOnError bool `json:"continueOnError,omitempty"`
}

// StepPhase is the outcome of a single step.
//
// +kubebuilder:validation:Enum={Pending,Running,Succeeded,Failed,Skipped,Aborted}
type StepPhase string

const (
	StepPending   StepPhase = "Pending"
	StepRunning   StepPhase = "Running"
	StepSucceeded StepPhase = "Succeeded"
	StepFailed    StepPhase = "Failed"
	StepSkipped   StepPhase = "Skipped"
	StepAborted   StepPhase = "Aborted"
)

// Terminal reports whether a step has reached a state it will not leave.
func (p StepPhase) Terminal() bool {
	switch p {
	case StepSucceeded, StepFailed, StepSkipped, StepAborted:
		return true
	default:
		return false
	}
}

// StepStatus records what happened when a step ran.
type StepStatus struct {
	// Uses is the step implementation that ran.
	Uses string `json:"uses"`
	// As is the step's output name, if it had one.
	//
	// +optional
	As string `json:"as,omitempty"`
	// Phase is the outcome.
	Phase StepPhase `json:"phase"`
	// Reason is a stable, machine-readable code for a failure, in PascalCase —
	// GitAuthFailed, FluxStalled, InvalidConfig.
	//
	// Message is for a human reading one failure; Reason is for everything that
	// has to reason across many: `hecate diagnose`, a dashboard counting failure
	// classes, an operator asking "is this the same problem as yesterday?".
	//
	// +optional
	Reason string `json:"reason,omitempty"`
	// Message explains the phase.
	//
	// +optional
	Message string `json:"message,omitempty"`
	// Output is what the step produced, available to later steps.
	//
	// +optional
	Output *apiextensionsv1.JSON `json:"output,omitempty"`
	// StartedAt is when the step first ran.
	//
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// FinishedAt is when the step reached a terminal phase.
	//
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
	// Attempts counts how many times this step has been invoked. Steps that
	// wait on external systems are invoked repeatedly until they settle.
	//
	// +optional
	Attempts int32 `json:"attempts,omitempty"`
}

// Condition type names used across Hecate resources.
const (
	// ConditionReady means the resource is doing its job.
	ConditionReady = "Ready"
	// ConditionReconciling means the controller is actively working on it.
	ConditionReconciling = "Reconciling"
	// ConditionStalled means the controller has stopped retrying.
	ConditionStalled = "Stalled"
)
