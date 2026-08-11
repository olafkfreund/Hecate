package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// Server answers MCP requests over a byte stream.
type Server struct {
	Name    string
	Version string
	// Instructions is natural-language guidance a client shows the model about
	// what this server is for. Worth writing well: it is the only place to say
	// "read before you act" to something that will otherwise guess.
	Instructions string

	mu    sync.RWMutex
	tools map[string]Tool
	// era is set by however the client opened the conversation, per the
	// specification: an `initialize` selects legacy semantics for the process,
	// anything carrying modern metadata selects modern.
	era string
}

// New returns a server with no tools.
func New(name, version, instructions string) *Server {
	return &Server{
		Name: name, Version: version, Instructions: instructions,
		tools: map[string]Tool{},
	}
}

// Register adds a tool. Names must be unique, and a duplicate is a programming
// error rather than something to resolve at runtime.
func (s *Server) Register(tools ...Tool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range tools {
		if t.Name == "" || t.Handler == nil {
			return fmt.Errorf("mcp: a tool needs a name and a handler")
		}
		if err := validToolName(t.Name); err != nil {
			return err
		}
		if len(t.InputSchema) == 0 {
			// The specification is explicit that this must be a schema object
			// and must not be null, even when the tool takes nothing.
			return fmt.Errorf("mcp: tool %s has no inputSchema", t.Name)
		}
		if _, taken := s.tools[t.Name]; taken {
			return fmt.Errorf("mcp: tool %s is already registered", t.Name)
		}
		s.tools[t.Name] = t
	}
	return nil
}

// MustRegister panics on failure, for wiring at startup.
func (s *Server) MustRegister(tools ...Tool) {
	if err := s.Register(tools...); err != nil {
		panic(err)
	}
}

// validToolName enforces the character set the specification asks for, so a
// name that a strict client would reject fails here instead.
func validToolName(name string) error {
	if len(name) > 128 {
		return fmt.Errorf("mcp: tool name %q is longer than 128 characters", name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '-', r == '.':
		default:
			return fmt.Errorf("mcp: tool name %q contains %q, but only letters, digits, underscore, hyphen and dot are allowed", name, r)
		}
	}
	return nil
}

// Serve reads newline-delimited JSON-RPC from r and writes replies to w.
//
// Nothing but protocol messages may go to w: a stray print to stdout corrupts
// the stream and the client's failure will point anywhere but here. Logging
// belongs on stderr, which the specification reserves for exactly that.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	in := bufio.NewScanner(r)
	// A tool result carrying an Explanation comfortably exceeds the default
	// 64KB, and a truncated line would fail as a parse error somewhere else.
	in.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	out := json.NewEncoder(w)
	for in.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := in.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		reply := s.Handle(ctx, line)
		if reply == nil {
			continue // a notification: the specification requires no reply
		}
		if err := out.Encode(reply); err != nil {
			return fmt.Errorf("mcp: writing a reply: %w", err)
		}
	}
	return in.Err()
}

// Handle processes one message and returns the reply, or nil for a
// notification.
func (s *Server) Handle(ctx context.Context, raw []byte) any {
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		return errorReply(nil, codeParseError, "Parse error", nil)
	}
	if req.Method == "" {
		return errorReply(req.ID, codeInvalidRequest, "Invalid request: no method", nil)
	}

	var p params
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return errorReply(req.ID, codeInvalidParams, "Invalid params", nil)
		}
	}

	// Which era the client is speaking, decided by how it opened. A modern
	// request declares its version; `initialize` is legacy by definition.
	if v := p.version(); v != "" {
		s.setEra(eraModern)
		if v != VersionModern {
			return errorReply(req.ID, codeUnsupportedVer, "Unsupported protocol version",
				map[string]any{"supported": SupportedVersions(), "requested": v})
		}
	}

	switch req.Method {
	case "initialize":
		s.setEra(eraLegacy)
		return s.initialize(req.ID, p)

	case "server/discover":
		return reply(req.ID, s.discover())

	case "tools/list":
		return reply(req.ID, s.list())

	case "tools/call":
		if req.isNotification() {
			return nil
		}
		return reply(req.ID, s.call(ctx, p))

	case "ping":
		return reply(req.ID, map[string]any{})

	default:
		// Notifications we do not act on — `notifications/initialized`,
		// `notifications/cancelled` — are silently fine. Answering a
		// notification at all would be the protocol violation.
		if req.isNotification() {
			return nil
		}
		return errorReply(req.ID, codeMethodNotFound, "Method not found: "+req.Method, nil)
	}
}

