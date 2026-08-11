// Package crds carries the CRDs this build expects the cluster to have, and
// checks at startup that it does.
//
// Helm installs a chart's crds/ directory once and never touches it again on
// upgrade. That is documented Helm behaviour rather than a bug, but it means
// `helm upgrade hecate` ships a new controller against the old API — and the
// API server *prunes* fields it does not recognise rather than rejecting them.
// The object stores without them, kubectl reports success, and the controller
// reads a zero value. Nothing anywhere says why (#117).
//
// So the controller refuses to run against an API older than itself. A failed
// rollout naming the one command that fixes it is worth a great deal more than
// a Beacon that has quietly stopped honouring one of its fields.
//
// The YAML here is a copy of charts/hecate/crds, made by `make generate`,
// because go:embed cannot reach outside its own package. `make check` fails if
// the two drift.
package crds

import (
	"context"
	"embed"
	"fmt"
	"sort"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//go:embed *.yaml
var files embed.FS

// The controller reads and compares CRDs; it never writes them. Whoever owns
// the cluster's CRDs keeps owning them — that is the whole premise of telling
// people to apply them with kubectl.
//
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list

// SkewError is a cluster whose CRDs are older than this build.
type SkewError struct {
	// Missing is what the cluster's CRDs do not declare, as dotted paths.
	Missing map[string][]string
}

func (e *SkewError) Error() string {
	var b strings.Builder
	b.WriteString("the cluster's CRDs are older than this build of Hecate.\n")
	b.WriteString("The API server silently drops fields it does not know, so Hecate would\n")
	b.WriteString("read zero values for these and give no indication why:\n")

	for _, name := range sortedKeys(e.Missing) {
		fmt.Fprintf(&b, "\n  %s\n", name)
		for _, path := range e.Missing[name] {
			fmt.Fprintf(&b, "    %s\n", path)
		}
	}

	b.WriteString("\nHelm does not upgrade CRDs. Apply them, then restart:\n")
	b.WriteString("\n  kubectl apply --server-side -f <release>/crds.yaml\n")
	b.WriteString("\nSee https://github.com/olafkfreund/Hecate/issues/117.\n")
	return b.String()
}

// MissingError is a CRD that is not installed at all.
type MissingError struct{ Names []string }

func (e *MissingError) Error() string {
	return fmt.Sprintf("these CRDs are not installed: %s.\n"+
		"Apply them with: kubectl apply --server-side -f <release>/crds.yaml",
		strings.Join(e.Names, ", "))
}

// Check reports whether the cluster's CRDs declare everything this build needs.
//
// One-directional on purpose: it asks whether the cluster is *missing* anything,
// not whether the two are identical. A cluster running CRDs newer than this
// binary is fine — the extra fields are simply unread — and failing on that
// would make every rollback an outage. It also means a cosmetic regeneration
// (a controller-gen bump rewording descriptions) cannot fail the check, which
// matters when the consequence is a controller that will not boot.
func Check(ctx context.Context, c client.Reader) error {
	want, err := Expected()
	if err != nil {
		return err
	}

	var absent []string
	missing := map[string][]string{}

	for _, name := range sortedKeys(want) {
		var installed apiextensionsv1.CustomResourceDefinition
		if err := c.Get(ctx, client.ObjectKey{Name: name}, &installed); err != nil {
			if apierrors.IsNotFound(err) {
				absent = append(absent, name)
				continue
			}
			return fmt.Errorf("reading CustomResourceDefinition %s: %w", name, err)
		}

		if gaps := missingPaths(want[name], pathsOf(&installed)); len(gaps) > 0 {
			missing[name] = gaps
		}
	}

	if len(absent) > 0 {
		return &MissingError{Names: absent}
	}
	if len(missing) > 0 {
		return &SkewError{Missing: missing}
	}
	return nil
}

// Expected is the set of property paths each shipped CRD declares.
func Expected() (map[string]map[string]bool, error) {
	entries, err := files.ReadDir(".")
	if err != nil {
		return nil, err
	}

	want := map[string]map[string]bool{}
	for _, entry := range entries {
		raw, err := files.ReadFile(entry.Name())
		if err != nil {
			return nil, err
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal(raw, &crd); err != nil {
			return nil, fmt.Errorf("parsing embedded %s: %w", entry.Name(), err)
		}
		want[crd.Name] = pathsOf(&crd)
	}
	if len(want) == 0 {
		// Would otherwise pass vacuously — the check would be dead code and
		// nothing would say so.
		return nil, fmt.Errorf("no CRDs are embedded in this build")
	}
	return want, nil
}

// pathsOf collects every property path in a CRD, as `version:spec.a.b`.
//
// Only leaf-bearing structure matters here: a field the API server does not
// declare is one it will prune, and its path is exactly what an operator needs
// to be told.
func pathsOf(crd *apiextensionsv1.CustomResourceDefinition) map[string]bool {
	paths := map[string]bool{}
	for _, version := range crd.Spec.Versions {
		if version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
			continue
		}
		walk(version.Name+":", version.Schema.OpenAPIV3Schema, paths)
	}
	return paths
}

// walk records the property paths under a schema node.
func walk(prefix string, schema *apiextensionsv1.JSONSchemaProps, into map[string]bool) {
	for name, prop := range schema.Properties {
		path := prefix + name
		into[path] = true
		walk(path+".", &prop, into)
	}
	// An array's items carry the fields, and `watch[].image.insecure` — the
	// field that prompted all this — lives under exactly this.
	if schema.Items != nil && schema.Items.Schema != nil {
		walk(prefix, schema.Items.Schema, into)
	}
}

func missingPaths(want, have map[string]bool) []string {
	var gaps []string
	for path := range want {
		if !have[path] {
			// The version prefix is noise to a reader who has one version.
			_, field, _ := strings.Cut(path, ":")
			gaps = append(gaps, field)
		}
	}
	sort.Strings(gaps)
	return gaps
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
