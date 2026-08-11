package steps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/expr"
	"github.com/olafkfreund/hecate/pkg/passage"
)

// StepHTTP is the value used in `steps[].uses`.
const StepHTTP = "http"

// Failure reasons this step can report.
const (
	// ReasonHTTPFailed means the request did not get an acceptable answer.
	ReasonHTTPFailed = "HTTPFailed"
	// ReasonHTTPUnreachable means there was no answer at all — DNS, connection
	// refused, a timeout. Worth retrying; a 400 is not.
	ReasonHTTPUnreachable = "HTTPUnreachable"
)

// maxCapturedBody bounds what goes into the Passage's status. A step that
// pasted a megabyte of HTML — a proxy's login page, usually — into an object
// stored in etcd would be a denial of service against the API server.
const maxCapturedBody = 4 << 10

// defaultHTTPTimeout keeps a hung endpoint from holding a worker. The engine
// would rather be told to retry.
const defaultHTTPTimeout = 30 * time.Second

// HTTPConfig is the `with:` block of an http step.
type HTTPConfig struct {
	URL string `json:"url"`
	// Method defaults to GET, or POST when a body is given.
	Method string `json:"method,omitempty"`
	// Headers are sent as written. Put nothing secret here: a Gate is a normal
	// object that anyone with read access can see.
	Headers map[string]string `json:"headers,omitempty"`
	// SecretHeaders are read from Secrets at call time, which is where a token
	// belongs.
	SecretHeaders []SecretHeader `json:"secretHeaders,omitempty"`
	// Body is sent as JSON.
	Body *apiextensionsv1.JSON `json:"body,omitempty"`
	// ExpectStatus lists acceptable status codes. Empty accepts any 2xx.
	ExpectStatus []int `json:"expectStatus,omitempty"`
	// SuccessIf is a condition over `status`, `body` and `json`, evaluated
	// after the response arrives.
	//
	// Written without `${{ }}`, because those are substituted before the step
	// runs and there is no response yet at that point.
	SuccessIf string           `json:"successIf,omitempty"`
	Timeout   *metav1.Duration `json:"timeout,omitempty"`
}

// SecretHeader is one header whose value comes from a Secret.
type SecretHeader struct {
	Name string `json:"name"`
	// Prefix goes in front of the secret value, for schemes like "Bearer ".
	Prefix    string                   `json:"prefix,omitempty"`
	SecretRef *v1alpha1.LocalSecretRef `json:"secretRef"`
	// Key in the Secret. Defaults to "token".
	Key string `json:"key,omitempty"`
}

// HTTP calls an external system as part of a crossing: notify, trigger, query.
//
// The escape hatch for everything there is no step for. Success is defined
// rather than assumed — plenty of systems answer 200 with a body saying the
// request failed, and a promotion that treated that as a passed check would be
// worse than having no check.
type HTTP struct {
	client client.Client
	http   *http.Client
}

// NewHTTP returns an http step.
func NewHTTP(c client.Client) *HTTP {
	return &HTTP{client: c, http: &http.Client{}}
}

// Name implements passage.Runner.
func (h *HTTP) Name() string { return StepHTTP }

