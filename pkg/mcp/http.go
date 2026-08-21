package mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// ProtocolVersionHeader carries the negotiated version on HTTP.
//
// On stdio the version travels in the request's `_meta`; on this transport the
// specification moves it to a header, so a proxy can route on it without
// parsing a body.
const ProtocolVersionHeader = "MCP-Protocol-Version"

// maxBody bounds one request. A tool result carrying an Explanation is large,
// but nothing a client sends is — and an unbounded read here is a way to spend
// the server's memory from outside it.
const maxBody = 4 << 20

// HTTPOptions configures the HTTP transport.
type HTTPOptions struct {
	// Authenticate decides whether a request may proceed. Returning an error
	// refuses it with 401.
	//
	// A function rather than a credential or a Kubernetes client, so this
	// package stays a transport: what counts as authenticated is a deployment's
	// question, and a server embedded in a CLI answers it differently from one
	// behind an ingress. Nil means no authentication, which is why Handler
	// refuses to build without AllowUnauthenticated set as well.
	Authenticate func(*http.Request) error

	// AllowUnauthenticated permits Authenticate to be nil.
	//
	// Spelled out rather than inferred from a nil check, because "no
	// authentication" is a decision and reaching it by forgetting to set a
	// field is not. An MCP server exposes tools that promote and abort; the
	// failure mode of getting this wrong quietly is an open write API.
	AllowUnauthenticated bool

	// AllowedOrigins are the browser origins permitted to call this server.
	//
	// Empty rejects every request carrying an Origin header, which is the safe
	// default and not the same as allowing none: a client that is not a browser
	// sends no Origin and is unaffected. The specification asks for this
	// explicitly — a local MCP server with no Origin check can be driven by any
	// page the user visits, which is DNS rebinding with extra steps.
	AllowedOrigins []string
}

