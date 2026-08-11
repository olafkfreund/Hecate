package crds

import (
	"context"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// shipped parses one of the embedded CRDs, which is what a correctly upgraded
// cluster would have.
func shipped(t *testing.T, file string) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	raw, err := files.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	return &crd
}

func all(t *testing.T) []client.Object {
	t.Helper()
	entries, err := files.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var objs []client.Object
	for _, e := range entries {
		objs = append(objs, shipped(t, e.Name()))
	}
	return objs
}

func checkAgainst(t *testing.T, objs ...client.Object) error {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return Check(context.Background(), c)
}

// The baseline. If this fails, every other test here is meaningless — the check
// would be rejecting a cluster that is exactly right.
func TestAMatchingClusterPasses(t *testing.T) {
	if err := checkAgainst(t, all(t)...); err != nil {
		t.Fatalf("a cluster with exactly the shipped CRDs was rejected:\n%v", err)
	}
}

func TestUninstalledCRDsAreReported(t *testing.T) {
	err := checkAgainst(t)
	if err == nil {
		t.Fatal("a cluster with no Hecate CRDs at all passed")
	}
	var missing *MissingError
	if !asType(err, &missing) {
		t.Fatalf("err = %T, want *MissingError: %v", err, err)
	}
	if len(missing.Names) != 4 {
		t.Errorf("reported %v, want all four", missing.Names)
	}
}

// The failure this whole package exists for, reproduced exactly: #109 added
// watch[].image.insecure, Helm did not upgrade the CRD, the API server pruned
// the field, and the Beacon went on trying HTTPS against a plain-HTTP registry
// with nothing logged.
//
// Note the path is under an array — `watch` is a list — which is the case a
// naive schema walk misses.
func TestAPrunedFieldIsCaught(t *testing.T) {
	objs := all(t)

	var beacons *apiextensionsv1.CustomResourceDefinition
	for _, o := range objs {
		if o.GetName() == "beacons.hecate.dev" {
			beacons = o.(*apiextensionsv1.CustomResourceDefinition)
		}
	}
	if beacons == nil {
		t.Fatal("no Beacon CRD is embedded")
	}

	// Roll the cluster's copy back to before insecure existed.
	image := beacons.Spec.Versions[0].Schema.OpenAPIV3Schema.
		Properties["spec"].Properties["watch"].Items.Schema.Properties["image"]
	if _, ok := image.Properties["insecure"]; !ok {
		t.Fatal("the shipped Beacon CRD has no watch[].image.insecure to remove")
	}
	delete(image.Properties, "insecure")

	err := checkAgainst(t, objs...)
	if err == nil {
		t.Fatal("a cluster missing watch[].image.insecure passed the check")
	}

	var skew *SkewError
	if !asType(err, &skew) {
		t.Fatalf("err = %T, want *SkewError", err)
	}
	// The operator has to be told which field, or the message is just "something
	// is old" and they are back to guessing.
	if !strings.Contains(err.Error(), "spec.watch.image.insecure") {
		t.Errorf("the message does not name the missing field:\n%s", err)
	}
	// And what to do about it.
	if !strings.Contains(err.Error(), "kubectl apply --server-side") {
		t.Errorf("the message does not say how to fix it:\n%s", err)
	}
}

// A cluster whose CRDs are *newer* than the binary must start. Otherwise every
// rollback becomes an outage, and rolling back is exactly what someone does
// when an upgrade went wrong.
func TestANewerClusterIsAllowed(t *testing.T) {
	objs := all(t)
	for _, o := range objs {
		crd := o.(*apiextensionsv1.CustomResourceDefinition)
		spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
		spec.Properties["fieldFromTheFuture"] = apiextensionsv1.JSONSchemaProps{Type: "string"}
	}

	if err := checkAgainst(t, objs...); err != nil {
		t.Fatalf("a cluster ahead of this build was rejected — a rollback would not boot:\n%v", err)
	}
}

// The embedded copies must be the ones the chart ships. `make check` enforces
// this too, but a Go test fails in `make test` where a contributor will see it.
func TestTheEmbeddedCRDsAreTheOnesWeShip(t *testing.T) {
	want, err := Expected()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"beacons.hecate.dev", "bundles.hecate.dev", "gates.hecate.dev", "passages.hecate.dev",
	} {
		if len(want[name]) == 0 {
			t.Errorf("%s is not embedded, or declares no fields", name)
		}
	}
}

func asType[T error](err error, target *T) bool {
	t, ok := err.(T)
	if ok {
		*target = t
	}
	return ok
}
