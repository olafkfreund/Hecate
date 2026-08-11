// Package fides talks to a Fides server — the compliance system Hecate records
// promotions in and asks for permission.
//
// Only the endpoints a promotion needs are here. Fides has a large API; Hecate
// uses three parts of it:
//
//   - verify a trail's attestation chain, so the tamper-evidence claim is
//     checkable by whoever relies on it rather than asserted in documentation;
//   - check an environment's policy against a trail;
//   - check whether an artifact is on an environment's allowlist.
//
// The last two are why a Gate has to name a Fides environment at all.
package fides

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

// Config is what a client needs to reach a Fides server.
type Config struct {
	// BaseURL is the server root, e.g. https://fides.acme.io. Required: there
	// is no public Fides to default to.
	BaseURL string
	// Token is a Fides API key, sent as a bearer token.
	Token string
	// Timeout bounds a single call. Zero uses 30s.
	Timeout time.Duration
}

// Client is a Fides API client.
type Client struct {
	base  *url.URL
	token string
	http  *http.Client
}

// New builds a client.
func New(cfg Config) (*Client, error) {
	raw := strings.TrimSpace(cfg.BaseURL)
	if raw == "" {
		return nil, errors.New("fides: no server URL")
	}
	base, err := url.Parse(strings.TrimSuffix(raw, "/") + "/")
	if err != nil {
		return nil, fmt.Errorf("fides: server URL %q: %w", raw, err)
	}
	if base.Scheme != "https" && base.Scheme != "http" {
		return nil, fmt.Errorf("fides: server URL %q must be http or https", raw)
	}
	if cfg.Token == "" {
		return nil, errors.New("fides: no API token")
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &Client{base: base, token: cfg.Token, http: &http.Client{Timeout: timeout}}, nil
}

// Error is a refusal from the server, with the status kept so a caller can tell
// a rejected token from a trail that is not there.
type Error struct {
	Status int
	Method string
	Path   string
	Body   string
}

func (e *Error) Error() string {
	body := strings.TrimSpace(e.Body)
	if body == "" {
		body = http.StatusText(e.Status)
	}
	return fmt.Sprintf("fides: %s %s: %d: %s", e.Method, e.Path, e.Status, body)
}

// IsNotFound reports whether Fides says the thing is not there.
func IsNotFound(err error) bool { return statusIs(err, http.StatusNotFound) }

// IsAuth reports whether Fides rejected the credentials.
func IsAuth(err error) bool {
	return statusIs(err, http.StatusUnauthorized) || statusIs(err, http.StatusForbidden)
}

func statusIs(err error, status int) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == status
}

// Chain is the verdict on a trail's attestation chain.
//
// Fields mirror Fides' own response rather than being renamed, so an operator
// comparing `hecate verify` with `fides verify-chain` sees the same words.
type Chain struct {
	// Valid is false when the chain has been tampered with, reordered, or had
	// an entry deleted.
	Valid bool `json:"valid"`
	// Count is how many attestations are on the trail.
	Count int `json:"count"`
	// BrokenAt is the index of the first bad entry, or -1 when valid.
	BrokenAt int `json:"broken_at"`
	// Reason says what was wrong, when something was.
	Reason string `json:"reason,omitempty"`
	// ExternalAnchor reports whether the chain head was timestamped by an
	// external authority. A valid chain that only we vouch for is a weaker
	// claim than one an RFC3161 authority saw at a point in time.
	ExternalAnchor *Anchor `json:"external_anchor,omitempty"`
}

// Anchor is the external timestamp over a chain head.
type Anchor struct {
	Anchored bool `json:"anchored"`
	// HeadMatches is false when the chain has moved on since it was anchored,
	// or when it was rewritten under the anchor.
	HeadMatches bool      `json:"head_matches"`
	TSAURL      string    `json:"tsa_url,omitempty"`
	AnchoredAt  time.Time `json:"anchored_at,omitempty"`
}

