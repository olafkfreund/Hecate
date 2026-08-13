package fides

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeFides records what Hecate wrote to it.
type fakeFides struct {
	t       *testing.T
	attests []map[string]any
}

func (f *fakeFides) start(t *testing.T) *Client {
	t.Helper()
	f.t = t
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)

	c, err := New(Config{BaseURL: srv.URL, Token: "k"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (f *fakeFides) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.EscapedPath()
	if r.Method == http.MethodPost && path == "/api/v1/attestations" {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		f.attests = append(f.attests, m)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
		return
	}
	f.t.Errorf("unexpected %s %s", r.Method, path)
	w.WriteHeader(http.StatusNotFound)
}

// The trail a crossing gates on is the one CI already built the image on, and
// it is found by digest.
func TestTrailForArtifact(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
			{"sha256":"0000","trail_id":"other-trail"},
			{"sha256":"ABCDEF","trail_id":"91b2ffff-0000-4000-8000-00000000abcd"},
			{"sha256":"1111","trail_id":null}]`))
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{BaseURL: srv.URL, Token: "k"})
	if err != nil {
		t.Fatal(err)
	}

	// Prefixed or bare, and Fides stores the hex in whichever case it was given.
	for _, digest := range []string{"sha256:abcdef", "abcdef", "ABCDEF"} {
		got, err := c.TrailForArtifact(context.Background(), digest)
		if err != nil {
			t.Fatal(err)
		}
		if got != "91b2ffff-0000-4000-8000-00000000abcd" {
			t.Errorf("%s: trail = %q", digest, got)
		}
	}

	// An artifact Fides has never seen is not an error. CI did not register it,
	// and whether that disqualifies the crossing is the caller's decision.
	got, err := c.TrailForArtifact(context.Background(), "sha256:deadbeef")
	if err != nil {
		t.Fatalf("an unknown digest should not be an error: %v", err)
	}
	if got != "" {
		t.Errorf("trail = %q, want empty", got)
	}

	// An artifact recorded with no trail is the same answer.
	if got, _ := c.TrailForArtifact(context.Background(), "1111"); got != "" {
		t.Errorf("trail = %q, want empty for an artifact with no trail", got)
	}
}

func TestTrailForArtifactValidates(t *testing.T) {
	c := (&fakeFides{}).start(t)
	if _, err := c.TrailForArtifact(context.Background(), " "); err == nil {
		t.Error("no digest should be refused before calling")
	}
}

func TestAttest(t *testing.T) {
	f := &fakeFides{}
	c := f.start(t)

	err := c.Attest(context.Background(), "trail-1", Attestation{
		Name: "promotion", Type: "deployment",
		ArtifactSHA256: "sha256:abcdef",
		Payload:        map[string]any{"gate": "production", "bundle": "podinfo-abc123"},
	})
	if err != nil {
		t.Fatal(err)
	}

	got := f.attests[0]
	// The type is what an environment policy asks for by name; getting it wrong
	// means a policy requiring "deployment" never sees one.
	if got["type_name"] != "deployment" {
		t.Errorf("type_name = %v", got["type_name"])
	}
	// Fides stores the bare hex.
	if got["artifact_sha256"] != "abcdef" {
		t.Errorf("artifact_sha256 = %v, want the sha256: prefix stripped", got["artifact_sha256"])
	}
	// The payload is a JSON *string*: Fides stores it as text and hashes the
	// canonical form into the chain.
	payload, ok := got["payload"].(string)
	if !ok {
		t.Fatalf("payload = %T, want a JSON string", got["payload"])
	}
	if !strings.Contains(payload, "podinfo-abc123") {
		t.Errorf("payload = %s", payload)
	}
	if got["signed_by"] != "hecate" {
		t.Errorf("signed_by = %v", got["signed_by"])
	}
}

func TestAttestValidates(t *testing.T) {
	c := (&fakeFides{}).start(t)
	ctx := context.Background()

	if err := c.Attest(ctx, "", Attestation{Name: "n", Type: "t"}); err == nil {
		t.Error("attesting to no trail should be refused")
	}
	if err := c.Attest(ctx, "trail", Attestation{Name: "n"}); err == nil {
		t.Error("an attestation with no type should be refused — a policy asks for it by name")
	}
}

func TestAssert(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"compliant":false,"violations":["no SBOM","critical CVE"]}`))
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{BaseURL: srv.URL, Token: "k"})
	if err != nil {
		t.Fatal(err)
	}

	out, err := c.Assert(context.Background(), "sha256:abcdef", "production-baseline")
	if err != nil {
		t.Fatal(err)
	}
	if out.Compliant || len(out.Violations) != 2 {
		t.Errorf("compliance = %+v", out)
	}
	if !strings.Contains(query, "sha256=abcdef") || !strings.Contains(query, "policy=production-baseline") {
		t.Errorf("query = %q", query)
	}
}

