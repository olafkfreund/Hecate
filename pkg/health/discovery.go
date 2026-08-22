package health

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// APIGroupLister is the slice of Kubernetes discovery this check needs.
//
// An interface rather than *discovery.DiscoveryClient so the check is testable
// without a cluster — and because one method is all it uses.
type APIGroupLister interface {
	ServerGroups() (*metav1.APIGroupList, error)
}

// CheckFluxAPIs compares the Flux API versions Hecate defaults to against what
// the cluster actually serves, and returns a warning for each mismatch.
//
// This is the mitigation D4 was missing. Reading Flux as unstructured buys
// version tolerance at the cost of a compile-time signal: an API version we no
// longer find cannot break the build, so it breaks at reconcile time instead,
// as a confusing "no matches for kind" against a Gate that used to work. Flux
// does remove API versions — image and notification v1beta2 reached EOL in
// v2.9 — so this is a real path, not a hypothetical one.
//
// **It warns; it never refuses to start.** A controller that will not run
// because Flux is newer than it expected converts a degraded watch into an
// outage, which is a worse failure than the one being reported. Everything
// Hecate does other than the affected kind keeps working, and an explicit
// `apiVersion` on the watch is a fix the operator can apply without waiting for
// a release.
func CheckFluxAPIs(d APIGroupLister) ([]string, error) {
	groups, err := d.ServerGroups()
	if err != nil {
		return nil, fmt.Errorf("listing the cluster's API groups: %w", err)
	}

	served := map[string][]string{}
	for _, g := range groups.Groups {
		for _, v := range g.Versions {
			served[g.Name] = append(served[g.Name], v.Version)
		}
	}

	var warnings []string
	for _, gv := range expectedGroupVersions() {
		group, version, _ := strings.Cut(gv, "/")
		versions, present := served[group]
		switch {
		case !present:
			warnings = append(warnings, fmt.Sprintf(
				"%s is not served by this cluster: the Flux component that provides %s "+
					"is not installed, and a Gate watching one will report Unknown",
				group, strings.Join(kindsFor(gv), ", ")))
		case !slices.Contains(versions, version):
			warnings = append(warnings, fmt.Sprintf(
				"%s serves %s but Hecate defaults to %s for %s: set apiVersion explicitly "+
					"on those watches, and expect this Flux to need a newer Hecate",
				group, strings.Join(versions, ", "), version, strings.Join(kindsFor(gv), ", ")))
		}
	}
	return warnings, nil
}

// expectedGroupVersions is the distinct set behind fluxAPIVersions, sorted so
// the warnings come out in a stable order rather than a map's.
func expectedGroupVersions() []string {
	seen := map[string]bool{}
	for _, gv := range fluxAPIVersions {
		seen[gv] = true
	}
	out := make([]string, 0, len(seen))
	for gv := range seen {
		out = append(out, gv)
	}
	sort.Strings(out)
	return out
}

// kindsFor names the kinds a group version covers, so a warning says what will
// actually stop working rather than only naming an API group.
func kindsFor(gv string) []string {
	var kinds []string
	for kind, v := range fluxAPIVersions {
		if v == gv {
			kinds = append(kinds, kind)
		}
	}
	sort.Strings(kinds)
	return kinds
}
