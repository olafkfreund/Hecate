package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/olafkfreund/hecate/pkg/fides"
)

// capture runs f with stdout redirected, and returns what it printed.
func capture(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	f()
	_ = w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func fidesServing(t *testing.T, body string) *fides.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c, err := fides.New(fides.Config{BaseURL: srv.URL, Token: "k"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The exit code is the whole contract: a pipeline gates on it, and it has to
// separate "the evidence is bad" from "I could not look".
func TestVerifyExitCodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int
		says string
	}{
		{
			"a valid chain",
			`{"valid":true,"count":14,"broken_at":-1,"external_anchor":{"anchored":true,"head_matches":true,"anchored_at":"2026-08-09T11:02:00Z"}}`,
			exitOK, "anchored 2026-08-09",
		},
		{
			"a tampered chain",
			`{"valid":false,"count":9,"broken_at":6,"reason":"content_hash does not match the recorded entry"}`,
			exitBroken, "BROKEN at entry 6",
		},
		{
			// A chain nobody independent timestamped is a weaker claim, and the
			// output should not let it read like the strong one.
			"a valid chain with no external anchor",
			`{"valid":true,"count":3,"broken_at":-1}`,
			exitOK, "not externally anchored",
		},
		{
			"an anchor over a different head",
			`{"valid":true,"count":3,"broken_at":-1,"external_anchor":{"anchored":true,"head_matches":false,"anchored_at":"2026-08-01T00:00:00Z"}}`,
			exitOK, "different chain head",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fidesServing(t, tc.body)
			var code int
			out := capture(t, func() {
				code = report(one(context.Background(), c, crossing{Gate: "production", Trail: "91b2ffff-0000-4000-8000-00000000abcd"}))
			})

			if code != tc.want {
				t.Errorf("exit = %d, want %d", code, tc.want)
			}
			if !strings.Contains(out, tc.says) {
				t.Errorf("output does not say %q:\n%s", tc.says, out)
			}
			if !strings.Contains(out, "production") || !strings.Contains(out, "91b2ffff") {
				t.Errorf("output does not identify the crossing:\n%s", out)
			}
		})
	}
}

// A Fides that will not answer is not a broken chain, and reporting it as one
// would send somebody hunting for tampering that never happened.
func TestUnreachableFidesIsNotTampering(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	c, err := fides.New(fides.Config{BaseURL: srv.URL, Token: "wrong"})
	if err != nil {
		t.Fatal(err)
	}

	var code int
	out := capture(t, func() {
		code = report(one(context.Background(), c, crossing{Gate: "production", Trail: "t"}))
	})

	if code != exitError {
		t.Errorf("exit = %d, want %d (could not check)", code, exitError)
	}
	if strings.Contains(out, "BROKEN") {
		t.Errorf("an unreachable Fides was reported as tampering:\n%s", out)
	}
}

// Every trail is checked before returning. Stopping at the first broken chain
// would hide how much of the history is affected.
func TestVerifyReportsEveryCrossingAndTheWorstCode(t *testing.T) {
	broken := fidesServing(t, `{"valid":false,"count":2,"broken_at":1,"reason":"deletion"}`)
	valid := fidesServing(t, `{"valid":true,"count":5,"broken_at":-1}`)
	ctx := context.Background()

	var code int
	out := capture(t, func() {
		code = report(
			one(ctx, valid, crossing{Gate: "dev", Trail: "aaaa1111-0000-4000-8000-00000000aaaa"}),
			one(ctx, broken, crossing{Gate: "production", Trail: "bbbb2222-0000-4000-8000-00000000bbbb"}),
			one(ctx, valid, crossing{Gate: "staging", Trail: "cccc3333-0000-4000-8000-00000000cccc"}),
		)
	})

	if code != exitBroken {
		t.Errorf("exit = %d, want %d", code, exitBroken)
	}
	for _, gate := range []string{"dev", "production", "staging"} {
		if !strings.Contains(out, gate) {
			t.Errorf("%s was not reported:\n%s", gate, out)
		}
	}
	if strings.Count(out, "✓") != 2 || strings.Count(out, "✗") != 1 {
		t.Errorf("wrong verdicts:\n%s", out)
	}
}

func TestVerifyRefusesUnusableInvocations(t *testing.T) {
	t.Setenv("FIDES_SERVER_URL", "")
	t.Setenv("FIDES_TOKEN", "")

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no server or token", []string{"some-bundle"}},
		{"a token but no server", []string{"--token", "k", "some-bundle"}},
		{"both a bundle and a trail", []string{"--server", "https://f.test", "--token", "k", "--trail", "t", "b"}},
		{"neither a bundle nor a trail", []string{"--server", "https://f.test", "--token", "k"}},
		{"two bundles", []string{"--server", "https://f.test", "--token", "k", "a", "b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code := verify(context.Background(), tc.args); code != exitUsage {
				t.Errorf("exit = %d, want %d", code, exitUsage)
			}
		})
	}
}

// A trail id is a UUID; printing the whole thing three times a line makes the
// report unreadable, and the first group is enough to tell them apart.
func TestShortenedTrailIDs(t *testing.T) {
	for in, want := range map[string]string{
		"91b2ffff-0000-4000-8000-00000000abcd": "91b2ffff",
		"nodashesbutlong":                      "nodashes",
		"short":                                "short",
	} {
		if got := short(in); got != want {
			t.Errorf("short(%q) = %q, want %q", in, got, want)
		}
	}
}