func TestChangeGate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		held     bool
		blockers []string
	}{
		{
			"approved",
			`{"recommendation":"approve","approved":true,"risk_score":5,"risk_level":"low"}`,
			false, nil,
		},
		{
			// The body is Fides' own, key for key: `failed` and
			// `missing_evidence` are arrays of objects while `passed` is an
			// array of strings. Hecate declared all three as []string, which
			// decodes fine against a hand-written fake and errors against the
			// real server — and only ever on a held verdict, the one case the
			// change gate exists for. Copied from evidance-vault's
			// computeChangeGate rather than written from memory.
			"held on a failing control",
			`{"trail_id":"t-1","recommendation":"hold","approved":false,"risk_score":45,` +
				`"risk_level":"medium","passed":["CC8.1"],` +
				`"failed":[{"control":"CC7.2","name":"Vulnerability scanning","reasons":["failed vuln-scan"]}],` +
				`"missing_evidence":[{"control":"CC6.1","name":"Change approval","reasons":["missing sbom"]}],` +
				`"waived":[],"attestations":{"total":3,"non_compliant":1},` +
				`"approvals":{"count":1,"human_approvers":1,"four_eyes":false,"approvers":["a@x"],"deployers":[]},` +
				`"segregation_of_duties":{"committer":"a@x","approvers":["a@x"],"deployers":[],` +
				`"compliant":false,"violations":["committer and approver are the same person"]},` +
				`"summary":"Controls failed."}`,
			true, []string{
				"failing control CC7.2 Vulnerability scanning (failed vuln-scan)",
				"missing evidence for CC6.1 Change approval (missing sbom)",
			},
		},
		{
			// Every control satisfied and still held: segregation of duties
			// working, not something broken. The message has to say so or an
			// operator goes hunting for a failure that does not exist.
			"held awaiting a human",
			`{"recommendation":"hold","approved":false,"risk_score":20,"risk_level":"medium"}`,
			true, []string{"awaiting a human approval"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)
			c, err := New(Config{BaseURL: srv.URL, Token: "k"})
			if err != nil {
				t.Fatal(err)
			}

			verdict, err := c.ChangeGate(context.Background(), "trail-1")
			if err != nil {
				t.Fatal(err)
			}
			if verdict.Held() != tc.held {
				t.Errorf("held = %v, want %v", verdict.Held(), tc.held)
			}
			if got := strings.Join(verdict.Blockers(), "; "); got != strings.Join(tc.blockers, "; ") {
				t.Errorf("blockers = %q, want %q", got, strings.Join(tc.blockers, "; "))
			}
		})
	}
}

