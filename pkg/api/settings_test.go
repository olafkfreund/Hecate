package api

import (
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

func TestUnreachableExplainsAnExpiredToken(t *testing.T) {
	// What a cluster answers when the kubeconfig's token has run out. The
	// client library renders this as "the server has asked for the client to
	// provide credentials", which reads as "there is no credential here" when
	// in fact there is one that has expired — the commonest way to get a
	// connected cluster wrong, and it fails a day after it is set up.
	unauthorised := apierrors.NewUnauthorized("Unauthorized")

	got := unreachableBecause(unauthorised)

	if !strings.Contains(got, "expired") {
		t.Errorf("a 401 is explained as %q, which does not mention expiry", got)
	}
	if strings.Contains(got, "asked for the client to provide credentials") {
		t.Error("a 401 still passes the client library's own wording through")
	}
}

func TestUnreachableKeepsAnUnfamiliarErrorIntact(t *testing.T) {
	// Guessing at a cause we have not recognised would replace a true message
	// with a plausible wrong one, which is worse than being unhelpful.
	got := unreachableBecause(errors.New("dial tcp 10.0.0.1:443: i/o timeout"))

	if got != "dial tcp 10.0.0.1:443: i/o timeout" {
		t.Errorf("an unrecognised error was rewritten to %q", got)
	}
}
