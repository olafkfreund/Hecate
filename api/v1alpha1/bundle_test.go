package v1alpha1

import "testing"

func img(repo, tag, digest string) Artifact {
	return Artifact{Image: &ImageArtifact{Repo: repo, Tag: tag, Digest: digest}}
}

func TestComputeDigestIsOrderIndependent(t *testing.T) {
	// The same release discovered in a different order must be the same Bundle,
	// otherwise it gets promoted twice.
	a := ComputeDigest([]Artifact{
		img("ghcr.io/acme/api", "1.2.3", "sha256:aaa"),
		img("ghcr.io/acme/web", "4.5.6", "sha256:bbb"),
	})
	b := ComputeDigest([]Artifact{
		img("ghcr.io/acme/web", "4.5.6", "sha256:bbb"),
		img("ghcr.io/acme/api", "1.2.3", "sha256:aaa"),
	})
	if a != b {
		t.Fatalf("digest depends on artifact order:\n  %s\n  %s", a, b)
	}
}

func TestComputeDigestPrefersDigestOverTag(t *testing.T) {
	// A tag that has been re-pointed at the same image is not a new Bundle.
	pinned := ComputeDigest([]Artifact{img("ghcr.io/acme/api", "1.2.3", "sha256:aaa")})
	retagged := ComputeDigest([]Artifact{img("ghcr.io/acme/api", "stable", "sha256:aaa")})
	if pinned != retagged {
		t.Error("same image digest under a different tag produced a different Bundle digest")
	}

	// But a genuinely different image is a different Bundle.
	other := ComputeDigest([]Artifact{img("ghcr.io/acme/api", "1.2.3", "sha256:ccc")})
	if pinned == other {
		t.Error("different image digests produced the same Bundle digest")
	}
}

func TestComputeDigestDistinguishesArtifactKinds(t *testing.T) {
	image := ComputeDigest([]Artifact{img("acme/thing", "1.0.0", "")})
	chart := ComputeDigest([]Artifact{{Chart: &ChartArtifact{Repo: "acme/thing", Version: "1.0.0"}}})
	if image == chart {
		t.Error("an image and a chart with the same coordinates must not collide")
	}
}

func TestBundleName(t *testing.T) {
	got := BundleName("podinfo", "9f8c1a2b3c4d5e6f7a8b")
	if want := "podinfo-9f8c1a2b3c4d"; got != want {
		t.Errorf("BundleName = %q, want %q", got, want)
	}
	// Short digests must not panic on truncation.
	if got := BundleName("podinfo", "abc"); got != "podinfo-abc" {
		t.Errorf("BundleName with short digest = %q", got)
	}
}

func TestHealthMerge(t *testing.T) {
	// A set is only as healthy as its unhealthiest member.
	tests := []struct{ a, b, want Health }{
		{HealthHealthy, HealthHealthy, HealthHealthy},
		{HealthHealthy, HealthNotApplicable, HealthNotApplicable},
		{HealthHealthy, HealthProgressing, HealthProgressing},
		{HealthProgressing, HealthDegraded, HealthDegraded},
		{HealthUnknown, HealthProgressing, HealthUnknown},
		{HealthDegraded, HealthUnknown, HealthDegraded},
	}
	for _, tt := range tests {
		if got := tt.a.Merge(tt.b); got != tt.want {
			t.Errorf("%s.Merge(%s) = %s, want %s", tt.a, tt.b, got, tt.want)
		}
		if got := tt.b.Merge(tt.a); got != tt.want {
			t.Errorf("Merge must be commutative: %s.Merge(%s) = %s, want %s", tt.b, tt.a, got, tt.want)
		}
	}
}

func TestPhaseTerminal(t *testing.T) {
	for _, p := range []PassagePhase{PassageSucceeded, PassageFailed, PassageAborted} {
		if !p.Terminal() {
			t.Errorf("%s should be terminal", p)
		}
	}
	for _, p := range []PassagePhase{PassagePending, PassageRunning} {
		if p.Terminal() {
			t.Errorf("%s should not be terminal", p)
		}
	}
	if !StepSkipped.Terminal() {
		t.Error("StepSkipped should be terminal")
	}
	if StepRunning.Terminal() {
		t.Error("StepRunning should not be terminal")
	}
}

func TestBundleClearedAndApproved(t *testing.T) {
	b := &Bundle{Status: BundleStatus{
		Cleared:     []GateCrossing{{Gate: "dev"}, {Gate: "staging"}},
		ApprovedFor: []string{"production"},
	}}
	if !b.HasCleared("staging") {
		t.Error("staging should be cleared")
	}
	if b.HasCleared("production") {
		t.Error("production should not be cleared")
	}
	if !b.IsApprovedFor("production") {
		t.Error("production should be approved")
	}
	if b.IsApprovedFor("staging") {
		t.Error("staging should not be approved")
	}
}
