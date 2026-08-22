package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// httpServer builds a Server with one tool and serves it over HTTP.
func httpServer(t *testing.T, opts HTTPOptions) http.Handler {
	t.Helper()
	s := New("hecate", "test", "")
	if err := s.Register(Tool{
		Name:        "gates",
		Description: "list gates",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Handler: func(context.Context, json.RawMessage) (any, string, error) {
			return map[string]any{"gates": []string{"production"}}, "production", nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	h, err := s.Handler(opts)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func post(t *testing.T, h http.Handler, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHTTPRefusesToServeWithNoAuthenticationDecision(t *testing.T) {
	s := New("hecate", "test", "")

	_, err := s.Handler(HTTPOptions{})

	// "No authentication" is a decision, and reaching it by forgetting to set a
	// field is not. This server exposes tools that promote and abort — getting
	// it wrong quietly is an open write API.
	if err == nil {
		t.Fatal("built an HTTP handler with neither Authenticate nor AllowUnauthenticated")
	}
	if !strings.Contains(err.Error(), "AllowUnauthenticated") {
		t.Errorf("the refusal %q does not say how to say the exposure is deliberate", err)
	}
}

func TestHTTPAnswersATool(t *testing.T) {
	h := httpServer(t, HTTPOptions{AllowUnauthenticated: true})

	rec := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("tools/list returned %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type is %q, want application/json", ct)
	}
	if !strings.Contains(rec.Body.String(), "gates") {
		t.Errorf("the tool is missing from %s", rec.Body.String())
	}
}

func TestHTTPAcceptsANotificationWithoutAnswering(t *testing.T) {
	h := httpServer(t, HTTPOptions{AllowUnauthenticated: true})

	rec := post(t, h, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil)

	// Answering a notification is a protocol violation, not merely noise. 202
	// says the message was accepted without claiming an answer follows.
	if rec.Code != http.StatusAccepted {
		t.Fatalf("a notification returned %d, want 202", rec.Code)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "" {
		t.Errorf("a notification was answered with %q", body)
	}
}

func TestHTTPReadsTheVersionFromTheHeader(t *testing.T) {
	h := httpServer(t, HTTPOptions{AllowUnauthenticated: true})

	rec := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"server/discover"}`,
		map[string]string{ProtocolVersionHeader: VersionModern})

	if rec.Code != http.StatusOK {
		t.Fatalf("discover returned %d: %s", rec.Code, rec.Body.String())
	}
	// On stdio the version travels in _meta; this transport moves it to a
	// header. A server reading only _meta would treat a client following the
	// HTTP specification as legacy, which is the interoperability failure this
	// transport is most likely to produce.
	if got := rec.Header().Get(ProtocolVersionHeader); got != VersionModern {
		t.Errorf("the version was not echoed: %q", got)
	}
}

func TestHTTPActsOnTheHeaderRatherThanMerelyEchoingIt(t *testing.T) {
	h := httpServer(t, HTTPOptions{AllowUnauthenticated: true})

	// A version this server does not speak, in the header and nowhere else. If
	// the header reaches the dispatch it is refused; if it is only echoed back
	// the request succeeds and the transport has quietly ignored what the
	// client said it was.
	//
	// Asserted here because the echo test cannot see the difference — it passes
	// either way, which is how this gap survived a mutation.
	rec := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"server/discover"}`,
		map[string]string{ProtocolVersionHeader: "1999-01-01"})

	if !strings.Contains(rec.Body.String(), "Unsupported protocol version") {
		t.Errorf("an unsupported version in the header was not acted on: %s", rec.Body.String())
	}
}

func TestHTTPLetsTheBodyOverrideTheHeader(t *testing.T) {
	h := httpServer(t, HTTPOptions{AllowUnauthenticated: true})

	// A version in the body the server speaks, and a header naming one it does
	// not. If the header won, this would be refused as unsupported.
	body := `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"` +
		metaProtocolVersion + `":"` + VersionModern + `"}}}`
	rec := post(t, h, body, map[string]string{ProtocolVersionHeader: "1999-01-01"})

	if rec.Code != http.StatusOK {
		t.Fatalf("discover returned %d: %s", rec.Code, rec.Body.String())
	}
	// A client that declared a version in the body meant it. Overwriting it
	// would make the header a way to change what a message says.
	if strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("the body's own version was overridden by the header: %s", rec.Body.String())
	}
}

func TestHTTPRefusesAnUnknownOrigin(t *testing.T) {
	h := httpServer(t, HTTPOptions{
		AllowUnauthenticated: true,
		AllowedOrigins:       []string{"http://localhost:3000"},
	})

	rec := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		map[string]string{"Origin": "https://evil.example"})

	// A local server with no Origin check can be driven by any page the user
	// visits, which is DNS rebinding with extra steps.
	if rec.Code != http.StatusForbidden {
		t.Fatalf("an unknown origin returned %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"error"`) {
		t.Error("the refusal carries no JSON-RPC error for the client to read")
	}
}

func TestHTTPAllowsAConfiguredOrigin(t *testing.T) {
	h := httpServer(t, HTTPOptions{
		AllowUnauthenticated: true,
		// Cased and with a trailing slash, because a configured value is typed
		// by a person: an allowlist that never matches is worse than one that
		// refuses, since nothing says why.
		AllowedOrigins: []string{"HTTP://LocalHost:3000/"},
	})

	rec := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		map[string]string{"Origin": "http://localhost:3000"})

	if rec.Code != http.StatusOK {
		t.Fatalf("a configured origin returned %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Errorf("CORS header is %q", got)
	}
}

func TestHTTPIgnoresOriginWhenThereIsNone(t *testing.T) {
	h := httpServer(t, HTTPOptions{AllowUnauthenticated: true})

	rec := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)

	// An empty allowlist rejects every browser and affects nothing else: a
	// client that is not a browser sends no Origin, and refusing it would make
	// the safe default unusable.
	if rec.Code != http.StatusOK {
		t.Fatalf("a request with no Origin returned %d", rec.Code)
	}
}

func TestHTTPRefusesAnUnauthenticatedCaller(t *testing.T) {
	h := httpServer(t, HTTPOptions{
		Authenticate: func(r *http.Request) error {
			if r.Header.Get("Authorization") != "Bearer good" {
				return errors.New("not signed in")
			}
			return nil
		},
	})

	rec := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated call returned %d, want 401", rec.Code)
	}
	// What to present, which is the difference between "sign in" and
	// "something is broken".
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Error("no WWW-Authenticate header")
	}

	ok := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		map[string]string{"Authorization": "Bearer good"})
	if ok.Code != http.StatusOK {
		t.Fatalf("an authenticated call returned %d: %s", ok.Code, ok.Body.String())
	}
}

