// Command supportmatrix writes the git-host and registry support table into
// README.md, between the `support-matrix` markers.
//
// The table it replaces was hand-written and covered nothing: no row said
// which git hosts or registries Hecate actually talks to, let alone which of
// them CI has ever proven. A hand-written "supported" claim is worse than no
// claim — nobody can tell it apart from one that was checked (#8).
//
// **A job existing is not proof.** registry-matrix.yml's `dockerhub` job
// "succeeds" even when DOCKERHUB_TOKEN is unset, because its login step
// notices the secret is missing, prints a notice, and exits 0 rather than
// failing a fork's build for a credential it cannot have. The job conclusion
// is green either way; only the step that actually pushes and pulls tells you
// whether anything was proven. So this reads step conclusions, not job
// conclusions, from the most recent completed run on main.
//
// **Four states, not two.** "Supported" and "not supported" hide the case
// this whole exercise exists to catch: `pkg/provider/gitlab.go` implements
// the full Provider interface and nothing has ever run it against a real
// GitLab. That is not the same as GitHub, which runs nightly, and it is not
// the same as Bitbucket, which is not written at all. Rendering those three
// identically is exactly the false confidence the epic's exit criterion
// objects to. See the legend this command writes beneath the table.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
)

const (
	repoOwner = "olafkfreund"
	repoName  = "Hecate"
)

// surface is one row: a git host or a registry.
type surface struct {
	name string
	kind string // "git host" or "registry"
	// codePath, if non-empty, is a file whose existence means an
	// implementation exists. Empty means the row needs no dedicated code —
	// true of plain git hosts, and of every registry, which all speak the
	// same OCI distribution API through pkg/registry.
	codePath string
	// workflow/job/step identify the CI proof, read from the most recent
	// completed run of workflow on main. job matches a job name exactly;
	// step matches by substring, because two workflows spell the same idea
	// ("Push and pull" vs "E2E — GitHub pull request lifecycle")
	// differently. Empty workflow means no job proves this row at all.
	workflow, job, step string
	note                string
}

var surfaces = []surface{
	{
		name: "Gitea", kind: "git host",
		workflow: "e2e.yml", job: "Crossing on k3d", step: "E2E — deployed controller",
		note: "plain git over HTTPS — no provider code, see pkg/provider's own doc comment",
	},
	{
		name: "GitHub", kind: "git host", codePath: "pkg/provider/github.go",
		workflow: "e2e.yml", job: "Promotion against real GitHub", step: "E2E — GitHub pull request lifecycle",
		note: "nightly and on demand, never on push — a GitHub outage must not redden the build",
	},
	{
		name: "GitLab", kind: "git host", codePath: "pkg/provider/gitlab.go",
		note: "full Provider implementation, no e2e test — see #101",
	},
	{name: "Bitbucket", kind: "git host"},
	{name: "Azure DevOps", kind: "git host"},
	{
		name: "GHCR", kind: "registry",
		workflow: "registry-matrix.yml", job: "ghcr", step: "Push and pull",
	},
	{
		name: "ECR", kind: "registry",
		workflow: "registry-matrix.yml", job: "ecr", step: "Push and pull",
		note: "OIDC, no static keys — see the workflow's role-to-assume step",
	},
	{
		name: "Harbor", kind: "registry",
		workflow: "registry-matrix.yml", job: "self-hosted", step: "Push and pull",
		note: "stood in for by registry:2 with htpasswd — same distribution API, see #50",
	},
	{
		name: "Docker Hub", kind: "registry",
		workflow: "registry-matrix.yml", job: "dockerhub", step: "Push and pull",
	},
	{name: "GAR", kind: "registry"},
	{name: "ACR", kind: "registry"},
	{name: "Quay", kind: "registry"},
}

// state is what a surface's row renders as. Ordered worst to best so a
// mutation that regresses a row sorts as a visible downgrade in a diff.
type state int

const (
	stateNone state = iota
	stateCodeOnly
	stateGated
	stateProven
)

func (s state) String() string {
	switch s {
	case stateProven:
		return "✅ proven"
	case stateGated:
		return "🧪 configured, not yet proven"
	case stateCodeOnly:
		return "🔧 code only, no CI proof"
	default:
		return "❌ not implemented"
	}
}

// ghJob is the subset of a workflow run's job list this command reads.
type ghJob struct {
	Name  string `json:"name"`
	Steps []struct {
		Name       string `json:"name"`
		Conclusion string `json:"conclusion"`
	} `json:"steps"`
}

// latestRunJobs returns the jobs of the most recent completed run of
// workflow on branch main, keyed by job name. Empty workflow returns nil.
func latestRunJobs(workflow string) (map[string]ghJob, error) {
	if workflow == "" {
		return nil, nil
	}
	runs, err := ghGet(fmt.Sprintf(
		"https://api.github.com/repos/%s/%s/actions/workflows/%s/runs?branch=main&status=completed&per_page=1",
		repoOwner, repoName, workflow))
	if err != nil {
		return nil, err
	}
	var runList struct {
		WorkflowRuns []struct {
			ID int64 `json:"id"`
		} `json:"workflow_runs"`
	}
	if err := json.Unmarshal(runs, &runList); err != nil {
		return nil, fmt.Errorf("decoding %s runs: %w", workflow, err)
	}
	if len(runList.WorkflowRuns) == 0 {
		return nil, fmt.Errorf("%s: no completed run on main", workflow)
	}
	jobs, err := ghGet(fmt.Sprintf(
		"https://api.github.com/repos/%s/%s/actions/runs/%d/jobs",
		repoOwner, repoName, runList.WorkflowRuns[0].ID))
	if err != nil {
		return nil, err
	}
	var jobList struct {
		Jobs []ghJob `json:"jobs"`
	}
	if err := json.Unmarshal(jobs, &jobList); err != nil {
		return nil, fmt.Errorf("decoding %s jobs: %w", workflow, err)
	}
	byName := make(map[string]ghJob, len(jobList.Jobs))
	for _, j := range jobList.Jobs {
		byName[j.Name] = j
	}
	return byName, nil
}

