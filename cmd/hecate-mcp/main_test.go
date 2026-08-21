package main

import (
	"context"
	"strings"
	"testing"

	"github.com/olafkfreund/hecate/pkg/mcp"
)

func TestLoopbackIsRecognised(t *testing.T) {
	for _, host := range []string{"localhost", "127.0.0.1", "::1"} {
		if !isLoopback(host) {
			t.Errorf("%q was not recognised as loopback", host)
		}
	}
	for _, host := range []string{"0.0.0.0", "10.0.0.1", "example.com"} {
		if isLoopback(host) {
			t.Errorf("%q was treated as loopback", host)
		}
	}
	// The one people type by accident: ":8085" splits to an empty host, which
	// means every interface and looks local.
	if isLoopback("") {
		t.Error(`an empty host — ":8085" — was treated as loopback`)
	}
}

func TestHTTPRefusesToBindTheWorldWithoutBeingTold(t *testing.T) {
	// An already-cancelled context, so a serveHTTP that wrongly reaches
	// ListenAndServe shuts itself down and returns instead of blocking. Without
	// it this test hangs rather than fails when the guard is removed, and a
	// hanging test reports nothing — it is caught by a timeout, somewhere else,
	// as an infrastructure problem.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := serveHTTP(ctx, mcp.New("hecate", "test", ""),
		runOptions{listen: "0.0.0.0:8085", allowWrites: true, actor: "ada"}, "demo", "writes enabled")

	// An MCP server with writes on, reachable from a network and with no
	// authentication, is a promote button with no password on it.
	if err == nil {
		t.Fatal("bound a non-loopback address with no authentication and no --insecure")
	}
	if !strings.Contains(err.Error(), "--insecure") {
		t.Errorf("the refusal %q does not say how to say the exposure is deliberate", err)
	}
}

func TestHTTPRefusesAnAddressThatIsNotHostPort(t *testing.T) {
	err := serveHTTP(t.Context(), mcp.New("hecate", "test", ""),
		runOptions{listen: "8085"}, "demo", "read-only")

	if err == nil || !strings.Contains(err.Error(), "host:port") {
		t.Errorf("a bare port was accepted or misreported: %v", err)
	}
}
