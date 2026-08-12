package metrics

import (
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

const dashboardPath = "../../charts/hecate/dashboards/hecate.json"

// A dashboard is a text file, so a renamed metric or a typo in a query breaks
// it silently — the panel just shows "No data", which is indistinguishable from
// a quiet system. That is exactly the state a delivery dashboard is supposed to
// rule out.
//
// This is the check that makes the dashboard part of the build: every
// hecate_* series it references must be one the code actually registers.
func TestDashboardOnlyReferencesRegisteredMetrics(t *testing.T) {
	registered := registeredNames(t)

	for _, ref := range dashboardMetrics(t) {
		if !registered[base(ref)] {
			t.Errorf("the dashboard queries %q, which nothing registers (known: %v)",
				ref, sorted(registered))
		}
	}
}

// The reverse direction: a metric worth exporting is worth showing. This is not
// a rule that every series must appear, but a new one silently missing from the
// dashboard is the common way a dashboard rots.
func TestDashboardCoversEveryHecateMetric(t *testing.T) {
	used := map[string]bool{}
	for _, ref := range dashboardMetrics(t) {
		used[base(ref)] = true
	}
	for name := range registeredNames(t) {
		if !used[name] {
			t.Errorf("%s is exported but appears nowhere in the dashboard", name)
		}
	}
}

func TestDashboardIsValidJSONWithPanels(t *testing.T) {
	var dash struct {
		Title  string `json:"title"`
		UID    string `json:"uid"`
		Panels []struct {
			Type    string `json:"type"`
			Title   string `json:"title"`
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	raw, err := os.ReadFile(dashboardPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &dash); err != nil {
		t.Fatalf("the dashboard is not valid JSON: %v", err)
	}
	if dash.UID == "" || dash.Title == "" {
		t.Error("a dashboard needs a uid and a title, or re-importing it creates a duplicate")
	}
	for _, p := range dash.Panels {
		// Rows are containers and carry no query of their own.
		if p.Type == "row" {
			continue
		}
		if len(p.Targets) == 0 {
			t.Errorf("panel %q has no query", p.Title)
		}
		for _, tgt := range p.Targets {
			if strings.TrimSpace(tgt.Expr) == "" {
				t.Errorf("panel %q has an empty query", p.Title)
			}
		}
	}
}

// metricRef matches a Prometheus series name in a query. Suffixes are kept so
// base() can strip them deliberately rather than by accident.
var metricRef = regexp.MustCompile(`hecate_[a-z0-9_]+`)

func dashboardMetrics(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(dashboardPath)
	if err != nil {
		t.Fatal(err)
	}
	// Matched against the whole file rather than only `expr` fields, so
	// template variable queries (`label_values(...)`) are covered too — those
	// break just as silently.
	return metricRef.FindAllString(string(raw), -1)
}

// base strips the suffixes Prometheus adds to a histogram or counter, which are
// series a dashboard queries but nothing registers by that name.
func base(name string) string {
	for _, suffix := range []string{"_bucket", "_sum", "_count"} {
		if strings.HasSuffix(name, suffix) {
			return strings.TrimSuffix(name, suffix)
		}
	}
	return name
}

// fqName pulls a metric's name out of its descriptor. Gather() will not do:
// a vec with no observations yet reports no families at all, so a gatherer
// sees nothing in a fresh process — which is precisely the state a test runs
// in.
var fqName = regexp.MustCompile(`fqName: "([a-z0-9_]+)"`)

func registeredNames(t *testing.T) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	for _, c := range Collectors() {
		ch := make(chan *prometheus.Desc, 8)
		go func() {
			c.Describe(ch)
			close(ch)
		}()
		for desc := range ch {
			m := fqName.FindStringSubmatch(desc.String())
			if m == nil {
				t.Fatalf("could not read a metric name out of %s", desc)
			}
			names[m[1]] = true
		}
	}
	if len(names) == 0 {
		t.Fatal("Collectors() is empty")
	}
	return names
}

func sorted(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
