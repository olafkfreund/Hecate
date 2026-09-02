package main

import "testing"

func fakeJob(name string, steps ...[2]string) ghJob {
	j := ghJob{Name: name, Conclusion: "success"}
	for _, s := range steps {
		j.Steps = append(j.Steps, struct {
			Name       string `json:"name"`
			Conclusion string `json:"conclusion"`
		}{Name: s[0], Conclusion: s[1]})
	}
	return j
}

// fakeSkippedJob is what a push run lists for a job whose own `if:` was
// false: present, conclusion "skipped", no steps at all.
func fakeSkippedJob(name string) ghJob {
	return ghJob{Name: name, Conclusion: "skipped"}
}

// fakeIndex builds a workflowIndex whose run/job data is pre-populated, so
// resolve's search walks it with no network call. runsNewestFirst[i] is the
// jobs map for the i-th newest run.
func fakeIndex(runsNewestFirst ...map[string]ghJob) *workflowIndex {
	idx := &workflowIndex{jobsByRun: map[int64]map[string]ghJob{}}
	for i, jobs := range runsNewestFirst {
		id := int64(i)
		idx.runIDs = append(idx.runIDs, id)
		idx.jobsByRun[id] = jobs
	}
	return idx
}

// TestResolve pins the state machine that is the whole point of this
// command: a job existing is not proof, only the proving step's own
// conclusion is. Break this and the table starts calling a skipped Docker
// Hub push "proven".
func TestResolve(t *testing.T) {
	idx := fakeIndex(map[string]ghJob{
		"dockerhub": fakeJob("dockerhub", [2]string{"Log in to Docker Hub", "success"}, [2]string{"Push and pull", "skipped"}),
		"ghcr":      fakeJob("ghcr", [2]string{"Push and pull", "success"}),
	})

	cases := []struct {
		name string
		s    surface
		want state
	}{
		{
			name: "job succeeds but the proving step was skipped -> gated, not proven",
			s:    surface{name: "Docker Hub", workflow: "registry-matrix.yml", job: "dockerhub", step: "Push and pull"},
			want: stateGated,
		},
		{
			name: "the proving step itself passed -> proven",
			s:    surface{name: "GHCR", workflow: "registry-matrix.yml", job: "ghcr", step: "Push and pull"},
			want: stateProven,
		},
		{
			name: "no workflow, code exists -> code only",
			s:    surface{name: "GitLab", codePath: "main.go"},
			want: stateCodeOnly,
		},
		{
			name: "no workflow, no code -> not implemented",
			s:    surface{name: "Bitbucket"},
			want: stateNone,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolve(c.s, idx)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got != c.want {
				t.Errorf("resolve(%+v) = %v, want %v", c.s, got, c.want)
			}
		})
	}
}

// TestResolveWalksBackToTheRunThatExecuted is the regression test for the
// bug review found: e2e.yml's GitHub provider job only runs on
// schedule/workflow_dispatch. On an ordinary push run the job is not
// omitted — the API still lists it, with conclusion "skipped" and no
// steps. resolve must recognise that as "did not execute here" and walk
// back through older runs to find the one where the job actually ran,
// rather than reading only the latest run (or matching the empty-steps
// entry and erroring that the proving step is missing).
func TestResolveWalksBackToTheRunThatExecuted(t *testing.T) {
	idx := fakeIndex(
		map[string]ghJob{ // newest: a push run, job listed but job-level skipped
			"Crossing on k3d (Flux 2.9, k8s v1.36)": fakeJob("Crossing on k3d (Flux 2.9, k8s v1.36)", [2]string{"E2E — deployed controller", "success"}),
			"Promotion against real GitHub":         fakeSkippedJob("Promotion against real GitHub"),
		},
		map[string]ghJob{ // another push run, same story
			"Promotion against real GitHub": fakeSkippedJob("Promotion against real GitHub"),
		},
		map[string]ghJob{ // oldest in window: last night's schedule run, job actually ran and passed
			"Promotion against real GitHub": fakeJob("Promotion against real GitHub", [2]string{"E2E — GitHub pull request lifecycle", "success"}),
		},
	)

	got, err := resolve(surface{name: "GitHub", workflow: "e2e.yml", job: "Promotion against real GitHub", step: "E2E — GitHub pull request lifecycle"}, idx)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != stateProven {
		t.Errorf("resolve = %v, want %v (should have walked back past the job-level-skipped runs to the one where the job executed)", got, stateProven)
	}
}

// TestResolveJobAbsentFromWholeWindowIsGatedNotError is the other half of
// the same bug: when a gated job did not execute in ANY run in the window
// (nobody has exercised it lately), that renders as "not proven" — a
// legitimate table state — rather than the generator erroring out and
// refusing to write anything.
func TestResolveJobAbsentFromWholeWindowIsGatedNotError(t *testing.T) {
	idx := fakeIndex(map[string]ghJob{}, map[string]ghJob{})
	got, err := resolve(surface{name: "GitHub", workflow: "e2e.yml", job: "Promotion against real GitHub", step: "E2E — GitHub pull request lifecycle"}, idx)
	if err != nil {
		t.Fatalf("resolve: want no error for a job absent from the whole window, got %v", err)
	}
	if got != stateGated {
		t.Errorf("resolve = %v, want %v", got, stateGated)
	}
}

// Same outcome when the job is listed every time but always job-level
// skipped in the window — e.g. no nightly has landed recently.
func TestResolveJobSkippedInEveryRunIsGatedNotError(t *testing.T) {
	idx := fakeIndex(
		map[string]ghJob{"Promotion against real GitHub": fakeSkippedJob("Promotion against real GitHub")},
		map[string]ghJob{"Promotion against real GitHub": fakeSkippedJob("Promotion against real GitHub")},
	)
	got, err := resolve(surface{name: "GitHub", workflow: "e2e.yml", job: "Promotion against real GitHub", step: "E2E — GitHub pull request lifecycle"}, idx)
	if err != nil {
		t.Fatalf("resolve: want no error when the job is job-level skipped throughout the window, got %v", err)
	}
	if got != stateGated {
		t.Errorf("resolve = %v, want %v", got, stateGated)
	}
}

// TestResolveMissingStepErrors: the job ran, but the named step is nowhere
// in it — the workflow's step was likely renamed out from under this
// generator's hardcoded surfaces table. That must still fail loudly.
func TestResolveMissingStepErrors(t *testing.T) {
	idx := fakeIndex(map[string]ghJob{
		"gar": fakeJob("gar", [2]string{"Some other step", "success"}),
	})
	_, err := resolve(surface{name: "GAR", workflow: "registry-matrix.yml", job: "gar", step: "Push and pull"}, idx)
	if err == nil {
		t.Fatal("resolve: want an error when the job ran but the proving step is missing, got nil")
	}
}

func TestMarkerBlockReplace(t *testing.T) {
	readme := "before\n" + startMarker + "\nstale\n" + endMarker + "\nafter\n"
	got := markerBlock.ReplaceAllString(readme, escapeReplacement(startMarker+"\nfresh\n"+endMarker))
	want := "before\n" + startMarker + "\nfresh\n" + endMarker + "\nafter\n"
	if got != want {
		t.Errorf("marker replace = %q, want %q", got, want)
	}
}
