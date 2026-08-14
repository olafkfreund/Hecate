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
func (f *Flagger) Verify(ctx context.Context, namespace string, raw []byte) (Result, error) {
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
		return Result{Verified: true, Done: true,
			Reason: fmt.Sprintf("Canary %s succeeded", cfg.Name)}, nil

	case "Failed", "Terminated":
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
