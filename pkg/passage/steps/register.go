package steps

import (
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/pkg/health"
	"github.com/olafkfreund/hecate/pkg/passage"
)

// Deps is what the steps need from whoever wires them up.
//
// A struct rather than four arguments because the list only grows, and a call
// site with four positional values of two types is one a new step quietly
// breaks.
type Deps struct {
	Client client.Client
	// FluxChecker is shared with the health registry, so flux-wait waits on
	// exactly what the Gate goes on to watch.
	FluxChecker *health.FluxChecker
	// CrossNamespace allows steps to name resources in another namespace.
	CrossNamespace bool
	// FidesServer is the default evidence server for evidence-gate.
	FidesServer string
}

// All returns every step, in the one list there is.
//
// The controller registers from this and the tests enumerate from it. It exists
// because there used to be two lists — the controller's wiring and a
// hand-maintained one in the tests — and they drifted: commit-status was
// registered in production but absent from the test list, so
// TestEveryStepChecksItsConfig never asked whether it validated its config, and
// it did not. A misspelt field in a commit-status `with:` block was silently
// dropped, which is the exact bug class that test exists to prevent.
//
// So: add a step here and it is wired *and* checked. There is nowhere else to
// forget it.
func All(d Deps) []passage.Runner {
	return []passage.Runner{
		NewFluxWait(d.FluxChecker),
		NewFluxReconcile(d.Client, d.CrossNamespace),
		NewGitClone(d.Client),
		NewGitCommit(),
		NewGitPush(d.Client),
		NewGitPullRequest(d.Client),
		NewEditYAML(),
		NewSetImage(),
		NewRenderKustomize(),
		NewRenderHelm(),
		NewOCIPush(d.Client),
		NewOCIPull(d.Client),
		NewHTTP(d.Client),
		NewEvidenceGate(d.Client, d.FidesServer),
		NewCommitStatus(d.Client),
	}
}
