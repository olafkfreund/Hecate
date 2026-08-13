package main

import (
	"context"
	"strings"
	"testing"
)

func completions(t *testing.T, args ...string) []string {
	t.Helper()
	out := capture(t, func() { complete(context.Background(), args) })
	return strings.Fields(out)
}

func has(candidates []string, want string) bool {
	for _, c := range candidates {
		if c == want {
			return true
		}
	}
	return false
}

func TestCompletionOffersCommands(t *testing.T) {
	got := completions(t, "")
	for _, want := range []string{"status", "explain", "verify", "completion", "man"} {
		if !has(got, want) {
			t.Errorf("%q not offered: %v", want, got)
		}
	}
	// The hidden one stays hidden: it exists to serve `complete`, and offering
	// it invites someone to script against a format that may change with it.
	if has(got, "__complete") {
		t.Error("__complete was offered to a user")
	}
}

func TestCompletionNarrowsByPrefix(t *testing.T) {
	got := completions(t, "ex")
	if len(got) != 1 || got[0] != "explain" {
		t.Errorf("got %v, want just explain", got)
	}
}

// Each command's own flags, not a union: offering -trail on `status` sends
// someone to write a command that cannot work.
func TestCompletionOffersOnlyThatCommandsFlags(t *testing.T) {
	verify := completions(t, "verify", "-")
	if !has(verify, "-trail") {
		t.Errorf("verify does not offer -trail: %v", verify)
	}
	status := completions(t, "status", "-")
	if has(status, "-trail") {
		t.Errorf("status offers -trail, which it does not have: %v", status)
	}
}

func TestCompletionOffersOutputFormats(t *testing.T) {
	for _, flag := range []string{"-o", "-output", "--output"} {
		got := completions(t, "status", flag, "")
		for _, want := range []string{formatTable, formatJSON, formatYAML} {
			if !has(got, want) {
				t.Errorf("%s did not offer %q: %v", flag, want, got)
			}
		}
	}
}

func TestCompletionOffersShells(t *testing.T) {
	got := completions(t, "completion", "")
	for _, want := range []string{"bash", "zsh", "fish"} {
		if !has(got, want) {
			t.Errorf("%q not offered: %v", want, got)
		}
	}
}

// Without this, `hecate explain -namespace uidemo <TAB>` lists the Gates of
// whatever the kubeconfig selects — every namespace except the one just named.
func TestCompletionHonoursANamespaceAlreadyTyped(t *testing.T) {
	for _, args := range [][]string{
		{"explain", "-namespace", "uidemo", ""},
		{"explain", "--namespace", "uidemo", ""},
		{"explain", "-namespace=uidemo", ""},
		{"explain", "--namespace=uidemo", ""},
	} {
		if got := namespaceIn(args[:len(args)-1]); got != "uidemo" {
			t.Errorf("%v → namespace %q, want uidemo", args, got)
		}
	}
}

// Completion runs on a keypress. An unreachable cluster must produce nothing,
// not a message the shell would offer as a candidate.
func TestCompletionIsSilentWhenTheClusterIsUnreachable(t *testing.T) {
	t.Setenv("KUBECONFIG", "/nonexistent/kubeconfig")
	var code int
	out := capture(t, func() { code = complete(context.Background(), []string{"explain", ""}) })
	if code != exitOK {
		t.Errorf("exit = %d — a shell would treat a non-zero completion as breakage", code)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("offered %q with no cluster to ask", out)
	}
}

func TestCompletionScriptsMentionTheHiddenCommand(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		out := capture(t, func() { completionScript(shell) })
		// Every shell asks the same binary the same question, which is what
		// stops the three drifting apart.
		if !strings.Contains(out, "hecate __complete") {
			t.Errorf("%s script does not call hecate __complete:\n%s", shell, out)
		}
	}
	var code int
	capture(t, func() { code = completionScript("powershell") })
	if code != exitUsage {
		t.Errorf("exit = %d for an unsupported shell, want %d", code, exitUsage)
	}
}

func TestManPageIsValidAsciiRoff(t *testing.T) {
	out := capture(t, func() { manPage() })

	for _, want := range []string{`.TH HECATE 1`, ".SH NAME", ".SH SYNOPSIS", ".SH COMMANDS", ".SH EXIT STATUS"} {
		if !strings.Contains(out, want) {
			t.Errorf("man page has no %s", want)
		}
	}
	// Generated from the same text `hecate help` prints, so the two cannot
	// disagree about what a command does.
	if !strings.Contains(out, "hecate verify <bundle>") {
		t.Error("the man page does not carry the usage text")
	}
	// ASCII only: a raw em dash renders doubled through preconv and is mangled
	// entirely by -Tascii.
	for i, r := range out {
		if r > 127 {
			t.Fatalf("non-ASCII %q at byte %d — roff escapes exist for a reason", r, i)
		}
	}
	if !strings.Contains(out, `\(em`) {
		t.Error("the em dash was not escaped")
	}
}
