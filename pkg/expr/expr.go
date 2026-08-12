// Package expr evaluates the `${{ ... }}` expressions in a Passage's steps.
//
// Built on expr-lang: sandboxed, no I/O, no arbitrary code. Deliberately not Go
// templates — a promotion step evaluating arbitrary templates over cluster data
// is an escalation path, and the expressions here are evaluated on data an
// attacker can influence (image tags, commit messages, PR titles).
package expr

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/expr-lang/expr"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// pattern matches `${{ ... }}`, non-greedily so two expressions in one string
// are two matches rather than one spanning both.
var pattern = regexp.MustCompile(`\$\{\{(.*?)\}\}`)

// Env is what expressions can see. Nothing else is reachable — no cluster, no
// filesystem, no environment variables.
type Env struct {
	Bundle *v1alpha1.Bundle
	// Steps maps a step's `as:` alias to its output.
	Steps map[string]map[string]any
	// Vars are the Gate's declared variables.
	Vars map[string]string
	// Failed reports that an earlier step has already failed the Passage.
	//
	// It exists so a step can say `if: ${{ failed }}` and run precisely when
	// something went wrong — reporting the outcome, recording evidence, tidying
	// up. Without it the only steps that can ever run are the ones on the happy
	// path, and anything that reports a result could only ever report success.
	Failed bool
	// Context about the crossing itself.
	Gate      string
	Passage   string
	Actor     string
	Namespace string
}

// build turns Env into the map expr evaluates against.
func (e Env) build() map[string]any {
	steps := e.Steps
	if steps == nil {
		steps = map[string]map[string]any{}
	}
	vars := e.Vars
	if vars == nil {
		vars = map[string]string{}
	}

	return map[string]any{
		"steps": toAnyMap(steps),
		"vars":  vars,
		"gate":  e.Gate,
		// `passage` is the object name, useful in commit messages so a change
		// can be traced back to the crossing that made it.
		"passage":   e.Passage,
		"actor":     e.Actor,
		"namespace": e.Namespace,
		"bundle":    e.bundleEnv(),
		"failed":    e.Failed,
		// `always` is simply true. It reads as intent where `if: ${{ true }}`
		// reads as a mistake, and it is the word people already know from CI
		// for "run this whatever happened".
		"always": true,
	}
}

// bundleEnv exposes the Bundle by lookup function rather than as a list.
//
// `bundle.image('ghcr.io/acme/api').tag` says what it means; indexing into an
// array by position would silently pick the wrong artifact the day someone
// reorders a Beacon's watch list.
func (e Env) bundleEnv() map[string]any {
	var artifacts []v1alpha1.Artifact
	name, digest, alias := "", "", ""
	if e.Bundle != nil {
		artifacts = e.Bundle.Spec.Artifacts
		name = e.Bundle.Name
		digest = e.Bundle.Spec.Digest
		alias = e.Bundle.Spec.Alias
	}

	return map[string]any{
		"name":   name,
		"digest": digest,
		"alias":  alias,

		// Returns nil when there is no match, so an expression referencing an
		// artifact the Bundle does not carry fails with "cannot fetch tag from
		// <nil>" rather than silently interpolating an empty string.
		"image": func(repo string) any {
			for _, a := range artifacts {
				if a.Image != nil && a.Image.Repo == repo {
					return map[string]any{
						"repo": a.Image.Repo, "tag": a.Image.Tag, "digest": a.Image.Digest,
					}
				}
			}
			return nil
		},
		"chart": func(repo string) any {
			for _, a := range artifacts {
				if a.Chart != nil && a.Chart.Repo == repo {
					return map[string]any{
						"repo": a.Chart.Repo, "name": a.Chart.Name, "version": a.Chart.Version,
					}
				}
			}
			return nil
		},
		"commit": func(repo string) any {
			for _, a := range artifacts {
				if a.Commit != nil && a.Commit.Repo == repo {
					return map[string]any{
						"repo": a.Commit.Repo, "sha": a.Commit.SHA,
						"branch": a.Commit.Branch, "tag": a.Commit.Tag,
						"message": a.Commit.Message,
					}
				}
			}
			return nil
		},
	}
}

