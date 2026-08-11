package steps

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/passage"
)

// endpoint is a server that records what it was sent and answers as told.
type endpoint struct {
	status  int
	body    string
	method  string
	headers http.Header
	got     []byte
}

func (e *endpoint) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.method = r.Method
		e.headers = r.Header.Clone()
		e.got, _ = io.ReadAll(r.Body)
		status := e.status
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(e.body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func httpStep(t *testing.T, objects ...runtime.Object) *HTTP {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, o := range objects {
		builder = builder.WithRuntimeObjects(o)
	}
	return NewHTTP(builder.Build())
}

func raw(t *testing.T, v any) *apiextensionsv1.JSON {
	t.Helper()
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return &apiextensionsv1.JSON{Raw: encoded}
}

func TestHTTPPostsABody(t *testing.T) {
	e := &endpoint{body: `{"ok":true}`}
	url := e.start(t)

	res := mustRun(t, httpStep(t), gitCtx(t, t.TempDir(), HTTPConfig{
		URL:  url,
		Body: raw(t, map[string]string{"text": "podinfo 6.5.0 crossed production"}),
	}))

	// A body implies POST: a GET with a payload is almost never what was meant.
	if e.method != http.MethodPost {
		t.Errorf("method = %s", e.method)
	}
	if e.headers.Get("Content-Type") != "application/json" {
		t.Errorf("content-type = %q", e.headers.Get("Content-Type"))
	}
	if !strings.Contains(string(e.got), "podinfo 6.5.0") {
		t.Errorf("body = %s", e.got)
	}
	if res.Output["status"] != 200 {
		t.Errorf("status = %v", res.Output["status"])
	}
	// Parsed when it parses, so a condition can read a field.
	if m, ok := res.Output["json"].(map[string]any); !ok || m["ok"] != true {
		t.Errorf("json = %v", res.Output["json"])
	}
}

// A token in a Gate is a token anyone with read access to the namespace has.
func TestHTTPTakesSecretsFromSecrets(t *testing.T) {
	e := &endpoint{}
	url := e.start(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "slack", Namespace: "acme"},
		Data:       map[string][]byte{"token": []byte("xoxb-1234")},
	}

	mustRun(t, httpStep(t, secret), gitCtx(t, t.TempDir(), HTTPConfig{
		URL:     url,
		Headers: map[string]string{"X-Trace": "abc"},
		SecretHeaders: []SecretHeader{{
			Name: "Authorization", Prefix: "Bearer ",
			SecretRef: &v1alpha1.LocalSecretRef{Name: "slack"},
		}},
	}))

	if got := e.headers.Get("Authorization"); got != "Bearer xoxb-1234" {
		t.Errorf("Authorization = %q", got)
	}
	if got := e.headers.Get("X-Trace"); got != "abc" {
		t.Errorf("X-Trace = %q", got)
	}
}

