package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The reason #37 exists: if it cannot be scripted, it is not done. A structured
// format that always exited 0 would break exactly that, and silently — the JSON
// would still say the chain was invalid while `$?` said everything was fine.
func TestStructuredOutputStillExitsOnABrokenChain(t *testing.T) {
	broken := fidesServing(t, `{"valid":false,"count":2,"broken_at":1,"reason":"deletion"}`)
	ctx := context.Background()

	for _, format := range []string{formatTable, formatJSON, formatYAML} {
		t.Run(format, func(t *testing.T) {
			var code int
			out := capture(t, func() {
				code = renderVerify(format,
					one(ctx, broken, crossing{Gate: "production", Trail: "bbbb2222-0000-4000-8000-00000000bbbb"}))
			})
			if code != exitBroken {
				t.Errorf("exit = %d, want %d — a script checking $? would ship a tampered artifact",
					code, exitBroken)
			}
			if !strings.Contains(out, "production") {
				t.Errorf("%s output does not name the Gate:\n%s", format, out)
			}
		})
	}
}

func TestVerifyJSONCarriesTheVerdict(t *testing.T) {
	broken := fidesServing(t, `{"valid":false,"count":2,"broken_at":1,"reason":"deletion"}`)
	ctx := context.Background()

	out := capture(t, func() {
		renderVerify(formatJSON,
			one(ctx, broken, crossing{Gate: "production", Trail: "bbbb2222-0000-4000-8000-00000000bbbb"}))
	})

	var got []verifyReport
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got) != 1 {
		t.Fatalf("got %d reports", len(got))
	}
	r := got[0]
	if r.Valid == nil || *r.Valid {
		t.Errorf("valid = %v, want false", r.Valid)
	}
	// The entry number is what an auditor goes and looks at. Reporting only
	// "invalid" makes them read the whole chain by hand.
	if r.BrokenAt == nil || *r.BrokenAt != 1 {
		t.Errorf("brokenAt = %v, want 1", r.BrokenAt)
	}
	if r.Reason != "deletion" || r.Gate != "production" {
		t.Errorf("report = %+v", r)
	}
}

// YAML goes through the json tags, so the field names match the JSON output and
// the API's. A second set of names for the same data is a second thing to learn.
func TestYAMLUsesTheSameFieldNamesAsJSON(t *testing.T) {
	valid := fidesServing(t, `{"valid":true,"count":5,"broken_at":-1}`)
	ctx := context.Background()
	x := crossing{Gate: "staging", Trail: "cccc3333-0000-4000-8000-00000000cccc"}

	asYAML := capture(t, func() { renderVerify(formatYAML, one(ctx, valid, x)) })
	asJSON := capture(t, func() { renderVerify(formatJSON, one(ctx, valid, x)) })

	var fromYAML, fromJSON []verifyReport
	if err := yaml.Unmarshal([]byte(asYAML), &fromYAML); err != nil {
		t.Fatalf("output is not YAML: %v\n%s", err, asYAML)
	}
	if err := json.Unmarshal([]byte(asJSON), &fromJSON); err != nil {
		t.Fatal(err)
	}
	// Compared as marshalled bytes: verifyReport holds pointers, so == would
	// compare addresses and pass for any two decodes.
	a, _ := json.Marshal(fromYAML)
	b, _ := json.Marshal(fromJSON)
	if string(a) != string(b) {
		t.Errorf("yaml and json disagree:\n%s\n%s", a, b)
	}
	// camelCase from the json tag, not Go's field name.
	if !strings.Contains(asYAML, "attestations:") {
		t.Errorf("yaml does not use the json field names:\n%s", asYAML)
	}
}

func TestAnUnknownFormatIsRefused(t *testing.T) {
	var code int
	capture(t, func() {
		code = render("toml", []string{"x"}, func() int { return exitOK })
	})
	// Silently falling back to a table would make `-o toml` in a pipeline look
	// like it worked until something downstream tried to parse it.
	if code != exitUsage {
		t.Errorf("exit = %d, want %d", code, exitUsage)
	}
}

// The Passage name is why a script calls promote: it has to watch or verify
// the crossing afterwards. Parsing it back out of "crossing X through Y as Z"
// is the failure the CLI principle names — if it cannot be scripted, it is not
// done — so the write commands emit it too.
func TestWriteCommandsEmitSomethingAScriptCanRead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result actionResult
		want   string
	}{
		{"promote", actionResult{Action: "promote", Namespace: "acme", Gate: "staging",
			Bundle: "b1", Passage: "staging-b1-abc123", Actor: "olaf@acme.example"}, "staging-b1-abc123"},
		{"approve", actionResult{Action: "approve", Namespace: "acme", Gate: "production",
			Bundle: "b1", Actor: "olaf@acme.example"}, "production"},
		{"abort", actionResult{Action: "abort", Namespace: "acme",
			Passage: "staging-b1-abc123", Actor: "olaf@acme.example"}, "staging-b1-abc123"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := capture(t, func() {
				render(formatJSON, tc.result, func() int { return exitOK })
			})
			var got actionResult
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("not JSON: %v\n%s", err, out)
			}
			if got != tc.result {
				t.Errorf("round trip lost something:\n got %+v\nwant %+v", got, tc.result)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("output does not carry %q:\n%s", tc.want, out)
			}
			// Who did it is the half an audit cares about, and it is the field
			// most easily dropped since the prose already reads fine without it.
			if got.Actor == "" {
				t.Error("no actor recorded")
			}
		})
	}
}