// Reporting an artifact links a Bundle's images to the trail CI recorded
// against them — the thing that gives a change gate something to read.
func TestReportArtifact(t *testing.T) {
	var got map[string]any
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c, err := New(Config{BaseURL: srv.URL, Token: "k"})
	if err != nil {
		t.Fatal(err)
	}
	err = c.ReportArtifact(context.Background(), Artifact{
		SHA256: "sha256:4a6f31e7c48b0fb7f3848479c9278284362ca590ee8ee06a377971f2af22464b",
		Trail:  "aaaa1111-0000-0000-0000-000000000000",
		Name:   "ghcr.io/acme/podinfo",
		Type:   "container-image",
	})
	if err != nil {
		t.Fatal(err)
	}

	if path != "/api/v1/artifacts" {
		t.Errorf("path = %q", path)
	}
	// Lowercase hex, no prefix: Fides keys artifacts that way, and a digest
	// sent with the prefix simply matches nothing.
	if got["sha256"] != "4a6f31e7c48b0fb7f3848479c9278284362ca590ee8ee06a377971f2af22464b" {
		t.Errorf("sha256 = %v, want the prefix stripped", got["sha256"])
	}
	if got["trail_id"] != "aaaa1111-0000-0000-0000-000000000000" {
		t.Errorf("trail_id = %v", got["trail_id"])
	}
}

// The safety rule. Fides upserts on the digest and overwrites trail_id with
// whatever it is sent, so reporting without a trail would detach the SBOM and
// the scans CI attached — the exact evidence the change gate exists to read.
func TestReportArtifactRefusesToDetachEvidence(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c, err := New(Config{BaseURL: srv.URL, Token: "k"})
	if err != nil {
		t.Fatal(err)
	}
	err = c.ReportArtifact(context.Background(), Artifact{SHA256: "abc", Name: "x"})

	if err == nil {
		t.Fatal("want a refusal when there is no trail to link to")
	}
	if !strings.Contains(err.Error(), "detaching the evidence") {
		t.Errorf("err = %v, want it to say what the danger is", err)
	}
	if called {
		t.Error("the request was sent anyway — Fides would have nulled the link")
	}
}

// Fides attributes an approval to the calling token unless told otherwise, so
// an approval with no human is one every promotion shares. The client refuses
// rather than sending it: a caller that lost track of the actor gets an error,
// not a compliance record naming a service account as the approver.
func TestRecordApprovalNeedsAHumanARoleAndATrail(t *testing.T) {
	var posted int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posted++
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	c, err := New(Config{BaseURL: srv.URL, Token: "k"})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		trail string
		a     Approval
	}{
		{"no human", "aaaa1111-0000-0000-0000-000000000000", Approval{Role: RoleApprover}},
		{"blank human", "aaaa1111-0000-0000-0000-000000000000", Approval{By: "  ", Role: RoleApprover}},
		{"no trail", "", Approval{By: "olaf@acme.example", Role: RoleApprover}},
		{"no role", "aaaa1111-0000-0000-0000-000000000000", Approval{By: "olaf@acme.example"}},
		{"invented role", "aaaa1111-0000-0000-0000-000000000000", Approval{By: "olaf@acme.example", Role: "owner"}},
	} {
		if err := c.RecordApproval(context.Background(), tc.trail, tc.a); err == nil {
			t.Errorf("%s: recorded without complaint", tc.name)
		}
	}
	if posted != 0 {
		t.Errorf("sent %d of those to Fides — each one is a compliance record that names "+
			"the wrong identity or none", posted)
	}
}

func TestRecordApprovalSendsTheApproval(t *testing.T) {
	var got map[string]any
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.EscapedPath()
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	c, err := New(Config{BaseURL: srv.URL, Token: "k"})
	if err != nil {
		t.Fatal(err)
	}

	err = c.RecordApproval(context.Background(), "aaaa1111-0000-0000-0000-000000000000", Approval{
		By: "olaf@acme.example", Role: RoleDeployer, Reason: "crossing production",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(path, "aaaa1111-0000-0000-0000-000000000000") {
		t.Errorf("posted to %q, want the trail's approvals", path)
	}
	if got["on_behalf_of"] != "olaf@acme.example" || got["role"] != RoleDeployer {
		t.Errorf("body = %v", got)
	}
}
