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

func TestHomeClusterReportsWhereHecateRuns(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")

	got := homeCluster()

	if !got.InCluster {
		t.Error("running in a cluster was not reported as such")
	}
	// The service address, which is what this process actually dials — not the
	// public endpoint someone might expect. Honest beats familiar here.
	if got.Server != "https://10.0.0.1:443" {
		t.Errorf("server is %q, want https://10.0.0.1:443", got.Server)
	}
}

func TestHomeClusterSaysNothingWhenNotInACluster(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")

	got := homeCluster()

	// A developer running the API against their own kubeconfig. Inventing an
	// address would be worse than saying nothing, because the screen would then
	// claim a connection that is not the one being used.
	if got.InCluster || got.Server != "" {
		t.Errorf("got %+v, want an empty home cluster", got)
	}
}

func TestHomeClusterHandlesAnIPv6ServiceAddress(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "fd00::1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")

	got := homeCluster()

	// Bracketed, or the URL is unparseable. Concatenating host and port with a
	// colon is the obvious spelling and produces https://fd00::1:443.
	if got.Server != "https://[fd00::1]:443" {
		t.Errorf("server is %q, want https://[fd00::1]:443", got.Server)
	}
}

func TestDedupeSortsAndRemovesRepeats(t *testing.T) {
	// Written against the hand-rolled implementation before replacing it with
	// slices.Sort + slices.Compact, so "the replacement is equivalent" is a
	// thing the suite can answer rather than a thing I asserted.
	for _, tc := range []struct {
		name string
		in   []string
		want []string
	}{
		{"repeats collapse", []string{"b", "a", "b", "a"}, []string{"a", "b"}},
		{"already unique still sorts", []string{"c", "a", "b"}, []string{"a", "b", "c"}},
		{"single value", []string{"only"}, []string{"only"}},
		// Empty answers nil, which matters only because the field is omitempty
		// and both spellings serialise away — recorded so a change to it is a
		// decision rather than a surprise.
		{"empty", []string{}, nil},
		{"nil", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := dedupe(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("dedupe(%v) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("dedupe(%v) = %v, want %v", tc.in, got, tc.want)
				}
			}
		})
	}
}
