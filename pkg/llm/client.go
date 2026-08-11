package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Config is what it takes to reach a model.
type Config struct {
	// BaseURL is the API root — http://localhost:11434/v1 for Ollama,
	// https://api.openai.com/v1 for OpenAI. The /chat/completions path is
	// appended.
	BaseURL string
	// Model is the model name the endpoint knows.
	Model string
	// APIKey is optional: local runtimes do not want one.
	APIKey string
	// Timeout bounds a single completion. Zero uses 60s, which is generous
	// because a local model on CPU is slow and a diagnosis is not on anyone's
	// critical path.
	Timeout time.Duration
}

// FromEnv reads the standard variables, so Hecate drops into a setup that
// already has a model configured.
//
// HECATE_LLM_* wins over OPENAI_* so a machine can point Hecate at a local
// model without disturbing whatever else uses the OpenAI variables.
func FromEnv() Config {
	return Config{
		BaseURL: first(os.Getenv("HECATE_LLM_URL"), os.Getenv("OPENAI_BASE_URL")),
		Model:   first(os.Getenv("HECATE_LLM_MODEL"), os.Getenv("OPENAI_MODEL")),
		APIKey:  first(os.Getenv("HECATE_LLM_API_KEY"), os.Getenv("OPENAI_API_KEY")),
	}
}

func first(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// Configured reports whether there is enough to talk to a model.
//
// Hecate must work with no model at all: diagnosis is an assist, never a
// dependency, so every caller asks this rather than treating its absence as an
// error.
func (c Config) Configured() bool { return c.BaseURL != "" && c.Model != "" }

// Client is an OpenAI-compatible chat completions client.
type Client struct {
	cfg  Config
	http *http.Client
}

// New returns a client, or an error if the configuration cannot work.
func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("llm: no base URL (set HECATE_LLM_URL)")
	}
	if cfg.Model == "" {
		return nil, errors.New("llm: no model (set HECATE_LLM_MODEL)")
	}
	if !strings.HasPrefix(cfg.BaseURL, "http://") && !strings.HasPrefix(cfg.BaseURL, "https://") {
		return nil, fmt.Errorf("llm: base URL %q must be http or https", cfg.BaseURL)
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	return &Client{cfg: cfg, http: &http.Client{Timeout: timeout}}, nil
}

// Model is the model this client will ask, for reporting.
func (c *Client) Model() string { return c.cfg.Model }

// Complete sends one prompt and returns the model's reply.
func (c *Client) Complete(ctx context.Context, p Prompt) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model": c.cfg.Model,
		"messages": []map[string]string{
			{"role": "system", "content": p.System()},
			{"role": "user", "content": p.User()},
		},
		// Low but not zero: a diagnosis should read the same way twice, and
		// some endpoints reject exactly 0.
		"temperature": 0.1,
		"stream":      false,
	})
	if err != nil {
		return "", fmt.Errorf("llm: encoding the request: %w", err)
	}

	url := strings.TrimSuffix(c.cfg.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("llm: reading the response: %w", err)
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("llm: %s: %s", resp.Status, strings.TrimSpace(truncate(string(payload), 300)))
	}

	var answer struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &answer); err != nil {
		return "", fmt.Errorf("llm: decoding the response: %w", err)
	}
	if len(answer.Choices) == 0 {
		return "", errors.New("llm: the model returned no choices")
	}
	return strings.TrimSpace(answer.Choices[0].Message.Content), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
