package steps

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/olafkfreund/hecate/pkg/passage"
)

// The committed schemas are only useful if they describe the steps that exist.
// `make generate` keeps them current and CI checks it, but that check lives a
// long way from someone adding a step — this one fails in the package they are
// already editing.
func TestEveryStepHasASchema(t *testing.T) {
	schemas, err := Schemas()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range all() {
		if _, ok := schemas[r.Name()]; !ok {
			t.Errorf("no schema for %s — run `make generate`", r.Name())
		}
	}
	if len(schemas) != len(all()) {
		t.Errorf("%d schemas for %d steps — a removed step leaves one behind; run `make generate`",
			len(schemas), len(all()))
	}
}

// A schema that has drifted from its struct offers fields the step does not
// read, and nothing fails when it does: the form simply shows the wrong thing.
func TestSchemaFieldsAreTheFieldsTheStepDecodes(t *testing.T) {
	schemas, err := Schemas()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range all() {
		raw, ok := schemas[r.Name()]
		if !ok {
			continue // reported by TestEveryStepHasASchema
		}
		var s struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Errorf("%s: %s", r.Name(), err)
			continue
		}

		d, ok := r.(passage.ConfigDescriber)
		if !ok {
			continue // reported by TestEveryStepDescribesItsConfig
		}
		want := map[string]bool{}
		for _, f := range jsonFields(reflect.TypeOf(d.ConfigType())) {
			want[f] = true
		}

		for got := range s.Properties {
			if !want[got] {
				t.Errorf("%s: schema offers %q, which its config struct does not declare — run `make generate`",
					r.Name(), got)
			}
		}
		for f := range want {
			if _, ok := s.Properties[f]; !ok {
				t.Errorf("%s: config declares %q, missing from the schema — run `make generate`",
					r.Name(), f)
			}
		}
	}
}
