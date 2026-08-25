package v1alpha1_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// The YAML we ship has to decode into the types we ship.
//
// `examples/` is what someone copies to start, and `demo/` is applied to the
// demo cluster — a field misspelled in either is found by whoever runs it,
// which is the wrong person and the wrong moment. Strict decoding catches it
// here: a `requiresApproval` that should be `requireApproval` is silently
// dropped by a lenient decoder and turns a gated pipeline into an open one.
func TestShippedManifestsDecode(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("building scheme: %v", err)
	}
	decoder := json.NewSerializerWithOptions(
		json.DefaultMetaFactory, scheme, scheme,
		json.SerializerOptions{Yaml: true, Strict: true},
	)

	for _, dir := range []string{"../../examples", "../../demo"} {
		paths, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
		if err != nil {
			t.Fatalf("globbing %s: %v", dir, err)
		}
		if len(paths) == 0 {
			t.Fatalf("%s has no manifests: the test would pass by finding nothing", dir)
		}
		for _, path := range paths {
			t.Run(filepath.Base(path), func(t *testing.T) {
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("reading: %v", err)
				}
				for i, doc := range strings.Split(string(raw), "\n---") {
					if strings.TrimSpace(stripComments(doc)) == "" {
						continue
					}
					obj, _, err := decoder.Decode([]byte(doc), nil, nil)
					if err != nil {
						// Flux types are not in Hecate's scheme, and demo/flux.yaml
						// is theirs. Not knowing a kind is not the same as a
						// malformed one of ours.
						if runtime.IsNotRegisteredError(err) {
							continue
						}
						t.Fatalf("document %d: %v", i, err)
					}
					if obj.GetObjectKind().GroupVersionKind().Empty() {
						t.Fatalf("document %d decoded to no kind", i)
					}
				}
			})
		}
	}
}

func stripComments(doc string) string {
	var out []string
	for _, line := range strings.Split(doc, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}