// Run implements passage.Runner.
func (h *HTTP) Run(ctx context.Context, sc *passage.StepContext) (passage.StepResult, error) {
	cfg, err := passage.DecodeConfig[HTTPConfig](sc)
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepHTTP, err)
	}
	if cfg.URL == "" {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: url is required", StepHTTP)
	}
	if !strings.HasPrefix(cfg.URL, "http://") && !strings.HasPrefix(cfg.URL, "https://") {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig,
			"%s: url %q must be http or https", StepHTTP, cfg.URL)
	}

	req, err := h.request(ctx, sc, cfg)
	if err != nil {
		return passage.StepResult{}, err
	}

	timeout := defaultHTTPTimeout
	if cfg.Timeout != nil && cfg.Timeout.Duration > 0 {
		timeout = cfg.Timeout.Duration
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resp, err := h.http.Do(req.WithContext(callCtx))
	if err != nil {
		// No answer at all: the endpoint may simply be restarting.
		return passage.StepResult{}, passage.Fail(ReasonHTTPUnreachable,
			"%s: %s %s: %s", StepHTTP, req.Method, cfg.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxCapturedBody+1))
	if err != nil {
		return passage.StepResult{}, passage.Fail(ReasonHTTPUnreachable,
			"%s: reading the response: %s", StepHTTP, err)
	}
	body, truncated := capBody(payload)

	output := map[string]any{"status": resp.StatusCode, "body": body, "truncated": truncated}
	// Parsed when it parses, so a condition can read a field rather than match
	// a substring. Not an error when it does not: plenty of endpoints answer
	// with plain text and the status is the whole answer.
	var parsed any
	if json.Unmarshal(payload, &parsed) == nil {
		output["json"] = parsed
	}

	if err := accepted(cfg, resp.StatusCode); err != nil {
		return passage.StepResult{Output: output}, err
	}
	if cfg.SuccessIf != "" {
		ok, err := expr.Condition(cfg.SuccessIf, map[string]any{
			"status": resp.StatusCode, "body": body, "json": parsed,
		})
		if err != nil {
			return passage.StepResult{Output: output}, passage.FailTerminal(ReasonInvalidConfig,
				"%s: successIf: %s", StepHTTP, err)
		}
		if !ok {
			return passage.StepResult{Output: output}, passage.FailTerminal(ReasonHTTPFailed,
				"%s: %s answered %d, but successIf (%s) was false",
				StepHTTP, cfg.URL, resp.StatusCode, cfg.SuccessIf)
		}
	}

	return passage.StepResult{
		Phase:   v1alpha1.StepSucceeded,
		Message: fmt.Sprintf("%s %s → %d", req.Method, cfg.URL, resp.StatusCode),
		Output:  output,
	}, nil
}

// accepted decides whether the status is one the author asked for.
func accepted(cfg HTTPConfig, status int) error {
	if len(cfg.ExpectStatus) > 0 {
		for _, want := range cfg.ExpectStatus {
			if status == want {
				return nil
			}
		}
		return statusError(cfg, status,
			fmt.Sprintf("expected %v", cfg.ExpectStatus))
	}
	if status >= 200 && status < 300 {
		return nil
	}
	return statusError(cfg, status, "expected a 2xx")
}

func statusError(cfg HTTPConfig, status int, wanted string) error {
	// A 5xx is the endpoint having a bad time; a 4xx is us asking for something
	// it will refuse just as firmly next time.
	if status >= 500 {
		return passage.Fail(ReasonHTTPFailed, "%s: %s answered %d (%s)", StepHTTP, cfg.URL, status, wanted)
	}
	return passage.FailTerminal(ReasonHTTPFailed, "%s: %s answered %d (%s)", StepHTTP, cfg.URL, status, wanted)
}

// request builds the call, resolving secret headers last so nothing else can
// overwrite them.
func (h *HTTP) request(
	ctx context.Context, sc *passage.StepContext, cfg HTTPConfig,
) (*http.Request, error) {
	var body io.Reader
	method := strings.ToUpper(cfg.Method)
	if cfg.Body != nil && len(cfg.Body.Raw) > 0 {
		body = bytes.NewReader(cfg.Body.Raw)
		if method == "" {
			method = http.MethodPost
		}
	}
	if method == "" {
		method = http.MethodGet
	}

	req, err := http.NewRequestWithContext(ctx, method, cfg.URL, body)
	if err != nil {
		return nil, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepHTTP, err)
	}
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	for i, sh := range cfg.SecretHeaders {
		value, err := h.secretValue(ctx, sc.Namespace, sh)
		if err != nil {
			return nil, passage.FailTerminal(ReasonInvalidConfig,
				"%s: secretHeaders[%d]: %s", StepHTTP, i, err)
		}
		req.Header.Set(sh.Name, sh.Prefix+value)
	}
	return req, nil
}

func (h *HTTP) secretValue(ctx context.Context, namespace string, sh SecretHeader) (string, error) {
	if sh.Name == "" {
		return "", fmt.Errorf("name is required")
	}
	if sh.SecretRef == nil {
		return "", fmt.Errorf("secretRef is required")
	}
	if h.client == nil {
		return "", fmt.Errorf("secretRef set but the step has no client")
	}
	key := sh.Key
	if key == "" {
		key = "token"
	}
	var secret corev1.Secret
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: sh.SecretRef.Name}, &secret); err != nil {
		return "", fmt.Errorf("reading Secret %s/%s: %w", namespace, sh.SecretRef.Name, err)
	}
	value := string(secret.Data[key])
	if value == "" {
		return "", fmt.Errorf("secret %s has no %s", sh.SecretRef.Name, key)
	}
	return value, nil
}

func capBody(payload []byte) (string, bool) {
	if len(payload) > maxCapturedBody {
		return string(payload[:maxCapturedBody]), true
	}
	return string(payload), false
}
