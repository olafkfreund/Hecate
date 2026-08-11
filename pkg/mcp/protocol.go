// Package mcp is a Model Context Protocol server: the transport that lets an
// LLM client ask Hecate questions.
//
// It is strictly a transport over pkg/ops. Any rule that appears here is a rule
// in the wrong place — the CLI, the API and this server must not be able to
// disagree about what "eligible" means (D32).
//
// # Two protocol eras
//
// MCP changed shape in revision 2026-07-28. Earlier revisions ("legacy") open
// with an `initialize` handshake that negotiates a version for the session.
// From 2026-07-28 ("modern") there is no handshake at all: every request
// carries its protocol version in `_meta`, `server/discover` replaces
// `initialize`, and a version the server does not support is refused per
// request.
//
// This server speaks both, because neither alone is enough. A modern-only
// server is unusable by the many clients still on a legacy revision — the
// specification's own compatibility matrix says that combination simply fails —
// and a legacy-only server is one that has to be rewritten the first time a
// client updates.
package mcp

import (
	"context"
	"encoding/json"
)

// Protocol revisions this server speaks, newest first.
const (
	// VersionModern is the stateless revision: per-request metadata,
	// server/discover, no handshake.
	VersionModern = "2026-07-28"
	// VersionLegacy is the newest handshake-based revision.
	VersionLegacy = "2025-11-25"
)

// legacyVersions are the handshake-based revisions this server will answer an
// `initialize` with. A client asking for one of these gets it echoed back;
// anything else is answered with the newest we know, which is what a legacy
// client expects when its preference is unavailable.
var legacyVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// SupportedVersions is what server/discover advertises.
func SupportedVersions() []string {
	return append([]string{VersionModern}, legacyVersions...)
}

// Meta keys, which the specification requires be namespaced.
const (
	metaProtocolVersion = "io.modelcontextprotocol/protocolVersion"
	metaServerInfo      = "io.modelcontextprotocol/serverInfo"
)

// JSON-RPC error codes. The first five are JSON-RPC 2.0's own; the last is
// MCP's, and is how a modern server refuses a version it does not speak.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
	codeUnsupportedVer = -32022
)

// request is one incoming JSON-RPC message.
//
// A message with no ID is a notification: the specification requires no reply,
// and sending one is a protocol violation rather than merely noise.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (r *request) isNotification() bool { return len(r.ID) == 0 }

// response is one outgoing JSON-RPC message.
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// params carries the request metadata every modern request must include.
type params struct {
	Meta      map[string]json.RawMessage `json:"_meta,omitempty"`
	Name      string                     `json:"name,omitempty"`
	Arguments json.RawMessage            `json:"arguments,omitempty"`
	Cursor    string                     `json:"cursor,omitempty"`
	// ProtocolVersion is the legacy handshake's version field, which lives in
	// params rather than _meta.
	ProtocolVersion string `json:"protocolVersion,omitempty"`
}

// version reads the protocol version a modern request declares, or "".
func (p *params) version() string {
	raw, ok := p.Meta[metaProtocolVersion]
	if !ok {
		return ""
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	return v
}

// Tool is one tool this server exposes.
type Tool struct {
	// Name is what the model calls. The specification restricts these to
	// letters, digits, underscore, hyphen and dot.
	Name string `json:"name"`
	// Title is the human-readable name a client shows.
	Title string `json:"title,omitempty"`
	// Description is what the model reads to decide whether to call it.
	Description string `json:"description"`
	// InputSchema is required and must be a JSON Schema object — never null,
	// even for a tool that takes nothing.
	InputSchema json.RawMessage `json:"inputSchema"`
	// OutputSchema describes structuredContent. Optional, and binding: a
	// server that publishes one must produce results that conform.
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`

	// Handler performs the call. It returns the value to put in
	// structuredContent, and a human-readable rendering for content.
	//
	// An error it returns is a *tool execution* error — reported in the result
	// with isError so the model can read it and correct itself, rather than as
	// a JSON-RPC error which clients are told is less recoverable.
	Handler func(ctx context.Context, args json.RawMessage) (structured any, text string, err error) `json:"-"`
}

// toolResult is a tools/call result.
type toolResult struct {
	ResultType string `json:"resultType,omitempty"`
	Content    []any  `json:"content"`
	// StructuredContent is the machine-readable answer. Providing it is the
	// single biggest difference between a tool a model can reason over and one
	// it has to parse prose from.
	StructuredContent any  `json:"structuredContent,omitempty"`
	IsError           bool `json:"isError,omitempty"`
}

func textContent(s string) map[string]any {
	return map[string]any{"type": "text", "text": s}
}
