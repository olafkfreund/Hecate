package api

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
)

// frames reads SSE frames off a live stream.
//
// One goroutine for the whole stream, started once: a helper that starts a
// reader per call leaves the previous one blocked on the same reader, and it
// wakes to consume the next frame and post it to a channel nobody is listening
// to. The frame is then lost, and the test reports the server never sent it.
type frames struct {
	ch   <-chan string
	stop func()
}

func newFrames(body *bufio.Reader) *frames {
	out := make(chan string)
	go func() {
		defer close(out)
		var current strings.Builder
		for {
			line, err := body.ReadString('\n')
			if err != nil {
				return
			}
			if line == "\n" {
				if current.Len() > 0 {
					out <- current.String()
					current.Reset()
				}
				continue
			}
			current.WriteString(line)
		}
	}()
	return &frames{ch: out}
}

// next waits for one frame, returning "" if none arrives in time.
func (f *frames) next(within time.Duration) string {
	select {
	case frame, ok := <-f.ch:
		if !ok {
			return ""
		}
		return frame
	case <-time.After(within):
		return ""
	}
}

// liveStream opens the watch endpoint against a real listener, because the
// point of the handler is what it writes over time and httptest.ResponseRecorder
// has nothing to flush to.
func liveStream(t *testing.T, s *Server, token, namespace string) (*frames, func()) {
	t.Helper()
	srv := httptest.NewServer(s.Handler())

	req, err := http.NewRequest(http.MethodGet,
		srv.URL+"/api/v1alpha1/namespaces/"+namespace+"/watch", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // closed by the returned func
	if err != nil {
		srv.Close()
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		srv.Close()
		t.Fatalf("watch returned %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		resp.Body.Close()
		srv.Close()
		t.Fatalf("Content-Type is %q, want text/event-stream", ct)
	}
	return newFrames(bufio.NewReader(resp.Body)), func() {
		resp.Body.Close()
		srv.Close()
	}
}

func TestWatchAnnouncesItselfThenReportsChanges(t *testing.T) {
	s, _ := newServer(t,
		map[string]string{"tok": "ada"},
		grants{"ada": {"list gates": true}},
		gate("production"),
	)

	body, stop := liveStream(t, s, "tok", "acme")
	defer stop()

	// The first frame arrives immediately. A client that has just connected
	// needs to know the stream is live rather than merely quiet.
	first := body.next(3 * time.Second)
	if !strings.Contains(first, "event: open") {
		t.Errorf("first frame is %q, want an open event", first)
	}
	if !strings.Contains(first, `"acme"`) {
		t.Errorf("first frame %q does not name the namespace", first)
	}

	// Change something. The next frame must say so.
	ctx := context.Background()
	g := gate("staging")
	if err := s.Ops.Client.Create(ctx, g); err != nil {
		t.Fatal(err)
	}

	next := body.next(4 * tick)
	if !strings.Contains(next, "event: changed") {
		t.Errorf("frame after a change is %q, want a changed event", next)
	}
}

func TestWatchSaysNothingWhileNothingChanges(t *testing.T) {
	s, _ := newServer(t,
		map[string]string{"tok": "ada"},
		grants{"ada": {"list gates": true}},
		gate("production"),
	)

	body, stop := liveStream(t, s, "tok", "acme")
	defer stop()

	if open := body.next(3 * time.Second); !strings.Contains(open, "event: open") {
		t.Fatalf("no open frame on connect, got %q", open)
	}

	// A stream that re-announced every tick would cost every open tab a refetch
	// three times a second for a namespace where nothing is happening.
	if extra := body.next(3 * tick); extra != "" {
		t.Errorf("got %q with nothing changing, want silence", extra)
	}
}

func TestWatchRefusesWhatTheClusterRefuses(t *testing.T) {
	s, log := newServer(t,
		map[string]string{"tok": "ada"},
		grants{"ada": {}}, // may read nothing
		gate("production"),
	)

	// A bounded context, not the plain call helper: a recorder never
	// disconnects, so a stream that reached the loop would run for its whole
	// lifetime and the test would hang rather than fail. With a deadline, a
	// handler that streams when it should have refused ends in two seconds and
	// reports the wrong status, which is the answer this test is asking for.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1alpha1/namespaces/acme/watch", nil).
		WithContext(ctx)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("watch returned %d, want 403", rec.Code)
	}
	// The stream is a read of the namespace and must be authorised as one. A
	// stream that asked a different question would be a way around the answer.
	want := authorizationv1.ResourceAttributes{
		Namespace: "acme", Verb: "list", Group: "hecate.dev", Resource: "gates",
	}
	if got := log.last().ResourceAttributes; got == nil || *got != want {
		t.Errorf("authorised %+v, want %+v", got, want)
	}
}

func TestWatchNeedsAuthentication(t *testing.T) {
	s, _ := newServer(t, map[string]string{"tok": "ada"}, grants{"ada": {"list gates": true}})

	rec := call(t, s, "", http.MethodGet, "/api/v1alpha1/namespaces/acme/watch", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("watch returned %d, want 401", rec.Code)
	}
}

func TestFingerprintChangesWithTheNamespace(t *testing.T) {
	s, _ := newServer(t,
		map[string]string{"tok": "ada"},
		grants{"ada": {"list gates": true}},
		gate("production"), bundle("app-1"),
	)
	ctx := context.Background()

	before, err := s.fingerprint(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if same, err := s.fingerprint(ctx, "acme"); err != nil || same != before {
		t.Fatalf("fingerprint moved on its own: %q then %q (%v)", before, same, err)
	}

	// A deletion is a change a page must not miss — a Bundle that vanished from
	// under an operator is exactly what the list on screen is now wrong about.
	if err := s.Ops.Client.Delete(ctx, bundle("app-1")); err != nil {
		t.Fatal(err)
	}
	after, err := s.fingerprint(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Errorf("fingerprint %q unchanged after a delete", after)
	}
}

func TestFingerprintIsPerNamespace(t *testing.T) {
	s, _ := newServer(t,
		map[string]string{"tok": "ada"},
		grants{"ada": {"list gates": true}},
		gate("production"),
	)
	ctx := context.Background()

	acme, err := s.fingerprint(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.fingerprint(ctx, "somewhere-else")
	if err != nil {
		t.Fatal(err)
	}
	// Otherwise every tab in every namespace refetches whenever anything
	// anywhere in the cluster moves.
	if acme == other {
		t.Errorf("both namespaces fingerprint as %q", acme)
	}
}
