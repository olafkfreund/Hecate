package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// Settings is what the settings screen shows.
//
// **Derived from cluster state, not from configuration.** The obvious
// alternative is to plumb the chart's values through to this process and report
// them, and it would be wrong: values say what someone intended, and a Gate
// says what is actually in force. When they disagree — a Gate overriding
// serverURL, a chart upgraded but not applied — the honest answer is the one
// the controller will act on.
type Settings struct {
	// Version is the running build, so a bug report can name it.
	Version string `json:"version"`
	// Identity is who the API thinks you are, straight from the token. The most
	// common sign-in complaint is "it says I cannot do this and I should be
	// able to", and the first useful question is which account you arrived as.
	Identity Identity `json:"identity"`
	// Fides is every evidence server the visible Gates point at, with whether
	// it answers.
	Fides []FidesTarget `json:"fides"`
	// Clusters are the remote clusters Gates watch (#22).
	Clusters []ClusterTarget `json:"clusters"`
	// Telemetry is where traces go, if anywhere.
	Telemetry Telemetry `json:"telemetry"`
}

// Identity is the authenticated caller.
type Identity struct {
	Name   string   `json:"name"`
	Groups []string `json:"groups,omitempty"`
}

// FidesTarget is one evidence server and the Gates that use it.
type FidesTarget struct {
	ServerURL string `json:"serverURL"`
	// Gates naming this server, as "namespace/name".
	Gates []string `json:"gates"`
	// Environments this server is asked about, as UUIDs — the same value that
	// appears in the Fides UI, so the two can be lined up.
	Environments []string `json:"environments,omitempty"`
	// Reachable is the result of actually asking, not of the URL looking
	// plausible. A misconfigured evidence server is indistinguishable from a
	// working one until a promotion needs it, which is the worst moment to find
	// out.
	Reachable bool `json:"reachable"`
	// Detail says why when Reachable is false.
	Detail string `json:"detail,omitempty"`
}

// ClusterTarget is a remote cluster this installation knows about.
type ClusterTarget struct {
	// Secret holding the kubeconfig, as "namespace/name".
	Secret string `json:"secret"`
	// Gates using it. Empty is not an error — a cluster can be connected before
	// any Gate references it, and saying so is more useful than hiding it,
	// which is what listing only Gate-referenced clusters used to do: you could
	// store a kubeconfig and watch nothing appear.
	Gates []string `json:"gates"`
	// Reachable is whether the credentials in the Secret actually answer.
	//
	// Checked here rather than left until a promotion needs it. A kubeconfig
	// that has expired, or names an endpoint this cluster cannot route to,
	// looks identical to a working one right up to the moment a Gate is waiting
	// on it — and that is the moment when nobody wants to be debugging
	// credentials.
	Reachable bool `json:"reachable"`
	// Detail says why when Reachable is false.
	Detail string `json:"detail,omitempty"`
}

// Telemetry is the OpenTelemetry export configuration.
//
// Hecate exports spans and stores none, so there is nothing here to browse.
// Reporting where they go is the useful thing a settings page can do: it turns
// "is tracing on?" into a question with an answer, and gives the address to
// open the collector's own UI at.
type Telemetry struct {
	Endpoint   string `json:"endpoint,omitempty"`
	Configured bool   `json:"configured"`
}