func ghGet(url string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if tok := firstEnv("GITHUB_TOKEN", "GH_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	body := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		body = append(body, buf[:n]...)
		if rerr != nil {
			break
		}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s: %s", url, resp.Status, string(body))
	}
	return body, nil
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

// resolve turns a surface into its state, given the jobs of its workflow's
// latest run on main (nil if the surface names no workflow).
func resolve(s surface, jobs map[string]ghJob) (state, error) {
	hasCode := s.codePath != "" && fileExists(s.codePath)

	if s.workflow == "" {
		if hasCode {
			return stateCodeOnly, nil
		}
		return stateNone, nil
	}

	var job ghJob
	var ok bool
	for name, j := range jobs {
		if strings.Contains(name, s.job) {
			job, ok = j, true
			break
		}
	}
	if !ok {
		return stateNone, fmt.Errorf("%s: no job matching %q found in latest run of %s", s.name, s.job, s.workflow)
	}
	for _, step := range job.Steps {
		if strings.Contains(step.Name, s.step) {
			if step.Conclusion == "success" {
				return stateProven, nil
			}
			return stateGated, nil
		}
	}
	return stateNone, fmt.Errorf("%s: step matching %q not found in job %q", s.name, s.step, s.job)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

const (
	startMarker = "<!-- support-matrix:start -->"
	endMarker   = "<!-- support-matrix:end -->"
)

func render() (string, error) {
	byWorkflow := map[string]map[string]ghJob{}
	for _, s := range surfaces {
		if s.workflow == "" || byWorkflow[s.workflow] != nil {
			continue
		}
		jobs, err := latestRunJobs(s.workflow)
		if err != nil {
			return "", err
		}
		byWorkflow[s.workflow] = jobs
	}

	var b strings.Builder
	b.WriteString(startMarker + "\n")
	b.WriteString("<!-- generated by `go run ./cmd/supportmatrix` — do not hand-edit between the markers -->\n\n")

	for _, kind := range []string{"git host", "registry"} {
		heading := "Git hosts"
		if kind == "registry" {
			heading = "Registries"
		}
		fmt.Fprintf(&b, "**%s**\n\n", heading)
		b.WriteString("| | State | Notes |\n|---|---|---|\n")
		for _, s := range surfaces {
			if s.kind != kind {
				continue
			}
			st, err := resolve(s, byWorkflow[s.workflow])
			if err != nil {
				return "", err
			}
			note := s.note
			if note == "" {
				note = "—"
			}
			fmt.Fprintf(&b, "| **%s** | %s | %s |\n", s.name, st, note)
		}
		b.WriteString("\n")
	}

	b.WriteString("Legend:\n\n")
	b.WriteString("- " + stateProven.String() + " — a CI job exercises this against the real host or registry, " +
		"and the step that does the proving passed on the most recent run on `main`.\n")
	b.WriteString("- " + stateGated.String() + " — the job exists and runs, but the proving step was skipped " +
		"on the most recent run, almost always because an optional credential is not set on this repository.\n")
	b.WriteString("- " + stateCodeOnly.String() + " — an implementation exists in the tree but no CI job runs " +
		"it against the real thing. Not the same as *proven*: nothing has checked the code is right.\n")
	b.WriteString("- " + stateNone.String() + " — no code and no job.\n")
	b.WriteString(endMarker)
	return b.String(), nil
}

var markerBlock = regexp.MustCompile(`(?s)` + regexp.QuoteMeta(startMarker) + `.*` + regexp.QuoteMeta(endMarker))

func main() {
	table, err := render()
	if err != nil {
		fmt.Fprintln(os.Stderr, "supportmatrix:", err)
		os.Exit(1)
	}

	readme, err := os.ReadFile("README.md")
	if err != nil {
		fmt.Fprintln(os.Stderr, "supportmatrix:", err)
		os.Exit(1)
	}
	if !markerBlock.Match(readme) {
		fmt.Fprintf(os.Stderr, "supportmatrix: README.md has no %s ... %s block\n", startMarker, endMarker)
		os.Exit(1)
	}
	updated := markerBlock.ReplaceAll(readme, []byte(escapeReplacement(table)))
	if err := os.WriteFile("README.md", updated, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "supportmatrix:", err)
		os.Exit(1)
	}
}

// escapeReplacement guards against ReplaceAll interpreting a literal `$` in
// the generated table (there is none today, but a future note might) as a
// regexp backreference.
func escapeReplacement(s string) string {
	return strings.ReplaceAll(s, "$", "$$")
}
