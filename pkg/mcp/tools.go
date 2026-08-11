package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/olafkfreund/hecate/pkg/ops"
)

// ReadTools are the tools that only look.
//
// Every one is a marshalling shim over pkg/ops. That is the whole design: a
// question answered here rather than there would be a second answer to it, and
// the operator would eventually be told two different things by two tools
// reading the same cluster (D32).
//
// Writes are deliberately absent. They are gated on an explicit opt-in and
// carry a question this layer cannot answer by itself — whether an agent may
// satisfy a four-eyes approval — so they are tracked separately.
func ReadTools(o *ops.Ops, defaultNamespace string) []Tool {
	ns := func(args map[string]any) string {
		if v, ok := args["namespace"].(string); ok && v != "" {
			return v
		}
		return defaultNamespace
	}

	return []Tool{
		{
			Name:        "list_gates",
			Title:       "List Gates",
			Description: "List the Gates in a namespace with what each is currently doing: its state, the Bundle it holds, its health, and a one-line summary. Start here when asked about the state of deployments.",
			InputSchema: schema(`{
				"type": "object",
				"properties": {"namespace": {"type": "string", "description": "Kubernetes namespace; defaults to the server's."}},
				"additionalProperties": false
			}`),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, string, error) {
				args, err := decodeArgs(raw)
				if err != nil {
					return nil, "", err
				}
				gates, err := o.Gates(ctx, ns(args))
				if err != nil {
					return nil, "", err
				}

				out := make([]*ops.Explanation, 0, len(gates))
				var lines []string
				for i := range gates {
					ex, err := o.Explain(ctx, ns(args), gates[i].Name)
					if err != nil {
						return nil, "", err
					}
					out = append(out, ex)
					lines = append(lines, fmt.Sprintf("%s: %s — %s", ex.Gate, ex.State, ex.Summary))
				}
				if len(out) == 0 {
					return out, "no Gates in " + ns(args), nil
				}
				return out, strings.Join(lines, "\n"), nil
			},
		},
		{
			Name:        "get_gate",
			Title:       "Get a Gate",
			Description: "Read one Gate in full, including its admission rules, its steps and its status.",
			InputSchema: schema(`{
				"type": "object",
				"properties": {
					"name": {"type": "string", "description": "The Gate's name."},
					"namespace": {"type": "string"}
				},
				"required": ["name"],
				"additionalProperties": false
			}`),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, string, error) {
				args, err := decodeArgs(raw)
				if err != nil {
					return nil, "", err
				}
				name, err := required(args, "name")
				if err != nil {
					return nil, "", err
				}
				gate, err := o.Gate(ctx, ns(args), name)
				if err != nil {
					return nil, "", err
				}
				return gate, "", nil
			},
		},
		{
			Name:        "list_bundles",
			Title:       "List Bundles",
			Description: "List the Bundles in a namespace, newest first. A Bundle is an immutable set of artifact versions — the thing that moves through Gates.",
			InputSchema: schema(`{
				"type": "object",
				"properties": {"namespace": {"type": "string"}},
				"additionalProperties": false
			}`),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, string, error) {
				args, err := decodeArgs(raw)
				if err != nil {
					return nil, "", err
				}
				bundles, err := o.Bundles(ctx, ns(args))
				if err != nil {
					return nil, "", err
				}
				return bundles, "", nil
			},
		},
		{
			Name:        "get_passage",
			Title:       "Get a Passage",
			Description: "Read one Passage — a single attempt to move a Bundle through a Gate — including every step's phase, reason code, message and output. Use this to see exactly which step failed and why.",
			InputSchema: schema(`{
				"type": "object",
				"properties": {
					"name": {"type": "string", "description": "The Passage's name."},
					"namespace": {"type": "string"}
				},
				"required": ["name"],
				"additionalProperties": false
			}`),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, string, error) {
				args, err := decodeArgs(raw)
				if err != nil {
					return nil, "", err
				}
				name, err := required(args, "name")
				if err != nil {
					return nil, "", err
				}
				p, err := o.Passage(ctx, ns(args), name)
				if err != nil {
					return nil, "", err
				}
				return p, "", nil
			},
		},
		{
			Name:  "why_stuck",
			Title: "Why is a Gate stuck?",
			Description: "Explain why a Gate is not crossing anything: an unmet upstream Gate, a missing approval, a closed promotion window, a failed step, a held evidence gate, or a Flux resource that is not converging. " +
				"Returns typed blockers, each with the command that would unblock it. This is the tool to reach for when asked why a deployment has not happened.",
			InputSchema: schema(`{
				"type": "object",
				"properties": {
					"gate": {"type": "string", "description": "The Gate's name."},
					"namespace": {"type": "string"}
				},
				"required": ["gate"],
				"additionalProperties": false
			}`),
			// Published because this is the tool a model reasons over, and the
			// blocker kinds are the part worth being explicit about: they are a
			// closed set, so a client can branch on them rather than match prose.
			OutputSchema: schema(`{
				"type": "object",
				"properties": {
					"gate": {"type": "string"},
					"namespace": {"type": "string"},
					"state": {"type": "string", "enum": ["Crossing", "Ready", "Blocked", "Idle", "Failed"]},
					"summary": {"type": "string"},
					"blockers": {
						"type": "array",
						"items": {
							"type": "object",
							"properties": {
								"kind": {"type": "string", "enum": [
									"Suspended", "InvalidSteps", "NoPassageTemplate", "NoBundles",
									"AwaitingApproval", "UpstreamNotCleared", "WindowClosed",
									"PassageFailed", "StepWaiting", "Unhealthy", "AwaitingRequest"]},
								"detail": {"type": "string"},
								"fix": {"type": "string"}
							},
							"required": ["kind", "detail"]
						}
					},
					"current": {"type": "string"},
					"eligible": {"type": "array", "items": {"type": "string"}},
					"waiting": {
						"type": "array",
						"items": {
							"type": "object",
							"properties": {"bundle": {"type": "string"}, "reason": {"type": "string"}},
							"required": ["bundle", "reason"]
						}
					},
					"health": {"type": "string"}
				},
				"required": ["gate", "namespace", "state", "summary"]
			}`),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, string, error) {
				args, err := decodeArgs(raw)
				if err != nil {
					return nil, "", err
				}
				name, err := required(args, "gate")
				if err != nil {
					return nil, "", err
				}
				ex, err := o.Explain(ctx, ns(args), name)
				if err != nil {
					return nil, "", err
				}
				return ex, renderExplanation(ex), nil
			},
		},
	}
}

