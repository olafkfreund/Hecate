// Package verify answers a different question from pkg/health.
//
// Health says "it is running". Verification says "it worked" — and the two
// diverge exactly where it matters: a canary that rolled back leaves a healthy
// Deployment serving the previous version, so every health check passes and
// nothing was delivered. Flagger is the Flux-family answer to the second
// question, and this reads its verdict.
package verify

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// VerifierFlagger is the name used in `verify[].uses`.
const VerifierFlagger = "flagger"

// canaryGVK is Flagger's Canary.
//
// Read as unstructured, per D4: a third-party CRD Hecate does not own and which
// need not be installed. Compiling against Flagger would make it a dependency
// of every build for a feature most Gates will not use.
var canaryGVK = schema.GroupVersionKind{
	Group: "flagger.app", Version: "v1beta1", Kind: "Canary",
}

// Result is what a verifier concluded.
type Result struct {
	// Verified is true only when the evidence says the crossing worked. It is
	// deliberately not a tri-state: the caller needs one answer, and "still
	// running" is not verified yet.
	Verified bool
	// Done is false while the evidence is still being gathered, which is the
	// difference between "not yet" and "no".
	Done bool
	// Reason is why, in the verifier's own words where it has them.
	Reason string
}

// FlaggerConfig selects the Canary to read.
type FlaggerConfig struct {
	// Name is the Canary object.
	Name string `json:"name"`
	// Namespace defaults to the Gate's.
	Namespace string `json:"namespace,omitempty"`
}

// Flagger reads a Canary's verdict.
type Flagger struct{ Client client.Client }

func (f *Flagger) Name() string { return VerifierFlagger }

// Verify reports whether Flagger promoted the canary.
//
// The phases come from the CRD's own enum, and they fall into three groups
// rather than two. Succeeded and Failed are verdicts. Progressing, Promoting,
// Waiting and the rest are "not finished", which must not be reported as a
// failure — a Gate that treated in-progress as failed would refuse every
// crossing that had not already completed by the time it looked.
//
// Initialized is the one that reads wrong at first glance: it means Flagger has
// set the canary up and no analysis has run. That is not success, and treating
// it as such would clear a Bundle on the strength of a canary that never ran.
//
// `since` is when the crossing being judged entered the Gate, and a verdict
// counts only if Flagger reached it after that — see staleVerdict.
func (f *Flagger) Verify(ctx context.Context, namespace string, raw []byte, since time.Time) (Result, error) {
	var cfg FlaggerConfig
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return Result{}, fmt.Errorf("flagger: unusable configuration: %w", err)
		}
	}
	if cfg.Name == "" {
		return Result{}, fmt.Errorf("flagger: no canary name")
	}
	if cfg.Namespace == "" {
		cfg.Namespace = namespace
	}
	if f.Client == nil {
		return Result{}, fmt.Errorf("flagger: no client to read Canary %s with", cfg.Name)
	}

	var canary unstructured.Unstructured
	canary.SetGroupVersionKind(canaryGVK)
	key := client.ObjectKey{Namespace: cfg.Namespace, Name: cfg.Name}
	if err := f.Client.Get(ctx, key, &canary); err != nil {
		// Including not-found: a Gate that names a Canary which does not exist
		// is misconfigured, and silently verifying would clear Bundles on the
		// strength of a canary nobody is running.
		return Result{}, fmt.Errorf("reading Canary %s/%s: %w", cfg.Namespace, cfg.Name, err)
	}

	phase, _, _ := unstructured.NestedString(canary.Object, "status", "phase")
	failed, _, _ := unstructured.NestedInt64(canary.Object, "status", "failedChecks")

	switch phase {
	case "Succeeded":
		if stale := staleVerdict(&canary, since, cfg.Name, phase); stale != "" {
			return Result{Reason: stale}, nil
		}
		return Result{Verified: true, Done: true,
			Reason: fmt.Sprintf("Canary %s succeeded", cfg.Name)}, nil

	case "Failed", "Terminated":
		if stale := staleVerdict(&canary, since, cfg.Name, phase); stale != "" {
			return Result{Reason: stale}, nil
		}
		reason := fmt.Sprintf("Canary %s %s", cfg.Name, phase)
		if failed > 0 {
			reason = fmt.Sprintf("%s after %d failed check(s)", reason, failed)
		}
		// Flagger's own message says more than the phase does — which metric
		// tripped, or which webhook refused.
		if msg := promotedMessage(&canary); msg != "" {
			reason = fmt.Sprintf("%s: %s", reason, msg)
		}
		return Result{Done: true, Reason: reason}, nil

	case "":
		return Result{Reason: fmt.Sprintf("Canary %s has not reported a phase yet", cfg.Name)}, nil

	default:
		// Initializing, Initialized, Waiting, Progressing, WaitingPromotion,
		// Promoting, Finalising, Terminating. None is a verdict.
		return Result{Reason: fmt.Sprintf("Canary %s is %s", cfg.Name, phase)}, nil
	}
}

// staleVerdict reports why a terminal phase is not this crossing's verdict, or
// "" when it is.
//
// A Canary keeps its phase after its analysis ends. One that succeeded for the
// *previous* crossing still reads Succeeded until Flagger notices the new pod
// spec, which takes an analysis interval — and a Gate reconciles the moment its
// Passage finishes, which is inside that window. Reading the phase alone
// therefore clears a Bundle on the strength of the previous crossing's canary:
// the same mistake the Initialized case avoids, in the phase that occurs far
// more often, and undetectable from the deployment afterwards because the
// canary does eventually go green.
//
// So a verdict has to be dated. `status.lastTransitionTime` is when Flagger
// last moved the phase; only a move after the crossing entered the Gate can be
// about what that crossing deployed.
//
// An undated verdict is treated as stale. Waiting is the safe direction — the
// unsafe one is the bug above — and Flagger writes the field unconditionally,
// so this costs nothing until that stops being true, at which point the reason
// says exactly what is missing.
func staleVerdict(canary *unstructured.Unstructured, since time.Time, name, phase string) string {
	raw, found, err := unstructured.NestedString(canary.Object, "status", "lastTransitionTime")
	if err != nil || !found || raw == "" {
		return fmt.Sprintf("Canary %s is %s, but has no status.lastTransitionTime, "+
			"so the verdict cannot be shown to be this crossing's", name, phase)
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return fmt.Sprintf("Canary %s is %s, but its status.lastTransitionTime %q is unreadable, "+
			"so the verdict cannot be shown to be this crossing's", name, phase, raw)
	}
	if at.After(since) {
		return ""
	}
	return fmt.Sprintf("Canary %s is %s from %s, before this crossing — waiting for Flagger "+
		"to analyse what was just deployed", name, phase, at.Format(time.RFC3339))
}

// promotedMessage is Flagger's own account of the outcome, from the Promoted
// condition it maintains.
func promotedMessage(canary *unstructured.Unstructured) string {
	conditions, found, err := unstructured.NestedSlice(canary.Object, "status", "conditions")
	if err != nil || !found {
		return ""
	}
	for _, c := range conditions {
		cond, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := cond["type"].(string); t != "Promoted" {
			continue
		}
		msg, _ := cond["message"].(string)
		return msg
	}
	return ""
}
