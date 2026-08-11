package fides

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// server answers with bodies copied from Fides' own handlers, so a change to
// its response shape fails here rather than silently reading as "not compliant".
type server struct {
	t      *testing.T
	status int
	body   string
	path   string
	query  string
	auth   string
}

func (s *server) start(t *testing.T) *Client {
	t.Helper()
	s.t = t
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.path, s.query, s.auth = r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Get("Authorization")
		if s.status != 0 && s.status >= 300 {
			w.WriteHeader(s.status)
		}
		_, _ = w.Write([]byte(s.body))
	}))
	t.Cleanup(srv.Close)

	c, err := New(Config{BaseURL: srv.URL, Token: "fides-key"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const (
	validChain  = `{"valid":true,"count":14,"broken_at":-1,"external_anchor":{"anchored":true,"head_matches":true,"tsa_url":"https://freetsa.org/tsr","anchored_at":"2026-08-09T11:02:00Z"}}`
	brokenChain = `{"valid":false,"count":9,"broken_at":6,"reason":"content_hash does not match the recorded entry (tampering)"}`
)

func TestVerifyChain(t *testing.T) {
	s := &server{body: validChain}
	c := s.start(t)

	chain, err := c.VerifyChain(context.Background(), "7f3a1c2e-0000-4000-8000-000000009b04")
	if err != nil {
		t.Fatal(err)
	}

	if !chain.Valid || chain.Count != 14 || chain.BrokenAt != -1 {
		t.Errorf("chain = %+v", chain)
	}
	if chain.ExternalAnchor == nil || !chain.ExternalAnchor.HeadMatches {
		t.Errorf("the external anchor was dropped: %+v", chain.ExternalAnchor)
	}
	if s.path != "/api/v1/trails/7f3a1c2e-0000-4000-8000-000000009b04/verify-chain" {
		t.Errorf("path = %s", s.path)
	}
	if s.auth != "Bearer fides-key" {
		t.Errorf("Authorization = %q", s.auth)
	}
}

// The whole point of the endpoint. A broken chain must arrive as a broken
// chain, not as an error or a zero value.
func TestVerifyChainReportsTampering(t *testing.T) {
	c := (&server{body: brokenChain}).start(t)

	chain, err := c.VerifyChain(context.Background(), "trail")
	if err != nil {
		t.Fatal(err)
	}
	if chain.Valid {
		t.Fatal("a tampered chain reported valid")
	}
	if chain.BrokenAt != 6 || !strings.Contains(chain.Reason, "tampering") {
		t.Errorf("chain = %+v — the verdict must say where and why", chain)
	}
}

func TestPolicyCheck(t *testing.T) {
	// Copied from handlePolicyCheck: `compliant`, not `passed`.
	s := &server{body: `{"compliant":false,"trail_id":"91b2","results":[
		{"policy":"production-requires-sbom","applies":true,"missing":["sbom","vulnerability-scan"]},
		{"policy":"eu-ai-act","applies":false,"missing":[]},
		{"policy":"signed-image","applies":true,"missing":[]}]}`}
	c := s.start(t)

	verdict, err := c.PolicyCheck(context.Background(), "env-uuid", "trail-uuid")
	if err != nil {
		t.Fatal(err)
	}

	if verdict.Compliant {
		t.Error("compliant = true, but a policy is missing attestations")
	}
	unmet := verdict.Unmet()
	if len(unmet) != 1 || !strings.Contains(unmet[0], "production-requires-sbom") {
		t.Errorf("unmet = %v — a refusal has to name the policy that refused", unmet)
	}
	// A policy that did not apply is not a policy that refused.
	if strings.Contains(strings.Join(unmet, " "), "eu-ai-act") {
		t.Error("an inapplicable policy was reported as unmet")
	}
	if !strings.Contains(s.path, "/api/v1/environments/env-uuid/policy-check") {
		t.Errorf("path = %s", s.path)
	}
	// The trail is a query parameter, and the endpoint 400s without it.
	if s.query != "trail=trail-uuid" {
		t.Errorf("query = %q", s.query)
	}
}

func TestAllowlisted(t *testing.T) {
	s := &server{body: `{"environment_id":"env","artifact_sha256":"abc","approved":true}`}
	c := s.start(t)

	approved, err := c.Allowlisted(context.Background(), "env", "sha256:abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if !approved {
		t.Error("approved = false")
	}
	// Fides stores the bare hex; sending the prefixed digest matches nothing.
	if s.query != "sha=abcdef" {
		t.Errorf("query = %q, want the sha256: prefix stripped", s.query)
	}
}

func TestErrorsAreClassifiable(t *testing.T) {
	notFound := (&server{status: http.StatusNotFound, body: "trail not found"}).start(t)
	if _, err := notFound.VerifyChain(context.Background(), "nope"); !IsNotFound(err) {
		t.Errorf("a missing trail should be recognisable: %v", err)
	}

	unauthorized := (&server{status: http.StatusUnauthorized, body: "unauthorized"}).start(t)
	_, err := unauthorized.VerifyChain(context.Background(), "trail")
	if !IsAuth(err) {
		t.Errorf("a rejected token should be recognisable: %v", err)
	}
	if IsNotFound(err) {
		t.Error("a 401 is not a 404")
	}
	// The operator needs to know it was Fides that refused, and which call.
	if !strings.Contains(err.Error(), "fides:") || !strings.Contains(err.Error(), "verify-chain") {
		t.Errorf("error = %v", err)
	}
}

func TestNewRefusesUnusableConfig(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no server":   {Token: "t"},
		"no token":    {BaseURL: "https://fides.acme.io"},
		"no scheme":   {BaseURL: "fides.acme.io", Token: "t"},
		"bad scheme":  {BaseURL: "ftp://fides.acme.io", Token: "t"},
		"nothing set": {},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: expected a refusal", name)
		}
	}
}

func TestCallsRefuseMissingIdentifiers(t *testing.T) {
	c := (&server{body: "{}"}).start(t)
	ctx := context.Background()

	if _, err := c.VerifyChain(ctx, "  "); err == nil {
		t.Error("verify-chain with no trail should refuse before calling")
	}
	if _, err := c.PolicyCheck(ctx, "env", ""); err == nil {
		t.Error("a policy check with no trail would 400 — refuse it here")
	}
	if _, err := c.Allowlisted(ctx, "", "sha"); err == nil {
		t.Error("an allowlist check with no environment should refuse")
	}
}