// renderExplanation is the human-readable half, for clients that show the text
// rather than the structure — and for a model that reads both.
func renderExplanation(ex *ops.Explanation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s is %s: %s\n", ex.Gate, ex.State, ex.Summary)
	for _, blocker := range ex.Blockers {
		fmt.Fprintf(&b, "\n[%s] %s", blocker.Kind, blocker.Detail)
		if blocker.Fix != "" {
			fmt.Fprintf(&b, "\n  fix: %s", blocker.Fix)
		}
	}
	if len(ex.Eligible) > 0 {
		fmt.Fprintf(&b, "\n\neligible: %s", strings.Join(ex.Eligible, ", "))
	}
	for _, w := range ex.Waiting {
		fmt.Fprintf(&b, "\nwaiting: %s — %s", w.Bundle, w.Reason)
	}
	return b.String()
}

// schema keeps the tool definitions readable by letting them hold JSON Schema
// as JSON. Invalid schema is a programming error and panics at startup rather
// than being served to a client that will reject the whole tool.
func schema(s string) json.RawMessage {
	var check any
	if err := json.Unmarshal([]byte(s), &check); err != nil {
		panic("mcp: invalid tool schema: " + err.Error())
	}
	compact, err := json.Marshal(check)
	if err != nil {
		panic("mcp: invalid tool schema: " + err.Error())
	}
	return compact
}

func decodeArgs(raw json.RawMessage) (map[string]any, error) {
	args := map[string]any{}
	if len(raw) == 0 {
		return args, nil
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("arguments must be a JSON object: %s", err)
	}
	return args, nil
}

func required(args map[string]any, key string) (string, error) {
	v, ok := args[key].(string)
	if !ok || v == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	return v, nil
}
