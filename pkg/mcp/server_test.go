package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	s := New("hecate", "test", "instructions here")
	s.MustRegister(Tool{
		Name:         "echo",
		Title:        "Echo",
		Description:  "Return what it was given",
		InputSchema:  schema(`{"type":"object","properties":{"say":{"type":"string"}},"required":["say"]}`),
		OutputSchema: schema(`{"type":"object","properties":{"said":{"type":"string"}}}`),
		Handler: func(_ context.Context, raw json.RawMessage) (any, string, error) {
			args, err := decodeArgs(raw)
			if err != nil {
				return nil, "", err
			}
			say, err := required(args, "say")
			if err != nil {
				return nil, "", err
			}
			return map[string]any{"said": say}, "said " + say, nil
		},
	})
	return s
}

// ask sends one message and decodes the reply.
func ask(t *testing.T, s *Server, msg string) map[string]any {
	t.Helper()
	out := s.Handle(context.Background(), []byte(msg))
	if out == nil {
		t.Fatalf("no reply to %s", msg)
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func result(t *testing.T, reply map[string]any) map[string]any {
	t.Helper()
	if e, isErr := reply["error"]; isErr {
		t.Fatalf("unexpected error reply: %v", e)
	}
	r, ok := reply["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %v", reply)
	}
	return r
}

const modernMeta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}`

// The modern revision replaced the handshake with per-request metadata, and
// server/discover is the one method a modern server MUST implement.
func TestModernDiscover(t *testing.T) {
	s := testServer(t)
	r := result(t, ask(t, s, `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{`+modernMeta+`}}`))

	if r["resultType"] != "complete" {
		t.Errorf("resultType = %v", r["resultType"])
	}
	versions, _ := r["supportedVersions"].([]any)
	if len(versions) == 0 || versions[0] != VersionModern {
		t.Errorf("supportedVersions = %v, want the modern revision first", versions)
	}
	// serverInfo moved into _meta under a namespaced key.
	meta, _ := r["_meta"].(map[string]any)
	info, _ := meta[metaServerInfo].(map[string]any)
	if info["name"] != "hecate" {
		t.Errorf("serverInfo = %v", meta)
	}
	if r["instructions"] != "instructions here" {
		t.Errorf("instructions = %v", r["instructions"])
	}
}

// A modern client asking for a version we do not speak must get the specific
// error carrying what we do speak, so it can retry rather than give up.
func TestUnsupportedVersionIsAnswerable(t *testing.T) {
	s := testServer(t)
	reply := ask(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"1900-01-01"}}}`)

	e, ok := reply["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error in %v", reply)
	}
	if code, _ := e["code"].(float64); int(code) != codeUnsupportedVer {
		t.Errorf("code = %v, want %d", e["code"], codeUnsupportedVer)
	}
	data, _ := e["data"].(map[string]any)
	if data["requested"] != "1900-01-01" {
		t.Errorf("data = %v — it must echo what was asked for", data)
	}
	supported, _ := data["supported"].([]any)
	if len(supported) == 0 {
		t.Error("the error must list the versions we do support, or the client cannot retry")
	}
}

// The legacy handshake still has to work: most deployed clients use it, and the
// specification's own compatibility matrix says a legacy client against a
// modern-only server simply fails.
func TestLegacyHandshake(t *testing.T) {
	s := testServer(t)
	r := result(t, ask(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{}}}`))

	// Echoed, because a legacy client cannot fall forward: answering with a
	// version it did not ask for is how a session dies with no diagnostic.
	if r["protocolVersion"] != "2025-06-18" {
		t.Errorf("protocolVersion = %v, want the client's own", r["protocolVersion"])
	}
	info, _ := r["serverInfo"].(map[string]any)
	if info["name"] != "hecate" {
		t.Errorf("serverInfo = %v", r["serverInfo"])
	}
	// Legacy results must not carry the modern field.
	if _, present := r["resultType"]; present {
		t.Error("a legacy result carried resultType")
	}
}

func TestLegacyUnknownVersionFallsBackToOurNewest(t *testing.T) {
	s := testServer(t)
	r := result(t, ask(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`))

	if r["protocolVersion"] != VersionLegacy {
		t.Errorf("protocolVersion = %v, want %s", r["protocolVersion"], VersionLegacy)
	}
}

// Once a client has opened legacy, its results must stay legacy — the modern
// resultType field would be unexpected to it.
func TestEraFollowsHowTheClientOpened(t *testing.T) {
	legacy := testServer(t)
	ask(t, legacy, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	r := result(t, ask(t, legacy, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`))
	if _, present := r["resultType"]; present {
		t.Error("a legacy session got a modern resultType")
	}

	modern := testServer(t)
	r = result(t, ask(t, modern, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{`+modernMeta+`}}`))
	if r["resultType"] != "complete" {
		t.Errorf("a modern session did not get resultType: %v", r)
	}
}

func TestToolsList(t *testing.T) {
	s := testServer(t)
	r := result(t, ask(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{`+modernMeta+`}}`))

	tools, _ := r["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(tools))
	}
	tool, _ := tools[0].(map[string]any)
	if tool["name"] != "echo" || tool["title"] != "Echo" {
		t.Errorf("tool = %v", tool)
	}
	// inputSchema is required and must never be null, even for a tool that
	// takes nothing — clients reject a tool whose schema is not an object.
	if _, ok := tool["inputSchema"].(map[string]any); !ok {
		t.Errorf("inputSchema = %v, want an object", tool["inputSchema"])
	}
	if _, ok := tool["outputSchema"].(map[string]any); !ok {
		t.Errorf("outputSchema = %v, want an object", tool["outputSchema"])
	}
}

// structuredContent is what makes a result something a model can reason over
// rather than parse out of prose.
func TestToolCallReturnsStructuredContent(t *testing.T) {
	s := testServer(t)
	r := result(t, ask(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{`+modernMeta+`,"name":"echo","arguments":{"say":"hello"}}}`))

	structured, _ := r["structuredContent"].(map[string]any)
	if structured["said"] != "hello" {
		t.Errorf("structuredContent = %v", r["structuredContent"])
	}
	// And the text rendering, for clients that show it — the specification asks
	// for both.
	content, _ := r["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %v", r["content"])
	}
	first, _ := content[0].(map[string]any)
	if first["type"] != "text" || !strings.Contains(first["text"].(string), "hello") {
		t.Errorf("content[0] = %v", first)
	}
	if r["isError"] == true {
		t.Error("isError was set on a successful call")
	}
}

