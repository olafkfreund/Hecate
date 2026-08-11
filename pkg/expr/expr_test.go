package expr

import (
	"encoding/json"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

func testEnv() Env {
	return Env{
		Bundle: &v1alpha1.Bundle{
			ObjectMeta: metav1.ObjectMeta{Name: "podinfo-9f8c1a2b"},
			Spec: v1alpha1.BundleSpec{
				Digest: "9f8c1a2b3c4d",
				Alias:  "wandering-owl",
				Artifacts: []v1alpha1.Artifact{
					{Image: &v1alpha1.ImageArtifact{
						Repo: "ghcr.io/acme/api", Tag: "1.2.3", Digest: "sha256:aaa",
					}},
					{Image: &v1alpha1.ImageArtifact{
						Repo: "ghcr.io/acme/web", Tag: "4.5.6", Digest: "sha256:bbb",
					}},
					{Chart: &v1alpha1.ChartArtifact{Repo: "oci://acme/charts", Version: "6.3.5"}},
					{Commit: &v1alpha1.CommitArtifact{Repo: "https://git/acme/app", SHA: "deadbeef"}},
				},
			},
		},
		Steps: map[string]map[string]any{
			"commit": {"sha": "9f8c1a2b", "count": 3},
		},
		Vars:      map[string]string{"region": "eu-west-1"},
		Gate:      "production",
		Namespace: "acme",
		Actor:     "olaf",
	}
}

func TestInterpolateReferences(t *testing.T) {
	env := testEnv()
	tests := map[string]any{
		"${{ steps.commit.sha }}":                          "9f8c1a2b",
		"${{ vars.region }}":                               "eu-west-1",
		"${{ gate }}":                                      "production",
		"${{ actor }}":                                     "olaf",
		"${{ bundle.digest }}":                             "9f8c1a2b3c4d",
		"${{ bundle.alias }}":                              "wandering-owl",
		`${{ bundle.image('ghcr.io/acme/api').tag }}`:      "1.2.3",
		`${{ bundle.image('ghcr.io/acme/web').tag }}`:      "4.5.6",
		`${{ bundle.chart('oci://acme/charts').version }}`: "6.3.5",
		`${{ bundle.commit('https://git/acme/app').sha }}`: "deadbeef",
		`${{ bundle.image('ghcr.io/acme/api').digest }}`:   "sha256:aaa",
	}
	for in, want := range tests {
		t.Run(in, func(t *testing.T) {
			got, err := Interpolate(in, env)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != want {
				t.Errorf("= %v, want %v", got, want)
			}
		})
	}
}

// Configuration is JSON: turning `${{ vars.replicas }}` into the string "3"
// would break a numeric field.
func TestWholeStringExpressionKeepsItsType(t *testing.T) {
	env := testEnv()

	got, err := Interpolate("${{ steps.commit.count }}", env)
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := got.(int); !ok || n != 3 {
		t.Errorf("= %#v, want the int 3 — a whole-string expression must keep its type", got)
	}

	// Embedded in text, the same value is stringified.
	got, err = Interpolate("count is ${{ steps.commit.count }}", env)
	if err != nil {
		t.Fatal(err)
	}
	if got != "count is 3" {
		t.Errorf("= %#v, want \"count is 3\"", got)
	}
}

func TestInterpolateEmbedded(t *testing.T) {
	env := testEnv()
	tests := map[string]string{
		"prod: ${{ steps.commit.sha }}":                                      "prod: 9f8c1a2b",
		"${{ gate }}/${{ vars.region }}":                                     "production/eu-west-1",
		`promote ${{ bundle.image('ghcr.io/acme/api').tag }} to ${{ gate }}`: "promote 1.2.3 to production",
		"no expressions here":                                                "no expressions here",
		"":                                                                   "",
	}
	for in, want := range tests {
		t.Run(in, func(t *testing.T) {
			got, err := Interpolate(in, env)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != want {
				t.Errorf("= %q, want %q", got, want)
			}
		})
	}
}

// A reference to an artifact the Bundle does not carry must fail loudly. The
// dangerous alternative is interpolating an empty string and promoting
// `image:` with no tag.
func TestMissingArtifactIsAnError(t *testing.T) {
	_, err := Interpolate(`${{ bundle.image('ghcr.io/acme/absent').tag }}`, testEnv())
	if err == nil {
		t.Fatal("expected an error for an artifact the Bundle does not carry")
	}
}

func TestErrorsAreExplained(t *testing.T) {
	env := testEnv()
	for name, code := range map[string]string{
		"syntax error":     "${{ steps. }}",
		"unknown variable": "${{ nonexistent.thing }}",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Interpolate(code, env)
			if err == nil {
				t.Fatal("expected an error")
			}
			// The message must name the expression, or a Passage with twenty
			// steps gives no clue which one is wrong.
			if !strings.Contains(err.Error(), "steps") && !strings.Contains(err.Error(), "nonexistent") {
				t.Errorf("error does not identify the expression: %v", err)
			}
		})
	}
}

