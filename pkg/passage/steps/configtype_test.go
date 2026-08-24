package steps

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/olafkfreund/hecate/pkg/passage"
)

// The same argument TestEveryStepChecksItsConfig makes. A step without a
// ConfigType is one a generated form cannot offer, and nothing would say so —
// the form would simply not have it, which looks like a step with no options
// rather than a gap.
func TestEveryStepDescribesItsConfig(t *testing.T) {
	for _, r := range all() {
		if _, ok := r.(passage.ConfigDescriber); !ok {
			t.Errorf("%s does not implement ConfigDescriber — add it to configtype.go", r.Name())
		}
	}
}

// A ConfigType that is not the struct CheckConfig decodes into would generate a
// schema for fields the step never reads, and every one would look valid until
// a crossing ran.
//
// Checked by offering the checker every field the type declares. Marshalling
// the zero value instead would prove nothing for the configs that are entirely
// `omitempty` — CommitStatusConfig serialises to `{}` — and a check that is
// vacuous for a third of the steps is one that reports coverage it does not
// have.
func TestConfigTypeIsWhatTheCheckerDecodes(t *testing.T) {
	for _, r := range all() {
		d, ok := r.(passage.ConfigDescriber)
		if !ok {
			continue // reported by TestEveryStepDescribesItsConfig
		}
		c, ok := r.(passage.ConfigChecker)
		if !ok {
			continue
		}

		fields := jsonFields(reflect.TypeOf(d.ConfigType()))
		if len(fields) == 0 {
			t.Errorf("%s: ConfigType %s declares no json fields",
				r.Name(), reflect.TypeOf(d.ConfigType()))
			continue
		}

		// Null for every field: accepted by any type, so what survives is the
		// question this asks — does the checker know the name?
		body := make(map[string]any, len(fields))
		for _, f := range fields {
			body[f] = nil
		}
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("%s: %s", r.Name(), err)
		}

		// A required-field complaint is expected and irrelevant. "unknown field"
		// is what a mismatched type looks like.
		if err := c.CheckConfig(raw); isUnknownField(err) {
			t.Errorf("%s: ConfigType is %s, whose fields its checker does not decode: %s",
				r.Name(), reflect.TypeOf(d.ConfigType()), err)
		}
	}
}

// jsonFields is the wire names a struct declares, embedded structs included.
func jsonFields(t reflect.Type) []string {
	if t.Kind() != reflect.Struct {
		return nil
	}
	var out []string
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			if f.Anonymous {
				out = append(out, jsonFields(f.Type)...)
				continue
			}
			name = f.Name
		}
		out = append(out, name)
	}
	return out
}

func isUnknownField(err error) bool {
	return err != nil && strings.Contains(err.Error(), "unknown field")
}