// A tool that fails reports it in the result, so the client hands it to the
// model and the model can correct itself. A JSON-RPC error would tell it less.
func TestAToolFailureIsInTheResult(t *testing.T) {
	s := testServer(t)
	r := result(t, ask(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{`+modernMeta+`,"name":"echo","arguments":{}}}`))

	if r["isError"] != true {
		t.Fatalf("isError = %v, want true", r["isError"])
	}
	content, _ := r["content"].([]any)
	first, _ := content[0].(map[string]any)
	if !strings.Contains(first["text"].(string), "say is required") {
		t.Errorf("the model is not told what was wrong: %v", first)
	}
}

// An unknown tool is a protocol error: no arguments the model could choose
// would make it work, so it is not something to self-correct from.
func TestAnUnknownToolIsAProtocolError(t *testing.T) {
	s := testServer(t)
	reply := ask(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{`+modernMeta+`,"name":"teleport"}}`)

	e, ok := reply["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error in %v", reply)
	}
	if !strings.Contains(e["message"].(string), "teleport") {
		t.Errorf("message = %v", e["message"])
	}
}

// Replying to a notification is itself a protocol violation.
func TestNotificationsGetNoReply(t *testing.T) {
	s := testServer(t)
	for _, msg := range []string{
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`,
	} {
		if out := s.Handle(context.Background(), []byte(msg)); out != nil {
			t.Errorf("%s got a reply: %v", msg, out)
		}
	}
}

func TestMalformedInput(t *testing.T) {
	s := testServer(t)

	reply := ask(t, s, `{not json`)
	e, _ := reply["error"].(map[string]any)
	if code, _ := e["code"].(float64); int(code) != codeParseError {
		t.Errorf("code = %v, want %d", e["code"], codeParseError)
	}

	reply = ask(t, s, `{"jsonrpc":"2.0","id":1,"method":"nonexistent","params":{`+modernMeta+`}}`)
	e, _ = reply["error"].(map[string]any)
	if code, _ := e["code"].(float64); int(code) != codeMethodNotFound {
		t.Errorf("code = %v, want %d", e["code"], codeMethodNotFound)
	}
}

// The stream is newline-delimited, and a message must never contain an embedded
// newline: one that did would be read as two malformed messages.
func TestServeIsNewlineDelimited(t *testing.T) {
	s := testServer(t)
	in := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{` + modernMeta + `}}` + "\n" +
			`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{` + modernMeta + `,"name":"echo","arguments":{"say":"multi\nline"}}}` + "\n")

	var out strings.Builder
	if err := s.Serve(context.Background(), in, &out); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	// Two requests, one notification: two replies.
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2:\n%s", len(lines), out.String())
	}
	for i, line := range lines {
		var check map[string]any
		if err := json.Unmarshal([]byte(line), &check); err != nil {
			t.Errorf("line %d is not one JSON message: %v", i, err)
		}
	}
	// A newline inside a value must be escaped rather than breaking the frame.
	if !strings.Contains(lines[1], `multi\nline`) {
		t.Errorf("an embedded newline was not escaped: %s", lines[1])
	}
}

func TestRegisterRefusesBadTools(t *testing.T) {
	s := New("t", "1", "")
	for name, tool := range map[string]Tool{
		"no name":    {Handler: func(context.Context, json.RawMessage) (any, string, error) { return nil, "", nil }, InputSchema: schema(`{}`)},
		"no handler": {Name: "x", InputSchema: schema(`{}`)},
		"no schema":  {Name: "x", Handler: func(context.Context, json.RawMessage) (any, string, error) { return nil, "", nil }},
		"bad name":   {Name: "has space", InputSchema: schema(`{}`), Handler: func(context.Context, json.RawMessage) (any, string, error) { return nil, "", nil }},
		"worse name": {Name: "has/slash", InputSchema: schema(`{}`), Handler: func(context.Context, json.RawMessage) (any, string, error) { return nil, "", nil }},
	} {
		if err := s.Register(tool); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}

	// And a duplicate, which is a wiring mistake rather than a runtime concern.
	ok := Tool{Name: "fine", InputSchema: schema(`{}`),
		Handler: func(context.Context, json.RawMessage) (any, string, error) { return nil, "", nil }}
	if err := s.Register(ok); err != nil {
		t.Fatal(err)
	}
	if err := s.Register(ok); err == nil {
		t.Error("a duplicate tool name was accepted")
	}
}
