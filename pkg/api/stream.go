package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// streamHandler is a handler that writes its own response over time, rather
// than returning a value to be marshalled once.
type streamHandler func(ctx context.Context, subject Subject, w http.ResponseWriter, r *http.Request)

// tick is how often the server looks for a change. Fides' console stream uses
// the same interval, and a promotion is a human-scale event — a second either
// way is not what makes this feel live, having to press reload is.
const tick = 3 * time.Second

// heartbeat keeps an idle connection from being reaped. Proxies and load
// balancers close connections that say nothing for long enough, and an SSE
// comment costs nothing and is ignored by every client.
const heartbeat = 20 * time.Second

// maxStreamLife bounds how long one connection may run.
//
// Authorisation is checked when the stream opens and not again, because a
// SubjectAccessReview every three seconds for every open tab is a poor trade.
// Closing the connection periodically is the cheap version of re-checking:
// EventSource reconnects on its own, and the new connection authenticates and
// authorises from scratch. Someone whose access is revoked keeps a stream that
// only says "something changed" for at most this long, and every refetch they
// make in the meantime is refused by the guarded handlers.
const maxStreamLife = 30 * time.Minute

// stream authenticates and authorises like guard, then hands the handler the
// raw ResponseWriter.
//
// Separate from guard rather than a flag on it because of the timeout: guard
// gives a request 30 seconds, which is correct for a read and fatal for a
// connection meant to stay open. A shared helper taking "but not the timeout"
// would be one function serving two opposite lifetimes.
func (s *Server) stream(action Action, h streamHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A short-lived context for the authorisation calls only — the stream
		// itself runs on the request context.
		authCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()

		subject, err := s.Auth.Authenticate(authCtx, r)
		if err != nil {
			if errors.Is(err, ErrUnauthenticated) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="hecate"`)
				writeError(w, http.StatusUnauthorized, "authentication required")
				return
			}
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		namespace := r.PathValue("namespace")
		if err := s.Auth.Authorize(authCtx, subject, action, namespace); err != nil {
			var forbidden *Forbidden
			if errors.As(err, &forbidden) {
				writeError(w, http.StatusForbidden, forbidden.Error())
				return
			}
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		h(r.Context(), subject, w, r)
	})
}

// streamAuthenticated is stream for a route with no namespace in its path.
//
// Authorising would mean asking "may you read cluster-wide?", which is the trap
// overview and the cluster-wide lists both avoid: a team-scoped operator has no
// reason to hold that right, and refusing them live updates on their own pages
// would be precisely backwards.
//
// Safe because of what the stream is. It carries a notification and never a
// resource, so the page refetches through the filtered endpoints and sees only
// what it may. What a caller can infer from this is "something, somewhere,
// changed" — no name, no namespace, no count — which is the same inference the
// existing per-namespace stream already grants for one namespace.
func (s *Server) streamAuthenticated(h streamHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()

		subject, err := s.Auth.Authenticate(authCtx, r)
		if err != nil {
			if errors.Is(err, ErrUnauthenticated) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="hecate"`)
				writeError(w, http.StatusUnauthorized, "authentication required")
				return
			}
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		h(r.Context(), subject, w, r)
	})
}

// watchNamespace tells a browser when something in a namespace has changed, so
// a page can reload itself instead of being reloaded by hand.
//
// It sends a notification, never the resources. The page refetches through the
// same guarded endpoints it already uses, which means authorisation is decided
// in one place rather than again here per event — and a caller whose access was
// revoked gets a 403 on the refetch rather than data over the stream.
//
// ponytail: one poll per open connection, so N tabs on the same namespace do N
// lists per tick. Fine for the handful of operators a namespace has; if this
// ever carries a wall display per team, share one poller per namespace and fan
// out to its subscribers.
func (s *Server) watchNamespace(ctx context.Context, _ Subject, w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "this server cannot stream")
		return
	}

	namespace := r.PathValue("namespace")

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	// Nginx buffers responses by default, which holds every event until the
	// buffer fills — a stream that arrives in bursts minutes apart is worse
	// than no stream, because it looks like it is working.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Told immediately rather than on the first change: a client that has just
	// connected needs to know the stream is live, and it is also how it learns
	// the state it should already be showing.
	last, err := s.fingerprint(ctx, namespace)
	if err != nil {
		// Not fatal. The fingerprint is a comparison, and failing to take the
		// first one only means the next tick reports a change that may not be
		// one — a spurious refetch, against a page that would otherwise never
		// update at all.
		last = ""
	}
	if !sendEvent(w, flusher, "open", namespace) {
		return
	}

	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	beat := time.NewTicker(heartbeat)
	defer beat.Stop()
	deadline := time.NewTimer(maxStreamLife)
	defer deadline.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-deadline.C:
			// EventSource reconnects on its own, and the new connection
			// re-authorises. Saying so first means a client that is not
			// EventSource knows this was not a failure.
			sendEvent(w, flusher, "reconnect", namespace)
			return

		case <-beat.C:
			// An SSE comment: ignored by clients, enough to keep a proxy from
			// deciding the connection is idle.
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()

		case <-ticker.C:
			current, err := s.fingerprint(ctx, namespace)
			if err != nil {
				// A transient read failure is not a change, and reporting it as
				// one would make every blip cost every open tab a refetch.
				continue
			}
			if current == last {
				continue
			}
			last = current
			if !sendEvent(w, flusher, "changed", namespace) {
				return
			}
		}
	}
}

// sendEvent writes one SSE frame, reporting whether the client is still there.
func sendEvent(w http.ResponseWriter, flusher http.Flusher, kind, namespace string) bool {
	if _, err := fmt.Fprintf(w, "event: %s\ndata: {\"namespace\":%q}\n\n", kind, namespace); err != nil {
		return false
	}
	flusher.Flush()
	return true
}

// fingerprint is a cheap summary of everything a page in this namespace shows.
//
// Built from resource versions rather than from the objects: Kubernetes changes
// the resource version on every write and on nothing else, so comparing them
// answers "has anything moved" without the server having to understand what
// moved or the client having to diff. Gates, Bundles and Passages together,
// because a crossing changes all three and a page showing one of them is stale
// when any of them changes.
func (s *Server) fingerprint(ctx context.Context, namespace string) (string, error) {
	var (
		gates    v1alpha1.GateList
		bundles  v1alpha1.BundleList
		passages v1alpha1.PassageList
		beacons  v1alpha1.BeaconList
	)
	in := client.InNamespace(namespace)
	if err := s.Ops.Client.List(ctx, &gates, in); err != nil {
		return "", err
	}
	if err := s.Ops.Client.List(ctx, &beacons, in); err != nil {
		return "", err
	}
	if err := s.Ops.Client.List(ctx, &bundles, in); err != nil {
		return "", err
	}
	if err := s.Ops.Client.List(ctx, &passages, in); err != nil {
		return "", err
	}

	// The list's own resource version moves when any member of it changes, so
	// four strings describe the whole namespace. Counts are included because a
	// deletion can leave the list version behind on some storage backends, and a
	// Bundle disappearing is exactly the kind of change a page must not miss.
	//
	// All four kinds, not only the three a Gate page reads: a Beacon that has
	// just looked at its sources is a change to the Beacons page, and a
	// fingerprint that ignored it would leave "check now" reporting nothing
	// happened when something had.
	return fmt.Sprintf("%s/%d.%s/%d.%s/%d.%s/%d",
		gates.ResourceVersion, len(gates.Items),
		bundles.ResourceVersion, len(bundles.Items),
		passages.ResourceVersion, len(passages.Items),
		beacons.ResourceVersion, len(beacons.Items),
	), nil
}
