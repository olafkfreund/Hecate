package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// APIError is a host's refusal, with the status kept so callers can tell a
// wrong token from a missing repository without reading English.
type APIError struct {
	Status  int
	Method  string
	Path    string
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("%s %s: %s", e.Method, e.Path, http.StatusText(e.Status))
	}
	return fmt.Sprintf("%s %s: %s", e.Method, e.Path, e.Message)
}

// IsNotFound reports whether the host says the thing is not there. On GitHub a
// private repository reached with an unauthorised token also answers 404, which
// is the same answer for a human: check the token and the name.
func IsNotFound(err error) bool { return statusIs(err, http.StatusNotFound) }

// IsAuth reports whether the host rejected the credentials.
func IsAuth(err error) bool {
	return statusIs(err, http.StatusUnauthorized) || statusIs(err, http.StatusForbidden)
}

func statusIs(err error, status int) bool {
	var api *APIError
	return errors.As(err, &api) && api.Status == status
}

// client is the shared REST plumbing. Both hosts need five endpoints between
// them, so they share this rather than each carrying a generated SDK: an SDK
// per host is two large dependencies, two release cadences and two ways to
// configure a base URL, in exchange for code we would write once.
type client struct {
	base    *url.URL
	http    *http.Client
	headers map[string]string
}

func newClient(baseURL, fallback string, headers map[string]string) (*client, error) {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		raw = fallback
	}
	u, err := url.Parse(strings.TrimSuffix(raw, "/") + "/")
	if err != nil {
		return nil, fmt.Errorf("base URL %q: %w", raw, err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("base URL %q must be http or https", raw)
	}
	return &client{
		base: u,
		// A timeout, because a step that hangs holds a worker until the
		// Passage's own deadline, and the engine would rather be told to retry.
		http:    &http.Client{Timeout: 30 * time.Second},
		headers: headers,
	}, nil
}

// do sends a request and decodes the response into out, which may be nil.
func (c *client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	ref, err := url.Parse(strings.TrimPrefix(path, "/"))
	if err != nil {
		return fmt.Errorf("path %q: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base.ResolveReference(ref).String(), reader)
	if err != nil {
		return err
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	// Capped: a host that answers a PR request with a megabyte of HTML — a
	// proxy's login page, usually — should not become a megabyte in a Passage's
	// status message.
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return &APIError{Status: resp.StatusCode, Method: method, Path: path, Message: apiMessage(payload)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("%s %s: decoding response: %w", method, path, err)
	}
	return nil
}

// apiMessage digs the human-readable part out of an error body. Both hosts put
// it under "message"; GitHub adds per-field detail that is usually the actually
// useful part ("A pull request already exists for acme:hecate/x").
func apiMessage(payload []byte) string {
	var body struct {
		Message string `json:"message"`
		Errors  []struct {
			Message string `json:"message"`
			Field   string `json:"field"`
			Code    string `json:"code"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return strings.TrimSpace(truncate(string(payload), 200))
	}
	var parts []string
	if body.Message != "" {
		parts = append(parts, body.Message)
	}
	for _, e := range body.Errors {
		switch {
		case e.Message != "":
			parts = append(parts, e.Message)
		case e.Field != "":
			parts = append(parts, fmt.Sprintf("%s %s", e.Field, e.Code))
		}
	}
	return strings.Join(parts, ": ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
