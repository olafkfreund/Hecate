package health

import (
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// stubDiscovery serves whatever group versions a test says the cluster has.
type stubDiscovery struct {
	groups map[string][]string
	err    error
}

func (s stubDiscovery) ServerGroups() (*metav1.APIGroupList, error) {
	if s.err != nil {
		return nil, s.err
	}
	list := &metav1.APIGroupList{}
	for name, versions := range s.groups {
		g := metav1.APIGroup{Name: name}
		for _, v := range versions {
			g.Versions = append(g.Versions, metav1.GroupVersionForDiscovery{Version: v})
		}
		list.Groups = append(list.Groups, g)
	}
	return list, nil
}

// A Flux that serves everything we expect is the case that must stay silent —
// a startup check that warns on a healthy cluster is a check people learn to
// ignore.
func TestFluxAPIsSaysNothingWhenEverythingMatches(t *testing.T) {
	warnings, err := CheckFluxAPIs(stubDiscovery{groups: map[string][]string{
		"kustomize.toolkit.fluxcd.io": {"v1"},
		"helm.toolkit.fluxcd.io":      {"v2"},
		"source.toolkit.fluxcd.io":    {"v1"},
		// Unrelated groups are none of our business.
		"apps": {"v1"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Errorf("warned about a healthy cluster: %v", warnings)
	}
}

// The case D4 could not otherwise catch: reading Flux as unstructured means an
// API version we no longer find cannot break the build, so without this it
// breaks at reconcile time as "no matches for kind" against a Gate that used to
// work.
func TestFluxAPIsWarnsWhenTheServedVersionDiffers(t *testing.T) {
	warnings, err := CheckFluxAPIs(stubDiscovery{groups: map[string][]string{
		"kustomize.toolkit.fluxcd.io": {"v1"},
		// An older Flux: HelmRelease v2 became stable in 2.3.
		"helm.toolkit.fluxcd.io":   {"v2beta1", "v2beta2"},
		"source.toolkit.fluxcd.io": {"v1"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one about HelmRelease", warnings)
	}
	w := warnings[0]
	for _, want := range []string{"helm.toolkit.fluxcd.io", "v2beta2", "v2", "HelmRelease", "apiVersion"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning does not mention %q — it has to say what broke and what to do: %s", want, w)
		}
	}
}

// Flux not installed at all is a different message from Flux being the wrong
// version, and conflating them sends the operator to the wrong place.
func TestFluxAPIsWarnsWhenAComponentIsAbsent(t *testing.T) {
	warnings, err := CheckFluxAPIs(stubDiscovery{groups: map[string][]string{
		"kustomize.toolkit.fluxcd.io": {"v1"},
		"source.toolkit.fluxcd.io":    {"v1"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want one about the missing helm group", warnings)
	}
	if !strings.Contains(warnings[0], "not installed") || !strings.Contains(warnings[0], "HelmRelease") {
		t.Errorf("warning = %q, want it to say the component is missing and which kind is affected",
			warnings[0])
	}
}

// Every kind Hecate can default is covered, so adding one to fluxAPIVersions
// without teaching this check about it is not possible.
func TestFluxAPIsCoversEveryDefaultedKind(t *testing.T) {
	warnings, err := CheckFluxAPIs(stubDiscovery{groups: map[string][]string{}})
	if err != nil {
		t.Fatal(err)
	}
	mentioned := strings.Join(warnings, " ")
	for kind := range fluxAPIVersions {
		if !strings.Contains(mentioned, kind) {
			t.Errorf("%s is defaulted but no warning mentions it: %v", kind, warnings)
		}
	}
}

// Discovery failing is not a reason to refuse to start, but the caller has to
// be able to tell "I checked and it is fine" from "I could not check".
func TestFluxAPIsReportsADiscoveryFailure(t *testing.T) {
	_, err := CheckFluxAPIs(stubDiscovery{err: errors.New("connection refused")})
	if err == nil {
		t.Fatal("want an error when discovery fails")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("err = %v, want the cause preserved", err)
	}
}
