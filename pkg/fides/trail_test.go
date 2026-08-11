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
			"held on a failing control",
			`{"recommendation":"hold","approved":false,"risk_score":45,"risk_level":"medium","failed":["CC7.2"],"missing_evidence":["sbom"]}`,
			true, []string{"failing control CC7.2", "missing evidence sbom"},
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