const (
	eraModern = "modern"
	eraLegacy = "legacy"
)

func (s *Server) setEra(era string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.era = era
}

// resultType is the field modern results carry and legacy ones must not.
func (s *Server) resultType() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.era == eraLegacy {
		return ""
	}
	return "complete"
}

// initialize answers the legacy handshake.
func (s *Server) initialize(id json.RawMessage, p params) any {
	// Echo the client's version when we know it. A legacy client has no way to
	// fall forward, so answering with something it did not ask for is how a
	// session dies with no useful diagnostic.
	version := VersionLegacy
	for _, known := range legacyVersions {
		if p.ProtocolVersion == known {
			version = known
			break
		}
	}
	return reply(id, map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": s.Name, "version": s.Version},
		"instructions":    s.Instructions,
	})
}

// discover answers the modern replacement for the handshake.
func (s *Server) discover() map[string]any {
	return map[string]any{
		"resultType":        "complete",
		"supportedVersions": SupportedVersions(),
		"capabilities":      map[string]any{"tools": map[string]any{}},
		"instructions":      s.Instructions,
		"_meta": map[string]any{
			metaServerInfo: map[string]any{"name": s.Name, "version": s.Version},
		},
	}
}

// list answers tools/list, in a stable order.
//
// Ordering is not cosmetic: clients cache the tool list, and a list that
// reshuffles between calls invalidates that cache and the model's prompt cache
// with it.
func (s *Server) list() map[string]any {
	s.mu.RLock()
	tools := make([]Tool, 0, len(s.tools))
	for _, t := range s.tools {
		tools = append(tools, t)
	}
	s.mu.RUnlock()

	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })

	out := map[string]any{"tools": tools}
	if rt := s.resultType(); rt != "" {
		out["resultType"] = rt
	}
	return out
}

// call runs a tool.
func (s *Server) call(ctx context.Context, p params) any {
	s.mu.RLock()
	tool, ok := s.tools[p.Name]
	s.mu.RUnlock()

	if !ok {
		// An unknown tool is a protocol error rather than a tool error: the
		// model cannot fix it by trying different arguments.
		return &rpcError{Code: codeInvalidParams, Message: "Unknown tool: " + p.Name}
	}

	structured, text, err := tool.Handler(ctx, p.Arguments)
	result := toolResult{ResultType: s.resultType()}
	if err != nil {
		// A tool execution error: reported in the result so the client hands it
		// to the model, which can then correct itself. Returning a JSON-RPC
		// error here would be telling the model less than we know.
		result.Content = []any{textContent(err.Error())}
		result.IsError = true
		return result
	}

	if text == "" && structured != nil {
		if encoded, mErr := json.MarshalIndent(structured, "", "  "); mErr == nil {
			text = string(encoded)
		}
	}
	result.Content = []any{textContent(text)}
	result.StructuredContent = structured
	return result
}

// reply wraps a result, unless it is already an error.
func reply(id json.RawMessage, result any) any {
	if e, isErr := result.(*rpcError); isErr {
		return &response{JSONRPC: "2.0", ID: id, Error: e}
	}
	if r, ok := result.(*response); ok {
		return r
	}
	return &response{JSONRPC: "2.0", ID: id, Result: result}
}

func errorReply(id json.RawMessage, code int, message string, data any) any {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return &response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message, Data: data}}
}