func toAnyMap(m map[string]map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Eval runs one expression and returns its value untouched.
func Eval(code string, env Env) (any, error) {
	program, err := expr.Compile(strings.TrimSpace(code))
	if err != nil {
		return nil, fmt.Errorf("invalid expression %q: %w", strings.TrimSpace(code), err)
	}
	out, err := expr.Run(program, env.build())
	if err != nil {
		return nil, fmt.Errorf("evaluating %q: %w", strings.TrimSpace(code), err)
	}
	return out, nil
}

// Bool evaluates a condition, for `step.if`.
//
// A non-boolean result is an error rather than a truthiness guess: `if: "1"` is
// a mistake, and silently treating it as true would run a step the author
// meant to gate.
func Bool(code string, env Env) (bool, error) {
	out, err := Eval(code, env)
	if err != nil {
		return false, err
	}
	b, ok := out.(bool)
	if !ok {
		return false, fmt.Errorf("condition %q evaluated to %T, not a boolean", strings.TrimSpace(code), out)
	}
	return b, nil
}

// Condition evaluates a boolean expression over an arbitrary environment.
//
// For the one case a step cannot express through Env: a condition about
// something that only exists after the step ran, like an HTTP response. The
// sandbox choice lives here so a step cannot accidentally widen it.
func Condition(code string, env map[string]any) (bool, error) {
	code = strings.TrimSpace(code)
	program, err := expr.Compile(code, expr.Env(env), expr.AsBool())
	if err != nil {
		return false, fmt.Errorf("invalid condition %q: %w", code, err)
	}
	out, err := expr.Run(program, env)
	if err != nil {
		return false, fmt.Errorf("evaluating %q: %w", code, err)
	}
	b, ok := out.(bool)
	if !ok {
		return false, fmt.Errorf("condition %q evaluated to %T, not a boolean", code, out)
	}
	return b, nil
}

// Interpolate substitutes every `${{ ... }}` in s.
//
// When the whole string is a single expression the typed value is returned;
// otherwise the results are stringified into the surrounding text. That
// distinction matters because step configuration is JSON: turning
// `${{ vars.replicas }}` into the string "3" would break a numeric field, while
// `"v${{ vars.major }}"` clearly wants text.
func Interpolate(s string, env Env) (any, error) {
	matches := pattern.FindAllStringSubmatchIndex(s, -1)
	if len(matches) == 0 {
		return s, nil
	}

	// Whole string is one expression: preserve its type.
	if len(matches) == 1 && matches[0][0] == 0 && matches[0][1] == len(s) {
		return Eval(s[matches[0][2]:matches[0][3]], env)
	}

	var b strings.Builder
	last := 0
	for _, m := range matches {
		b.WriteString(s[last:m[0]])
		out, err := Eval(s[m[2]:m[3]], env)
		if err != nil {
			return nil, err
		}
		b.WriteString(stringify(out))
		last = m[1]
	}
	b.WriteString(s[last:])
	return b.String(), nil
}

// stringify renders a value for embedding in text. Structured values become
// JSON rather than Go's `map[...]` formatting, which nothing downstream could
// parse.
func stringify(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case fmt.Stringer:
		return t.String()
	}
	switch v.(type) {
	case map[string]any, []any:
		if encoded, err := json.Marshal(v); err == nil {
			return string(encoded)
		}
	}
	return fmt.Sprintf("%v", v)
}

// InterpolateJSON walks a JSON document and interpolates every string leaf.
//
// Applied by the engine to a step's `with:` block, so interpolation is
// universal rather than something each step remembers to do — and so a step
// added later cannot forget it.
func InterpolateJSON(raw json.RawMessage, env Env) (json.RawMessage, error) {
	if len(raw) == 0 {
		return raw, nil
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("configuration is not valid JSON: %w", err)
	}
	walked, err := walk(doc, env)
	if err != nil {
		return nil, err
	}
	out, err := json.Marshal(walked)
	if err != nil {
		return nil, fmt.Errorf("re-encoding configuration: %w", err)
	}
	return out, nil
}

func walk(v any, env Env) (any, error) {
	switch t := v.(type) {
	case string:
		return Interpolate(t, env)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			// Keys are interpolated too: a map key is as likely to need a
			// variable as a value, and skipping them is a surprise.
			key, err := Interpolate(k, env)
			if err != nil {
				return nil, err
			}
			walked, err := walk(val, env)
			if err != nil {
				return nil, err
			}
			out[stringify(key)] = walked
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			walked, err := walk(val, env)
			if err != nil {
				return nil, err
			}
			out[i] = walked
		}
		return out, nil
	default:
		return v, nil
	}
}
