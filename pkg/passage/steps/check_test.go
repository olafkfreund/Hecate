package steps

import (
	"encoding/json"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/passage"
)

// all is every step Hecate ships, which is what the controller registers.
func all() []passage.Runner {
	return []passage.Runner{
		NewFluxWait(nil),
		NewFluxReconcile(nil, false),
		NewGitClone(nil),
		NewGitCommit(),
		NewGitPush(nil),
		NewGitPullRequest(nil),
		NewEditYAML(),
		NewSetImage(),
		NewRenderKustomize(),
		NewHTTP(nil),
		NewEvidenceGate(nil, ""),
	}
}

// The value of checking configuration is that it is uniform: a Gate author
// should not have to know which steps happen to be checked. A step added later
// without a checker would be silently exempt, and nobody would notice until a
// production crossing failed on a typo.
func TestEveryStepChecksItsConfig(t *testing.T) {
	for _, r := range all() {
		if _, ok := r.(passage.ConfigChecker); !ok {
			t.Errorf("%s does not implement ConfigChecker — add it to check.go", r.Name())
		}
	}
}

// The misspelt field is the case #97 is about. Silently dropping it is what
// made the eventual failure point at the wrong field.
func TestAMisspeltFieldIsRefusedAndNamed(t *testing.T) {
	for _, tc := range []struct {
		runner passage.Runner
		raw    string
	}{
		{NewGitCommit(), `{"mesage":"promote"}`},
		{NewGitClone(nil), `{"repo":"https://example.test/x.git","brunch":"main"}`},
		{NewSetImage(), `{"path":"repo/k.yaml","image":"a/b","tagg":"1.0"}`},
		{NewHTTP(nil), `{"url":"https://x.test","sucessIf":"status == 200"}`},
	} {
		checker, ok := tc.runner.(passage.ConfigChecker)
		if !ok {
			t.Fatalf("%s implements no checker", tc.runner.Name())
		}
		err := checker.CheckConfig(json.RawMessage(tc.raw))
		if err == nil {
			t.Errorf("%s accepted %s", tc.runner.Name(), tc.raw)
			continue
		}
		// The message must name the field that is wrong, or the author is left
		// hunting through a step that looks correct.
		if !strings.Contains(err.Error(), "unknown field") {
			t.Errorf("%s: %v — the error should name the unknown field", tc.runner.Name(), err)
		}
	}
}

func TestCheckersAcceptValidConfiguration(t *testing.T) {
	for _, tc := range []struct {
		runner passage.Runner
		raw    string
	}{
		{NewGitClone(nil), `{"repo":"https://example.test/fleet.git","credentialsRef":{"name":"git"}}`},
		{NewGitCommit(), `{"message":"promote"}`},
		{NewGitPush(nil), `{"toNewBranch":true}`},
		{NewGitPush(nil), ``},
		{NewSetImage(), `{"path":"repo/k.yaml","image":"ghcr.io/acme/api"}`},
		{NewEditYAML(), `{"path":"repo/v.yaml","edits":[{"key":"image.tag","value":"1.0"}]}`},
		{NewRenderKustomize(), `{"path":"repo/base","out":"repo/rendered.yaml"}`},
		{NewFluxWait(nil), `{"resources":[{"kind":"Kustomization","name":"app"}]}`},
		{NewFluxReconcile(nil, false), `{"resources":[{"kind":"GitRepository","name":"fleet"}]}`},
		{NewHTTP(nil), `{"url":"https://x.test","successIf":"status == 200"}`},
		{NewEvidenceGate(nil, ""), `{"gates":["assert","change"],"maxRisk":40}`},
		{NewGitPullRequest(nil), `{"credentialsRef":{"name":"forge"},"provider":"gitlab"}`},
	} {
		checker, ok := tc.runner.(passage.ConfigChecker)
		if !ok {
			t.Fatalf("%s implements no checker", tc.runner.Name())
		}
		if err := checker.CheckConfig(json.RawMessage(tc.raw)); err != nil {
			t.Errorf("%s rejected valid config %s: %v", tc.runner.Name(), tc.raw, err)
		}
	}
}

