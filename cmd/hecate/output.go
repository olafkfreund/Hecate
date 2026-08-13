package main

import (
	"encoding/json"
	"flag"
	"os"

	"sigs.k8s.io/yaml"
)

// Output formats. Table is for a person, the other two are for a pipeline.
const (
	formatTable = "table"
	formatJSON  = "json"
	formatYAML  = "yaml"
)

// outputFlag registers -o/--output on a command.
//
// One helper rather than each command growing its own flag, because "every read
// command, no exceptions" is the requirement — and the way that promise breaks
// is a command added later whose author did not know it was a promise.
func outputFlag(fs *flag.FlagSet) *string {
	out := fs.String("output", formatTable, "output format: table, json or yaml")
	fs.StringVar(out, "o", formatTable, "shorthand for -output")
	return out
}

// render writes v in the requested format, falling back to the caller's table
// renderer for human output.
//
// The structured formats serialise the same value the table is built from, so
// `-o json` cannot drift into showing something the table does not — the way it
// would if each command formatted twice from different sources.
func render(format string, v any, table func() int) int {
	switch format {
	case formatTable, "":
		return table()
	case formatJSON:
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(v); err != nil {
			return fail(exitError, "writing JSON: %s", err)
		}
		return exitOK
	case formatYAML:
		// Via sigs.k8s.io/yaml rather than a YAML library directly: it routes
		// through the json tags, so the field names match the JSON output and
		// the API's own. A second set of names for the same data is a second
		// thing to learn and a second thing to get wrong.
		encoded, err := yaml.Marshal(v)
		if err != nil {
			return fail(exitError, "writing YAML: %s", err)
		}
		if _, err := os.Stdout.Write(encoded); err != nil {
			return fail(exitError, "writing YAML: %s", err)
		}
		return exitOK
	default:
		return fail(exitUsage,
			"unknown output format %q — expected table, json or yaml", format)
	}
}

// verifyReport is the structured form of `hecate verify`.
//
// A struct rather than the internal result type, because that one carries a
// *fides.Chain and an error, neither of which serialises into anything an
// auditor or a script can read.
type verifyReport struct {
	Gate    string `json:"gate,omitempty"`
	Passage string `json:"passage,omitempty"`
	Trail   string `json:"trail"`
	// Valid is the answer. Absent when the check could not be made at all,
	// which is not the same as a chain that failed.
	Valid *bool `json:"valid,omitempty"`
	// Attestations is how many entries the chain holds.
	Attestations int `json:"attestations,omitempty"`
	// BrokenAt is the first bad entry, when there is one.
	BrokenAt *int `json:"brokenAt,omitempty"`
	// Reason says what was wrong, or why the check could not be made.
	Reason string `json:"reason,omitempty"`
	// Anchored reports whether an external authority timestamped the chain
	// head. A valid chain only we vouch for is a weaker claim than one an
	// RFC3161 authority saw.
	Anchored *bool `json:"anchored,omitempty"`
}

func reports(results ...result) []verifyReport {
	out := make([]verifyReport, 0, len(results))
	for _, r := range results {
		rep := verifyReport{Gate: r.Gate, Passage: r.Passage, Trail: r.Trail}
		switch {
		case r.err != nil:
			rep.Reason = r.err.Error()
		case r.chain != nil:
			valid := r.chain.Valid
			rep.Valid = &valid
			rep.Attestations = r.chain.Count
			rep.Reason = reason(r.chain)
			if !valid {
				broken := r.chain.BrokenAt
				rep.BrokenAt = &broken
			}
			if a := r.chain.ExternalAnchor; a != nil {
				anchored := a.Anchored && a.HeadMatches
				rep.Anchored = &anchored
			}
		}
		out = append(out, rep)
	}
	return out
}
