package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/olafkfreund/hecate/pkg/ops"
)

// ActorPrefix marks an action as having come through this server.
//
// A promotion an agent performed on someone's behalf is not the same event as
// one that person performed, and the trail should say which. The configured
// actor is who authorised the server to act; the prefix is how it acted.
const ActorPrefix = "mcp:"

// WriteTools are the tools that change something.
//
// There are two, and there will not be a third by accident: `approve` is
// absent, and absent by construction rather than by configuration. See
// [ApproveIsNotAvailable].
//
// They enforce nothing themselves. Every rule — eligibility, upstream
// clearance, promotion windows, one crossing at a time — is pkg/ops', which is
// the same code the CLI and the controller go through. An MCP client is a
// client, not a bypass.
func WriteTools(o *ops.Ops, defaultNamespace, actor string) []Tool {
	ns := func(args map[string]any) string {
		if v, ok := args["namespace"].(string); ok && v != "" {
			return v
		}
		return defaultNamespace
	}
	who := ActorPrefix + actor

	return []Tool{
		{
			Name:  "promote",
			Title: "Promote a Bundle",
			Description: "Ask a Gate to cross a Bundle, opening a Passage. " +
				"The same rules apply as to an automatic crossing: a Bundle that has not cleared an upstream Gate, " +
				"or lacks an approval, or falls outside a promotion window, is refused. " +
				"Call why_stuck first if you are not sure the Bundle is eligible.",
			InputSchema: schema(`{
				"type": "object",
				"properties": {
					"gate": {"type": "string", "description": "The Gate to cross."},
					"bundle": {"type": "string", "description": "The Bundle to move."},
					"namespace": {"type": "string"}
				},
				"required": ["gate", "bundle"],
				"additionalProperties": false
			}`),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, string, error) {
				args, err := decodeArgs(raw)
				if err != nil {
					return nil, "", err
				}
				gate, err := required(args, "gate")
				if err != nil {
					return nil, "", err
				}
				bundle, err := required(args, "bundle")
				if err != nil {
					return nil, "", err
				}

				p, err := o.Promote(ctx, ns(args), gate, bundle, who)
				if err != nil {
					return nil, "", err
				}
				return map[string]any{
						"passage": p.Name, "gate": p.Spec.Gate,
						"bundle": p.Spec.Bundle, "actor": p.Spec.Actor,
					},
					fmt.Sprintf("opened Passage %s to cross %s through %s", p.Name, p.Spec.Bundle, p.Spec.Gate),
					nil
			},
		},
		{
			Name:  "abort",
			Title: "Abort a crossing",
			Description: "Ask a running Passage to stop. Its remaining steps are marked aborted. " +
				"The Passage is kept as the record that a crossing was started and stopped; nothing is deleted. " +
				"Work a step has already done — a commit that was pushed — is not undone.",
			InputSchema: schema(`{
				"type": "object",
				"properties": {
					"passage": {"type": "string", "description": "The Passage to stop."},
					"namespace": {"type": "string"}
				},
				"required": ["passage"],
				"additionalProperties": false
			}`),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, string, error) {
				args, err := decodeArgs(raw)
				if err != nil {
					return nil, "", err
				}
				name, err := required(args, "passage")
				if err != nil {
					return nil, "", err
				}
				if err := o.Abort(ctx, ns(args), name, who); err != nil {
					return nil, "", err
				}
				return map[string]any{"passage": name, "aborted": true, "actor": who},
					fmt.Sprintf("asked %s to stop; the controller will mark its remaining steps aborted", name),
					nil
			},
		},
	}
}

// ApproveIsNotAvailable records why there is no `approve` tool, so that the
// absence reads as a decision rather than an oversight to be helpfully fixed.
//
// Approval is a segregation-of-duties control: Fides' change gate withholds
// approval until a human who is not the committer has signed off, and a Gate's
// requireApproval exists to make a person look at a Bundle before it moves.
// An agent that can approve can be one of the two required eyes — and one
// operator driving an agent can then be both. The control would still appear in
// every audit trail while having stopped meaning anything, which is worse than
// not having it: an absent control is visibly absent.
//
// This is not a setting. A flag to enable it would be a flag to make the
// guarantee untrue, and the value of a guarantee is that it holds without
// anyone having to check how the server was started. Approval stays with the
// CLI, the API and the Fides UI, where a person is the one acting.
const ApproveIsNotAvailable = "approval is a segregation-of-duties control and is not exposed over MCP"