func TestCheckersCatchMissingAndWrongValues(t *testing.T) {
	for _, tc := range []struct {
		name   string
		runner passage.Runner
		raw    string
		says   string
	}{
		{"clone without a repo", NewGitClone(nil), `{}`, "repo is required"},
		{"commit without a message", NewGitCommit(), `{"message":"  "}`, "message is required"},
		{"set-image without an image", NewSetImage(), `{"path":"k.yaml"}`, "image is required"},
		{"render without an out", NewRenderKustomize(), `{"path":"repo/base"}`, "out is required"},
		{"edit-yaml with no edits", NewEditYAML(), `{"path":"v.yaml","edits":[]}`, "no edits"},
		{"edit-yaml with a keyless edit", NewEditYAML(), `{"path":"v.yaml","edits":[{"value":"1"}]}`, "key is required"},
		{"flux-wait with no resources", NewFluxWait(nil), `{"resources":[]}`, "at least one resource"},
		{"flux-wait with an unknown kind", NewFluxWait(nil), `{"resources":[{"kind":"Sprocket","name":"x"}]}`, "apiVersion"},
		{"http with no url", NewHTTP(nil), `{}`, "url is required"},
		{"http with a scheme we will not call", NewHTTP(nil), `{"url":"file:///etc/passwd"}`, "must be http"},
		{"a header with no secret", NewHTTP(nil), `{"url":"https://x.test","secretHeaders":[{"name":"A"}]}`, "secretRef is required"},
		{"evidence-gate with no gates", NewEvidenceGate(nil, ""), `{"gates":[]}`, "checks nothing"},
		{"evidence-gate with an invented gate", NewEvidenceGate(nil, ""), `{"gates":["vibes"]}`, "no gate named"},
		{"a risk score off the scale", NewEvidenceGate(nil, ""), `{"gates":["change"],"maxRisk":140}`, "outside 0-100"},
		{"a pull request without a token", NewGitPullRequest(nil), `{}`, "credentialsRef is required"},
		{"a provider that does not exist", NewGitPullRequest(nil), `{"credentialsRef":{"name":"f"},"provider":"svn"}`, "not one of"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checker, ok := tc.runner.(passage.ConfigChecker)
			if !ok {
				t.Fatalf("%s implements no checker", tc.runner.Name())
			}
			err := checker.CheckConfig(json.RawMessage(tc.raw))
			if err == nil {
				t.Fatalf("%s accepted %s", tc.runner.Name(), tc.raw)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("error does not mention %q: %v", tc.says, err)
			}
		})
	}
}

// An expression cannot be judged before the Passage runs, and refusing one
// would ban the interpolation the engine exists to provide.
func TestAnExpressionIsNotAMalformedURL(t *testing.T) {
	var checker passage.ConfigChecker = NewHTTP(nil)
	if err := checker.CheckConfig(json.RawMessage(`{"url":"${{ vars.endpoint }}"}`)); err != nil {
		t.Errorf("an interpolated URL was refused: %v", err)
	}
}

// The registry's view: a whole step list, judged at once.
func TestRegistryValidatesAStepList(t *testing.T) {
	registry := passage.NewRegistry()
	for _, r := range all() {
		registry.MustRegister(r)
	}

	with := func(v string) *apiextensionsv1.JSON { return &apiextensionsv1.JSON{Raw: []byte(v)} }
	problems := registry.Validate([]v1alpha1.Step{
		{Uses: "git-clone", With: with(`{"repo":"https://example.test/x.git"}`)},
		{Uses: "git-commit", As: "commit", With: with(`{"mesage":"promote"}`)},
		{Uses: "teleport"},
		{Uses: "git-push", As: "commit"},
		{},
	})

	if len(problems) != 4 {
		t.Fatalf("got %d problems, want 4:\n%v", len(problems), problems)
	}
	// Every problem, not just the first: fixing a Gate one error per apply is
	// several wasted cycles.
	joined := ""
	for _, p := range problems {
		joined += p.Error() + "\n"
	}
	for _, want := range []string{
		`steps[1] (git-commit): invalid configuration: json: unknown field "mesage"`,
		"steps[2] (teleport): no such step",
		`steps[3] (git-push): alias "commit" is already used by steps[1]`,
		"steps[4]: no step named",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	// The available steps are listed, so the author does not have to guess.
	if !strings.Contains(joined, "git-clone") {
		t.Errorf("an unknown step should list what is available:\n%s", joined)
	}
}

func TestRegistryAcceptsAValidPassage(t *testing.T) {
	registry := passage.NewRegistry()
	for _, r := range all() {
		registry.MustRegister(r)
	}
	with := func(v string) *apiextensionsv1.JSON { return &apiextensionsv1.JSON{Raw: []byte(v)} }

	// The README's example.
	problems := registry.Validate([]v1alpha1.Step{
		{Uses: "git-clone", With: with(`{"repo":"https://github.com/acme/fleet.git"}`)},
		{Uses: "set-image", With: with(`{"path":"repo/apps/production/kustomization.yaml","image":"ghcr.io/acme/podinfo"}`)},
		{Uses: "git-commit", As: "commit", With: with(`{"message":"promote podinfo"}`)},
		{Uses: "git-push"},
		{Uses: "flux-wait", With: with(
			`{"resources":[{"kind":"Kustomization","name":"podinfo","namespace":"flux-system"}],` +
				`"expectedRevision":"${{ steps.commit.sha }}"}`)},
	})
	if len(problems) != 0 {
		t.Errorf("the documented example does not validate: %v", problems)
	}
}