func TestBool(t *testing.T) {
	env := testEnv()
	tests := map[string]bool{
		`gate == "production"`:                       true,
		`gate == "staging"`:                          false,
		`vars.region startsWith "eu"`:                true,
		`steps.commit.count > 2`:                     true,
		`bundle.image('ghcr.io/acme/api') != nil`:    true,
		`bundle.image('ghcr.io/acme/absent') == nil`: true,
	}
	for code, want := range tests {
		t.Run(code, func(t *testing.T) {
			got, err := Bool(code, env)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != want {
				t.Errorf("= %v, want %v", got, want)
			}
		})
	}
}

// `if: "1"` is a mistake. Guessing truthiness would run a step the author meant
// to gate.
func TestNonBooleanConditionIsAnError(t *testing.T) {
	for _, code := range []string{`"yes"`, `1`, `bundle.digest`} {
		if _, err := Bool(code, testEnv()); err == nil {
			t.Errorf("condition %q returned no error but is not a boolean", code)
		}
	}
}

func TestInterpolateJSON(t *testing.T) {
	env := testEnv()
	in := json.RawMessage(`{
		"path": "envs/${{ gate }}",
		"image": "${{ bundle.image('ghcr.io/acme/api').repo }}:${{ bundle.image('ghcr.io/acme/api').tag }}",
		"replicas": "${{ steps.commit.count }}",
		"nested": {"sha": "${{ steps.commit.sha }}"},
		"list": ["${{ gate }}", "static"],
		"untouched": 42,
		"flag": true
	}`)

	out, err := InterpolateJSON(in, env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}

	if got["path"] != "envs/production" {
		t.Errorf("path = %v", got["path"])
	}
	if got["image"] != "ghcr.io/acme/api:1.2.3" {
		t.Errorf("image = %v", got["image"])
	}
	// Stayed a number through the JSON round trip.
	if n, ok := got["replicas"].(float64); !ok || n != 3 {
		t.Errorf("replicas = %#v, want the number 3", got["replicas"])
	}
	if got["nested"].(map[string]any)["sha"] != "9f8c1a2b" {
		t.Errorf("nested = %v", got["nested"])
	}
	if got["list"].([]any)[0] != "production" || got["list"].([]any)[1] != "static" {
		t.Errorf("list = %v", got["list"])
	}
	if got["untouched"].(float64) != 42 || got["flag"] != true {
		t.Errorf("non-string values were altered: %v %v", got["untouched"], got["flag"])
	}
}

func TestInterpolateJSONEmptyAndInvalid(t *testing.T) {
	if out, err := InterpolateJSON(nil, testEnv()); err != nil || out != nil {
		t.Errorf("empty config should pass through: %v %v", out, err)
	}
	if _, err := InterpolateJSON(json.RawMessage(`{not json`), testEnv()); err == nil {
		t.Error("expected an error for malformed JSON")
	}
}

// An empty Env must not panic — a Passage's first step has no prior outputs,
// and a Gate need not declare vars.
func TestEmptyEnvIsUsable(t *testing.T) {
	got, err := Interpolate("${{ gate }}", Env{Gate: "dev"})
	if err != nil || got != "dev" {
		t.Fatalf("= %v, %v", got, err)
	}
	if _, err := Interpolate("${{ steps.absent.sha }}", Env{}); err == nil {
		t.Error("referencing an absent step should error, not return empty")
	}
}
