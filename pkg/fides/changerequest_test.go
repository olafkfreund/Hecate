package fides

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// changeFides answers the attestation search and the attestation read.
type changeFides struct {
	// list is the search body.
	list string
	// byID maps an attestation id to its detail body.
	byID map[string]string
	// asked records every path, so a test can assert which attestation was read
	// rather than only that one was.
	asked []string
}

func (f *changeFides) start(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.asked = append(f.asked, r.URL.EscapedPath()+"?"+r.URL.RawQuery)
		switch {
		case strings.Contains(r.URL.Path, "/search/attestations"):
			_, _ = w.Write([]byte(f.list))
		case strings.Contains(r.URL.Path, "/attestations/"):
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			body, ok := f.byID[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(body))
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{BaseURL: srv.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestChangeRequestReadsTheAttestedChange(t *testing.T) {
	f := &changeFides{
		list: `[{"id":"a1","type_name":"servicenow-change","is_compliant":true,"created_at":"2026-08-18T12:15:24Z"}]`,
		byID: map[string]string{"a1": `{"is_compliant":true,"created_at":"2026-08-18T12:15:24Z","payload":
			{"number":"CHG0033184","state":"new","approval":"not requested","risk":"3","on_hold":false,
			 "short_description":"Fides e2e","found":true}}`},
	}

	got, err := f.start(t).ChangeRequestFor(context.Background(), "trail-1")
	if err != nil {
		t.Fatal(err)
	}

	if got == nil {
		t.Fatal("no change returned")
	}
	if got.Number != "CHG0033184" || got.State != "new" || got.Approval != "not requested" {
		t.Errorf("got %+v", got)
	}
	// Fides' judgement of the evidence, which is not a field of the change.
	if !got.Compliant {
		t.Error("the attestation's compliance was dropped")
	}
	if got.At == "" {
		t.Error("no attestation time, so a fortnight-old check reads as fresh")
	}
}

func TestChangeRequestIsAbsentRatherThanAnError(t *testing.T) {
	f := &changeFides{list: `[]`}

	got, err := f.start(t).ChangeRequestFor(context.Background(), "trail-1")

	// Most trails carry no ITSM check. Reporting that as a failure would be
	// wrong about almost every crossing.
	if err != nil {
		t.Fatalf("an absent change returned an error: %v", err)
	}
	if got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}

func TestChangeRequestTakesTheNewestAttestation(t *testing.T) {
	// Deliberately oldest-first, which Fides does not send — but a caller that
	// depended on the order would be confidently wrong the day it changed, and
	// an older attestation names a change that has since moved on.
	f := &changeFides{
		list: `[{"id":"old","type_name":"servicenow-change","created_at":"2026-08-01T00:00:00Z"},
		        {"id":"new","type_name":"servicenow-change","created_at":"2026-08-18T00:00:00Z"}]`,
		byID: map[string]string{
			"old": `{"payload":{"number":"CHG0000001","state":"closed"}}`,
			"new": `{"payload":{"number":"CHG0033184","state":"new"}}`,
		},
	}

	got, err := f.start(t).ChangeRequestFor(context.Background(), "trail-1")
	if err != nil {
		t.Fatal(err)
	}

	if got.Number != "CHG0033184" {
		t.Errorf("read %s, want the newest attestation's change", got.Number)
	}
}

func TestChangeRequestAsksAboutTheTrailItWasGiven(t *testing.T) {
	f := &changeFides{
		list: `[{"id":"a1","type_name":"servicenow-change","created_at":"2026-08-18T00:00:00Z"}]`,
		byID: map[string]string{"a1": `{"payload":{"number":"CHG1"}}`},
	}

	if _, err := f.start(t).ChangeRequestFor(context.Background(), "trail-1"); err != nil {
		t.Fatal(err)
	}

	// Asking about the wrong trail answers plausibly and wrongly: another
	// crossing's change request, presented as this one's.
	if !strings.Contains(f.asked[0], "trail=trail-1") {
		t.Errorf("searched %q, which does not name the trail", f.asked[0])
	}
	if !strings.Contains(f.asked[0], "type=servicenow-change") {
		t.Errorf("searched %q, which does not filter to the ITSM check", f.asked[0])
	}
}

func TestChangeRequestNeedsATrail(t *testing.T) {
	f := &changeFides{list: `[]`}

	if _, err := f.start(t).ChangeRequestFor(context.Background(), ""); err == nil {
		t.Error("an empty trail was accepted, which would search every trail there is")
	}
}