// Handler serves MCP over streamable HTTP.
//
// One endpoint, taking POSTed JSON-RPC. Replies are returned as
// application/json rather than as an event stream: the specification allows
// either, and every tool here answers in one message — a stream would be a
// framing with nothing to frame.
//
// The dispatch is Handle, the same one stdio uses, so a tool cannot behave
// differently depending on how it was reached.
func (s *Server) Handler(opts HTTPOptions) (http.Handler, error) {
	if opts.Authenticate == nil && !opts.AllowUnauthenticated {
		return nil, fmt.Errorf(
			"mcp: refusing to serve HTTP with no authentication — set Authenticate, " +
				"or AllowUnauthenticated to say the exposure is deliberate")
	}
	origins, err := normaliseOrigins(opts.AllowedOrigins)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /", func(w http.ResponseWriter, r *http.Request) {
		s.serveHTTP(w, r, opts, origins)
	})
	// Answered explicitly rather than left to the mux's 405, so a client that
	// tries the GET stream some servers offer learns why rather than guessing.
	mux.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Allow", "POST")
		writeRPCError(w, http.StatusMethodNotAllowed, nil, codeInvalidRequest,
			"this server does not open a server-to-client stream; POST each request instead")
	})
	return mux, nil
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request, opts HTTPOptions, origins map[string]bool) {
	if origin := r.Header.Get("Origin"); origin != "" {
		if !origins[strings.ToLower(origin)] {
			// Refused before authentication, because the point is to stop a page
			// the user did not mean to trust from spending their credentials —
			// and a browser attaches those on its own.
			writeRPCError(w, http.StatusForbidden, nil, codeInvalidRequest,
				"origin "+origin+" is not allowed to reach this server")
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
	}

	if opts.Authenticate != nil {
		if err := opts.Authenticate(r); err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="hecate-mcp"`)
			writeRPCError(w, http.StatusUnauthorized, nil, codeInvalidRequest, err.Error())
			return
		}
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		writeRPCError(w, http.StatusBadRequest, nil, codeParseError, "could not read the request")
		return
	}

	// The header is authoritative for the version on this transport, so a
	// request that sets it is answered as if it had declared the same version
	// in _meta. Without this a client following the HTTP specification would be
	// treated as legacy by a server that only reads _meta — which is the
	// interoperability failure this transport is most likely to produce.
	if v := r.Header.Get(ProtocolVersionHeader); v != "" {
		raw, err = withMetaVersion(raw, v)
		if err != nil {
			writeRPCError(w, http.StatusBadRequest, nil, codeParseError, "Parse error")
			return
		}
	}

	reply := s.Handle(r.Context(), raw)
	if reply == nil {
		// A notification. The specification requires no reply, and 202 says the
		// message was accepted without claiming an answer follows.
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// Echoed so a client can confirm what it is talking to, and so a proxy sees
	// the version on the way back as well as the way in.
	if v := r.Header.Get(ProtocolVersionHeader); v != "" {
		w.Header().Set(ProtocolVersionHeader, v)
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(reply); err != nil {
		// The status is already written; nothing useful is left to say to a
		// client whose connection has gone.
		return
	}
}

// withMetaVersion puts the header's version into the request's _meta, so the
// one dispatch path reads the version from one place whichever transport
// carried it.
//
// The request's own _meta wins if it has one: a client that declared a version
// in the body meant it, and quietly overwriting it would make the header a way
// to change what a message says.
func withMetaVersion(raw []byte, version string) ([]byte, error) {
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		// Not an object — a batch, or malformed. Handle answers either better
		// than this function can, so pass it through untouched.
		return raw, nil //nolint:nilerr // deliberate: dispatch reports the real error
	}

	params := map[string]json.RawMessage{}
	if p, ok := msg["params"]; ok {
		if err := json.Unmarshal(p, &params); err != nil {
			return raw, nil //nolint:nilerr // params is not an object; let dispatch say so
		}
	}

	meta := map[string]json.RawMessage{}
	if m, ok := params["_meta"]; ok {
		if err := json.Unmarshal(m, &meta); err != nil {
			return raw, nil //nolint:nilerr // _meta is not an object; let dispatch say so
		}
	}
	if _, already := meta[metaProtocolVersion]; already {
		return raw, nil
	}

	encoded, err := json.Marshal(version)
	if err != nil {
		return nil, err
	}
	meta[metaProtocolVersion] = encoded

	if params["_meta"], err = json.Marshal(meta); err != nil {
		return nil, err
	}
	if msg["params"], err = json.Marshal(params); err != nil {
		return nil, err
	}
	return json.Marshal(msg)
}

// normaliseOrigins lowercases and validates the allowlist.
func normaliseOrigins(in []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, o := range in {
		u, err := url.Parse(o)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf(
				"mcp: %q is not an origin — an origin is scheme://host[:port], with no path", o)
		}
		// Rebuilt from the parts rather than lowercased whole, so a trailing
		// slash or a path in the configured value cannot make an origin that
		// never matches and is never noticed.
		out[strings.ToLower(u.Scheme+"://"+u.Host)] = true
	}
	return out, nil
}

// writeRPCError answers with an HTTP status and a JSON-RPC error body.
//
// Both, because the two audiences differ: a proxy and a browser act on the
// status, and an MCP client reads the body. A transport error carrying only one
// of them leaves the other guessing.
func writeRPCError(w http.ResponseWriter, status int, id json.RawMessage, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorReply(id, code, message, nil))
}

// LocalOrigins are the origins a server bound to loopback should allow.
//
// Provided because assembling them by hand is where the mistake happens: a
// browser sends `http://localhost:3000` and `http://127.0.0.1:3000` as distinct
// origins, and an allowlist naming one rejects the other for reasons nobody
// enjoys diagnosing.
func LocalOrigins(port string) []string {
	return []string{
		"http://" + net.JoinHostPort("localhost", port),
		"http://" + net.JoinHostPort("127.0.0.1", port),
		// JoinHostPort adds the brackets; passing them produces [[::1]].
		"http://" + net.JoinHostPort("::1", port),
	}
}