// The failure mode the issue names: a 200 whose body says it did not work.
// Treating that as a passed check is worse than having no check.
func TestHTTPSuccessIsDefinedNotAssumed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		succeeds  bool
		successIf string
	}{
		{"the body agrees", `{"result":"ok"}`, true, `json.result == "ok"`},
		{"the body disagrees", `{"result":"denied"}`, false, `json.result == "ok"`},
		{"a condition over the status", `{}`, true, `status == 200`},
		// Plenty of endpoints answer plain text; an unparseable body must not
		// stop a condition that does not look at it.
		{"a body that is not JSON", "APPROVED", true, `body contains "APPROVED"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &endpoint{body: tc.body}
			_, err := httpStep(t).Run(context.Background(), gitCtx(t, t.TempDir(), HTTPConfig{
				URL: e.start(t), SuccessIf: tc.successIf,
			}))
			if tc.succeeds && err != nil {
				t.Fatalf("unexpected failure: %v", err)
			}
			if !tc.succeeds {
				if !passage.IsTerminal(err) || passage.ReasonOf(err) != ReasonHTTPFailed {
					t.Fatalf("err = %v, reason = %s", err, passage.ReasonOf(err))
				}
				// The message has to say what was checked, or the operator is
				// left with "the step failed" and a 200.
				if !strings.Contains(err.Error(), tc.successIf) {
					t.Errorf("the message does not name the condition: %v", err)
				}
			}
		})
	}
}

// A 4xx will be refused just as firmly next time; a 5xx is the endpoint having
// a bad time. Retrying the first is noise, retrying the second is the point.
func TestHTTPRetriesOnlyWhatMightSucceed(t *testing.T) {
	for _, tc := range []struct {
		status   int
		terminal bool
	}{
		{http.StatusBadRequest, true},
		{http.StatusForbidden, true},
		{http.StatusInternalServerError, false},
		{http.StatusBadGateway, false},
	} {
		e := &endpoint{status: tc.status}
		_, err := httpStep(t).Run(context.Background(), gitCtx(t, t.TempDir(), HTTPConfig{URL: e.start(t)}))
		if err == nil {
			t.Fatalf("%d: expected a failure", tc.status)
		}
		if passage.IsTerminal(err) != tc.terminal {
			t.Errorf("%d: terminal = %v, want %v", tc.status, passage.IsTerminal(err), tc.terminal)
		}
	}
}

func TestHTTPExpectStatus(t *testing.T) {
	e := &endpoint{status: http.StatusAccepted}
	url := e.start(t)

	mustRun(t, httpStep(t), gitCtx(t, t.TempDir(), HTTPConfig{
		URL: url, ExpectStatus: []int{http.StatusAccepted},
	}))

	// A 200 is not automatically acceptable once the author has said what is.
	_, err := httpStep(t).Run(context.Background(), gitCtx(t, t.TempDir(), HTTPConfig{
		URL: url, ExpectStatus: []int{http.StatusCreated},
	}))
	if !passage.IsTerminal(err) {
		t.Errorf("err = %v", err)
	}
}

// A response pasted whole into a Passage would put a megabyte of a proxy's
// login page into etcd.
func TestHTTPCapsTheCapturedBody(t *testing.T) {
	e := &endpoint{body: strings.Repeat("x", maxCapturedBody*2)}
	res := mustRun(t, httpStep(t), gitCtx(t, t.TempDir(), HTTPConfig{URL: e.start(t)}))

	body, _ := res.Output["body"].(string)
	if len(body) != maxCapturedBody {
		t.Errorf("captured %d bytes, want %d", len(body), maxCapturedBody)
	}
	if res.Output["truncated"] != true {
		t.Error("truncation was not reported, so the body reads as the whole answer")
	}
}

func TestHTTPRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cfg    HTTPConfig
		reason string
	}{
		{"no url", HTTPConfig{}, ReasonInvalidConfig},
		{"a scheme we will not call", HTTPConfig{URL: "file:///etc/passwd"}, ReasonInvalidConfig},
		{
			"a secret header with no secret",
			HTTPConfig{URL: "https://example.test", SecretHeaders: []SecretHeader{{Name: "A"}}},
			ReasonInvalidConfig,
		},
		{
			"a secret that is not there",
			HTTPConfig{URL: "https://example.test", SecretHeaders: []SecretHeader{{
				Name: "A", SecretRef: &v1alpha1.LocalSecretRef{Name: "missing"},
			}}},
			ReasonInvalidConfig,
		},
		{
			"a condition that does not compile",
			HTTPConfig{URL: "https://example.test", SuccessIf: "status =="},
			ReasonInvalidConfig,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The two that reach the network are refused before it: no server
			// is running at example.test in a test.
			_, err := httpStep(t).Run(context.Background(), gitCtx(t, t.TempDir(), tc.cfg))
			if err == nil {
				t.Fatal("expected a failure")
			}
			if tc.reason != "" && passage.ReasonOf(err) != tc.reason && passage.IsTerminal(err) {
				t.Errorf("reason = %s, want %s (%v)", passage.ReasonOf(err), tc.reason, err)
			}
		})
	}
}

func TestHTTPTimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	_, err := httpStep(t).Run(context.Background(), gitCtx(t, t.TempDir(), HTTPConfig{
		URL: srv.URL, Timeout: &metav1.Duration{Duration: 20 * time.Millisecond},
	}))
	if err == nil {
		t.Fatal("expected a timeout")
	}
	// A slow endpoint is worth retrying — it may be restarting.
	if passage.IsTerminal(err) {
		t.Errorf("a timeout should be retryable: %v", err)
	}
	if passage.ReasonOf(err) != ReasonHTTPUnreachable {
		t.Errorf("reason = %s", passage.ReasonOf(err))
	}
}
