package main

import "testing"

// TestResolve pins the state machine that is the whole point of this
// command: a job existing is not proof, only the proving step's own
// conclusion is. Break this and the table starts calling a skipped Docker
// Hub push "proven".
func TestResolve(t *testing.T) {
	jobs := map[string]ghJob{
		"dockerhub": {
			Name: "dockerhub",
			Steps: []struct {
				Name       string `json:"name"`
				Conclusion string `json:"conclusion"`
			}{
				{Name: "Log in to Docker Hub", Conclusion: "success"},
				{Name: "Push and pull", Conclusion: "skipped"},
			},
		},
		"ghcr": {
			Name: "ghcr",
			Steps: []struct {
				Name       string `json:"name"`
				Conclusion string `json:"conclusion"`
			}{
				{Name: "Push and pull", Conclusion: "success"},
			},
		},
	}

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
			got, err := resolve(c.s, jobs)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got != c.want {
				t.Errorf("resolve(%+v) = %v, want %v", c.s, got, c.want)
			}
		})
	}
}

// TestResolveMissingJobErrors makes sure a surface naming a job that isn't in
// the latest run fails loudly rather than silently rendering "not
// implemented" for something that is, in fact, implemented.
func TestResolveMissingJobErrors(t *testing.T) {
	_, err := resolve(surface{name: "GAR", workflow: "registry-matrix.yml", job: "gar", step: "Push and pull"}, map[string]ghJob{})
	if err == nil {
		t.Fatal("resolve: want an error for a job absent from the latest run, got nil")
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