// VerifyChain checks a trail's tamper-evidence chain.
func (c *Client) VerifyChain(ctx context.Context, trail string) (*Chain, error) {
	if strings.TrimSpace(trail) == "" {
		return nil, errors.New("fides: no trail id")
	}
	var chain Chain
	if err := c.get(ctx, "api/v1/trails/"+url.PathEscape(trail)+"/verify-chain", &chain); err != nil {
		return nil, err
	}
	return &chain, nil
}

// PolicyVerdict is the answer to "does this trail satisfy the environment's
// policy?"
//
// Field names mirror Fides' own response. `compliant` rather than `passed`, and
// a per-policy breakdown rather than a flat list of violations, because a Gate
// that refuses a crossing has to be able to say which policy refused it.
type PolicyVerdict struct {
	Compliant bool           `json:"compliant"`
	TrailID   string         `json:"trail_id,omitempty"`
	Results   []PolicyResult `json:"results,omitempty"`
}

// PolicyResult is one environment policy's verdict on the trail.
type PolicyResult struct {
	Policy string `json:"policy"`
	// Applies is false when the policy is conditional on a flow tag the trail
	// does not carry. A policy that did not apply is not a policy that passed.
	Applies bool `json:"applies"`
	// Missing lists the attestation types the policy required and the trail
	// does not have a compliant one of.
	Missing []string `json:"missing,omitempty"`
}

// Unmet lists the policies that refused, so a failure can name them.
func (v PolicyVerdict) Unmet() []string {
	var names []string
	for _, r := range v.Results {
		if r.Applies && len(r.Missing) > 0 {
			names = append(names, fmt.Sprintf("%s (missing %s)", r.Policy, strings.Join(r.Missing, ", ")))
		}
	}
	return names
}

// PolicyCheck evaluates an environment's policy against a trail.
//
// Both identifiers are UUIDs: the environment comes from the Gate, the trail
// from the crossing being judged.
func (c *Client) PolicyCheck(ctx context.Context, environment, trail string) (*PolicyVerdict, error) {
	if environment == "" || trail == "" {
		return nil, errors.New("fides: policy check needs both an environment and a trail")
	}
	var verdict PolicyVerdict
	path := fmt.Sprintf("api/v1/environments/%s/policy-check?trail=%s",
		url.PathEscape(environment), url.QueryEscape(trail))
	if err := c.get(ctx, path, &verdict); err != nil {
		return nil, err
	}
	return &verdict, nil
}

// Allowlisted reports whether an artifact digest is approved for an
// environment. The digest is the bare sha256 hex, as Fides stores it.
func (c *Client) Allowlisted(ctx context.Context, environment, sha256 string) (bool, error) {
	if environment == "" || sha256 == "" {
		return false, errors.New("fides: allowlist check needs both an environment and a digest")
	}
	var answer struct {
		Approved bool `json:"approved"`
	}
	path := fmt.Sprintf("api/v1/environments/%s/allowlist?sha=%s",
		url.PathEscape(environment), url.QueryEscape(strings.TrimPrefix(sha256, "sha256:")))
	if err := c.get(ctx, path, &answer); err != nil {
		return false, err
	}
	return answer.Approved, nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

// do sends a request and decodes the response into out, which may be nil.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("fides: encoding the request: %w", err)
		}
		payload = bytes.NewReader(encoded)
	}

	ref, err := url.Parse(path)
	if err != nil {
		return fmt.Errorf("fides: path %q: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base.ResolveReference(ref).String(), payload)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("fides: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Capped: a reverse proxy answering with a login page should not become a
	// megabyte in an error message.
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("fides: reading the response: %w", err)
	}
	if resp.StatusCode >= 300 {
		return &Error{Status: resp.StatusCode, Method: method, Path: path, Body: string(answer)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(answer, out); err != nil {
		return fmt.Errorf("fides: %s %s: decoding the response: %w", method, path, err)
	}
	return nil
}
