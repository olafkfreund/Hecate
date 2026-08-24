// Command stepschema writes a JSON Schema for every step's `with:` block, so a
// form can be generated from the same struct the step validates against.
//
// **Generated, not hand-written** (#114). Every step already has a Go config
// struct with json tags, and CheckConfig already decodes strictly into it. A
// schema written beside that struct would be a second description of one thing,
// free to disagree — and it would disagree silently, because nothing fails when
// a form offers a field the step does not read.
//
// **Generated at build time, not served from reflection.** The field
// descriptions are the Go doc comments, and reading those needs the source,
// which a container does not have. Committing the output also means the
// existing "Generated files are current" check catches a struct that changed
// without the schema following — the same bargain the CRDs already make.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"

	"github.com/invopop/jsonschema"

	"github.com/olafkfreund/hecate/pkg/passage"
	"github.com/olafkfreund/hecate/pkg/passage/steps"
)

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(out *os.File) error {
	r := &jsonschema.Reflector{
		// The form wants one self-contained schema per step, not a graph of
		// $refs into a shared $defs it would have to resolve itself.
		ExpandedStruct: true,
		DoNotReference: true,
		// A field absent from `with:` is one the step defaults, which is not the
		// same as a field that may not appear. Required comes from the checker's
		// own rules, which live in CheckConfig and are not expressible here.
		RequiredFromJSONSchemaTags: true,
	}

	// Doc comments are the descriptions. Without this the schema carries field
	// names and no help text, which is a form nobody can fill in.
	for _, pkg := range []string{"./pkg/passage/steps", "./pkg/health", "./api/v1alpha1"} {
		if err := r.AddGoComments("github.com/olafkfreund/hecate", pkg); err != nil {
			return fmt.Errorf("reading doc comments from %s: %w", pkg, err)
		}
	}

	schemas := map[string]*jsonschema.Schema{}
	var missing []string

	for _, runner := range steps.All(steps.Deps{}) {
		d, ok := runner.(passage.ConfigDescriber)
		if !ok {
			missing = append(missing, runner.Name())
			continue
		}
		schemas[runner.Name()] = r.ReflectFromType(reflect.TypeOf(d.ConfigType()))
	}

	// A step without a schema is a step a generated form cannot offer, and the
	// form would simply not have it — which looks like a step with no options
	// rather than a gap. Refuse rather than emit a partial file.
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("no ConfigType for %v — add it to pkg/passage/steps/configtype.go", missing)
	}

	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(schemas)
}