func TestHTTPChecksTheOriginBeforeTheCredential(t *testing.T) {
	called := false
	h := httpServer(t, HTTPOptions{
		Authenticate:   func(*http.Request) error { called = true; return nil },
		AllowedOrigins: []string{"http://localhost:3000"},
	})

	post(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		map[string]string{"Origin": "https://evil.example"})

	// The point of the Origin check is to stop a page the user did not mean to
	// trust from spending their credentials — and a browser attaches those on
	// its own, so checking the credential first would authenticate the request
	// it exists to refuse.
	if called {
		t.Error("the credential was checked for a request from a refused origin")
	}
}

func TestHTTPSaysItHasNoServerToClientStream(t *testing.T) {
	h := httpServer(t, HTTPOptions{AllowUnauthenticated: true})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET returned %d, want 405", rec.Code)
	}
	// A client trying the GET stream some servers offer learns why rather than
	// guessing from a bare 405.
	if !strings.Contains(rec.Body.String(), "POST each request") {
		t.Errorf("the refusal does not say what to do instead: %s", rec.Body.String())
	}
}

func TestHTTPRefusesAMalformedOriginAtConstruction(t *testing.T) {
	s := New("hecate", "test", "")

	_, err := s.Handler(HTTPOptions{AllowUnauthenticated: true, AllowedOrigins: []string{"localhost:3000"}})

	// No scheme, so it is a host and not an origin. Caught when the server is
	// built rather than when the first browser is turned away.
	if err == nil {
		t.Fatal("a bare host was accepted as an origin")
	}
}