// settings assembles the screen.
func (s *Server) settings(ctx context.Context, subject Subject, _ *http.Request) (any, error) {
	out := Settings{
		Version: s.Version,
		// A conversion, not a copy: Identity exists to give the JSON its own
		// shape and has the same fields, so staticcheck is right that spelling
		// them out invites the two drifting apart.
		Identity: Identity(subject),
		Fides:    []FidesTarget{},
		Clusters: []ClusterTarget{},
	}

	// The OTEL_ variables are the standard ones the SDK reads, so this reports
	// what the exporter is actually using rather than a Hecate-specific copy of
	// it that could drift.
	if ep := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); ep != "" {
		out.Telemetry = Telemetry{Endpoint: ep, Configured: true}
	}

	namespaces, err := s.Ops.Namespaces(ctx)
	if err != nil {
		return nil, err
	}

	byServer := map[string]*FidesTarget{}
	byCluster := map[string]*ClusterTarget{}

	for _, ns := range namespaces {
		// Only namespaces this caller may read. Settings is a read of the same
		// objects the rest of the API guards, and a screen that leaks another
		// team's evidence server because it is "just configuration" is still a
		// leak.
		if err := s.Auth.Authorize(ctx, subject, ActionRead, ns); err != nil {
			continue
		}
		gates, err := s.Ops.Gates(ctx, ns)
		if err != nil {
			return nil, err
		}
		for i := range gates {
			g := &gates[i]
			ref := g.Namespace + "/" + g.Name

			if ev := g.Spec.Evidence; ev != nil && ev.ServerURL != "" {
				t, ok := byServer[ev.ServerURL]
				if !ok {
					t = &FidesTarget{ServerURL: ev.ServerURL}
					byServer[ev.ServerURL] = t
				}
				t.Gates = append(t.Gates, ref)
				if ev.FidesEnvironment != "" {
					t.Environments = append(t.Environments, ev.FidesEnvironment)
				}
			}

			for _, w := range g.Spec.Watch {
				// clusterRef lives inside the check's opaque `with` blob rather
				// than in a typed field, because a health check's configuration
				// belongs to whichever checker reads it (D4). So this parses
				// for the one key it needs and ignores everything else — a
				// checker that has no clusterRef simply yields nothing.
				name := clusterRefName(w.With)
				if name == "" {
					continue
				}
				key := g.Namespace + "/" + name
				c, ok := byCluster[key]
				if !ok {
					c = &ClusterTarget{Secret: key}
					byCluster[key] = c
				}
				if len(c.Gates) == 0 || c.Gates[len(c.Gates)-1] != ref {
					c.Gates = append(c.Gates, ref)
				}
			}
		}
	}

	for _, t := range byServer {
		t.Reachable, t.Detail = probe(ctx, t.ServerURL)
		sort.Strings(t.Gates)
		t.Environments = dedupe(t.Environments)
		out.Fides = append(out.Fides, *t)
	}
	sort.Slice(out.Fides, func(i, j int) bool { return out.Fides[i].ServerURL < out.Fides[j].ServerURL })

	// Every labelled cluster Secret, not only the ones a Gate names. Merged
	// into whatever the Gate walk found so a connected-but-unused cluster
	// appears with no Gates rather than not at all.
	for _, ns := range namespaces {
		if err := s.Auth.Authorize(ctx, subject, ActionRead, ns); err != nil {
			continue
		}
		var secrets corev1.SecretList
		if err := s.Ops.Client.List(ctx, &secrets,
			client.InNamespace(ns), client.MatchingLabels{ClusterLabel: "true"}); err != nil {
			// A caller who may read Gates but not Secrets is ordinary, and the
			// rest of the screen is still worth showing.
			continue
		}
		for i := range secrets.Items {
			key := ns + "/" + secrets.Items[i].Name
			if _, ok := byCluster[key]; !ok {
				byCluster[key] = &ClusterTarget{Secret: key, Gates: []string{}}
			}
		}
	}

	for _, c := range byCluster {
		sort.Strings(c.Gates)
		c.Reachable, c.Detail = s.probeCluster(ctx, c.Secret)
		out.Clusters = append(out.Clusters, *c)
	}
	sort.Slice(out.Clusters, func(i, j int) bool { return out.Clusters[i].Secret < out.Clusters[j].Secret })

	return out, nil
}

// probe asks whether the evidence server answers at all.
//
// Deliberately unauthenticated: the question is "is this address a Fides server
// this cluster can reach", and a 401 answers it as well as a 200 does — better,
// in fact, since it proves something is listening and checking credentials. The
// server's token lives in a namespace this process should not reach into just
// to make a status dot greener.
func probe(ctx context.Context, url string) (bool, string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/api/v1/flows", nil)
	if err != nil {
		return false, err.Error()
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return false, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()

	// 5xx is the server failing; anything else means it answered.
	if resp.StatusCode >= 500 {
		return false, resp.Status
	}
	return true, ""
}

// probeCluster asks whether the stored credentials still work.
//
// Deliberately the cheapest possible question — a version read, which every
// apiserver answers and which needs no permissions worth having. The point is
// "do these credentials reach a Kubernetes API", not "what may they do there";
// that second question is answered by the Gate's own checks, against the
// namespace the Gate is allowed to look at.
func (s *Server) probeCluster(ctx context.Context, secret string) (bool, string) {
	namespace, name, ok := strings.Cut(secret, "/")
	if !ok {
		return false, "malformed secret reference"
	}

	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	var sec corev1.Secret
	if err := s.Ops.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &sec); err != nil {
		return false, "cannot read the Secret: " + err.Error()
	}
	raw := sec.Data["value"]
	if len(raw) == 0 {
		return false, `the Secret has no "value" key — a kubeconfig lives there, the same key Flux uses`
	}

	cfg, err := clientcmd.RESTConfigFromKubeConfig(raw)
	if err != nil {
		return false, "the kubeconfig is unusable: " + err.Error()
	}
	// Bounded here as well as by the context: a kubeconfig naming an address
	// that blackholes would otherwise hold the request open for the whole
	// settings call, and one unreachable cluster should not stall the screen.
	cfg.Timeout = 5 * time.Second

	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return false, err.Error()
	}
	if _, err := dc.ServerVersion(); err != nil {
		return false, err.Error()
	}
	return true, ""
}

func dedupe(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := in[:0]
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// clusterRefName pulls `clusterRef.name` out of a health check's `with` blob.
//
// Tolerant on purpose: an unparseable or absent block means "this check does
// not name a cluster", which is true of almost all of them, and is not
// something a settings screen should report as an error.
func clusterRefName(with *apiextensionsv1.JSON) string {
	if with == nil || len(with.Raw) == 0 {
		return ""
	}
	var cfg struct {
		ClusterRef *v1alpha1.LocalSecretRef `json:"clusterRef"`
	}
	if err := json.Unmarshal(with.Raw, &cfg); err != nil || cfg.ClusterRef == nil {
		return ""
	}
	return cfg.ClusterRef.Name
}
