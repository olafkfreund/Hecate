package ops

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// changeGateFides answers everything Evidence asks for, including the ITSM
// check — the fake in approval_test.go deliberately has none.
type changeGateFides struct {
	change string // the attestation payload, or "" for no ITSM check
	// searchStatus fails only the ITSM lookup, leaving everything else working.
	searchStatus int
}

func (f *changeGateFides) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.EscapedPath()
		switch {
		case strings.Contains(path, "/search/attestations"):
			if f.searchStatus != 0 {
				w.WriteHeader(f.searchStatus)
				_, _ = w.Write([]byte("unavailable"))
				return
			}
			if f.change == "" {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[{"id":"a1","type_name":"servicenow-change","created_at":"2026-08-18T00:00:00Z"}]`))
		case strings.Contains(path, "/attestations/"):
			_, _ = w.Write([]byte(`{"is_compliant":true,"created_at":"2026-08-18T00:00:00Z","payload":` + f.change + `}`))
		case strings.HasSuffix(path, "/change-gate"):
			_, _ = w.Write([]byte(`{"recommendation":"hold","risk_score":40}`))
		case strings.HasSuffix(path, "/artifacts"):
			_, _ = w.Write([]byte(`[{"sha256":"` + strings.TrimPrefix(sodDigest, "sha256:") +
				`","trail_id":"` + sodTrail + `"}]`))
		default:
			t.Errorf("unexpected request to %s", path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestEvidenceCarriesTheChangeRequest(t *testing.T) {
	f := &changeGateFides{change: `{"number":"CHG0033184","state":"new","approval":"not requested","on_hold":false,"found":true}`}
	o, _ := evidenceOps(t, testGate("production", withEvidence(f.start(t))), bundleWithDigest("b1"), sodSecret())

	ev, err := o.Evidence(context.Background(), "acme", "b1")
	if err != nil {
		t.Fatal(err)
	}

	// The verdict says the crossing is held; this says which ticket has to move
	// for it not to be.
	if ev.Change == nil {
		t.Fatal("no change request on the evidence")
	}
	if ev.Change.Number != "CHG0033184" || ev.Change.Approval != "not requested" {
		t.Errorf("got %+v", ev.Change)
	}
	// And the verdict is still there — the change is carried alongside it.
	if ev.Verdict == nil || ev.Verdict.Recommendation != "hold" {
		t.Errorf("the verdict was lost: %+v", ev.Verdict)
	}
}

func TestEvidenceWithoutAnITSMCheckStillReportsTheVerdict(t *testing.T) {
	f := &changeGateFides{} // no ITSM check on this trail
	o, _ := evidenceOps(t, testGate("production", withEvidence(f.start(t))), bundleWithDigest("b1"), sodSecret())

	ev, err := o.Evidence(context.Background(), "acme", "b1")
	if err != nil {
		t.Fatal(err)
	}

	// Most crossings have no ITSM check, and an evidence panel that failed —
	// or lost its verdict — because one is absent would be wrong about almost
	// every crossing.
	if ev.Change != nil {
		t.Errorf("invented a change request: %+v", ev.Change)
	}
	if ev.Verdict == nil {
		t.Fatal("the verdict was dropped when there was no change to report")
	}
}

func TestEvidenceKeepsTheVerdictWhenTheChangeLookupFails(t *testing.T) {
	f := &changeGateFides{searchStatus: http.StatusInternalServerError}
	o, _ := evidenceOps(t, testGate("production", withEvidence(f.start(t))), bundleWithDigest("b1"), sodSecret())

	ev, err := o.Evidence(context.Background(), "acme", "b1")

	// The change is an addition to the answer, not the answer. Losing the whole
	// evidence panel because the ITSM lookup was unavailable would trade the
	// thing someone came for against a detail they may not even need.
	if err != nil {
		t.Fatalf("a failed change lookup failed the whole panel: %v", err)
	}
	if ev.Verdict == nil || ev.Verdict.Recommendation != "hold" {
		t.Errorf("the verdict was lost: %+v", ev.Verdict)
	}
	if ev.Change != nil {
		t.Errorf("reported a change it could not read: %+v", ev.Change)
	}
}
