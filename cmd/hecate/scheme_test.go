package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// TestClientSchemeKnowsSecrets guards the gap that broke `hecate approve`.
//
// The scheme registered only v1alpha1, so every command worked until one
// reached for a Secret — recording an approval reads the Fides credentials —
// and then failed with "no kind is registered for the type v1.Secret". Nothing
// failed at startup and nothing failed to compile, because the scheme is
// assembled at runtime and the Ops methods that read Secrets are several calls
// away from the code that builds it.
//
// Asserting on Secret specifically rather than "the scheme is non-empty": the
// bug was a scheme that had entries, just not that one.
func TestClientSchemeKnowsSecrets(t *testing.T) {
	s, err := clientScheme()
	if err != nil {
		t.Fatalf("clientScheme: %v", err)
	}

	if !s.Recognizes(corev1.SchemeGroupVersion.WithKind("Secret")) {
		t.Error("the scheme does not recognise v1.Secret — `hecate approve` " +
			"against a Gate with evidence.credentialsRef will fail when it " +
			"reads the Fides credentials")
	}

	// The Hecate kinds must still be there. A fix that swapped one omission for
	// another would otherwise pass.
	for _, kind := range []string{"Gate", "Bundle", "Passage", "Beacon"} {
		if !s.Recognizes(v1alpha1.GroupVersion.WithKind(kind)) {
			t.Errorf("the scheme does not recognise %s", kind)
		}
	}
}
