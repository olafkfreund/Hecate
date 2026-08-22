// Command apishape writes the field names the API actually sends, so the
// browser's copy of those types can be checked against them.
//
// ui/lib/api.ts mirrors these Go types by hand, and says so: "Hand-written
// rather than generated, and therefore able to drift — the first version of
// this file invented `reason` and `remedy` where the Go type has `kind` and
// `fix`, and TypeScript was perfectly happy because nothing checks a response
// against a schema at runtime."
//
// That happened, and nothing stopped it happening again. This is the stopping.
//
// **Shape, not types.** The obvious fix is to generate the TypeScript, and it
// is the wrong one here: 27% of api.ts is explanatory comment, much of it about
// browser-side concerns the Go types have no reason to carry — how a static
// export reaches the API, why a field is rendered one way rather than another.
// Generation would delete all of it or demand it be written twice. What
// actually went wrong was a field name nobody checked, so checking field names
// is the whole fix, and the prose survives.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/api"
	"github.com/olafkfreund/hecate/pkg/fides"
	"github.com/olafkfreund/hecate/pkg/ops"
)

// mirrored maps the TypeScript interface name to the Go value it mirrors.
//
// Named explicitly rather than discovered, because "every exported struct in
// four packages" is not the surface the browser sees — it is every struct that
// happens to exist, and a check that drifts from what is actually served is one
// people learn to ignore.
var mirrored = map[string]any{
	// The CRDs, as the API serialises them.
	"GateOccupant":   v1alpha1.GateOccupant{},
	"HealthReport":   v1alpha1.HealthReport{},
	"Admission":      v1alpha1.Admission{},
	"Gate":           v1alpha1.Gate{},
	"ImageArtifact":  v1alpha1.ImageArtifact{},
	"Artifact":       v1alpha1.Artifact{},
	"GateCrossing":   v1alpha1.GateCrossing{},
	"BundleApproval": v1alpha1.BundleApproval{},
	"Bundle":         v1alpha1.Bundle{},
	"StepStatus":     v1alpha1.StepStatus{},
	"Passage":        v1alpha1.Passage{},
	"WatchSource":    v1alpha1.WatchSource{},
	"Beacon":         v1alpha1.Beacon{},
	"EvidenceRef":    v1alpha1.EvidenceRef{},

	// The operations layer, which is most of what the UI reads.
	"GateSummary":       ops.GateSummary{},
	"NamespaceOverview": ops.NamespaceOverview{},
	"Totals":            ops.Totals{},
	"Day":               ops.Day{},
	"Overview":          ops.Overview{},
	"Preflight":         ops.Preflight{},
	"FluxResource":      ops.FluxResource{},
	"AuditEntry":        ops.AuditEntry{},
	"Blocker":           ops.Blocker{},
	"Waiting":           ops.Waiting{},
	"Evidence":          ops.Evidence{},
	"Explanation":       ops.Explanation{},

	// Fides' answers, passed through.
	"Control":       fides.Control{},
	"ChangeVerdict": fides.ChangeVerdict{},
	"ChangeRequest": fides.ChangeRequest{},

	// The settings screen.
	"Settings": api.Settings{},
}

func main() {
	out := map[string][]string{}
	for name, v := range mirrored {
		out[name] = fields(reflect.TypeOf(v))
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintln(os.Stderr, "apishape:", err)
		os.Exit(1)
	}
}

// fields is the JSON names a type serialises to, one level deep.
//
// One level, because that is where the browser's copy can disagree: a nested
// object in TypeScript either names a Go type that is checked in its own right,
// or is an inline literal whose keys are this function's answer for that field.
// Going deeper would mean modelling TypeScript's structure in Go to compare it,
// which is a parser, and there is already a real one on the other side.
func fields(t reflect.Type) []string {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}

	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue // unexported: never serialised
		}
		tag := f.Tag.Get("json")
		name, opts, _ := strings.Cut(tag, ",")
		if name == "-" {
			continue // explicitly not serialised
		}
		// An embedded struct with no name of its own contributes its own
		// fields, which is what `json:",inline"` and bare embedding both mean.
		if name == "" && f.Anonymous {
			out = append(out, fields(f.Type)...)
			continue
		}
		if name == "" {
			name = f.Name // no tag: encoding/json uses the field name verbatim
		}
		_ = opts
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
