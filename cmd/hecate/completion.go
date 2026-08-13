package main

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// commands are the user-facing verbs, in the order the usage text lists them.
var commands = []string{
	"status", "explain", "promote", "approve", "abort", "verify", "completion", "man", "version", "help",
}

// completesWith says which resource a command's positional argument names.
// Commands absent from here take no completable argument.
var completesWith = map[string]string{
	"explain": "gates",
	"promote": "gates",
	"approve": "bundles",
	"abort":   "passages",
	"verify":  "bundles",
}

// flagsFor lists each command's flags, so `hecate verify -<TAB>` offers the
// ones that command actually has rather than a union nobody can use.
var flagsFor = map[string][]string{
	"status":  {"-namespace", "-output"},
	"explain": {"-namespace", "-output", "-ai"},
	"promote": {"-namespace", "-bundle", "-watch", "-timeout", "-actor"},
	"approve": {"-namespace", "-gate", "-actor"},
	"abort":   {"-namespace", "-actor"},
	"verify":  {"-namespace", "-output", "-server", "-token", "-trail"},
}

// complete prints candidate completions, one per line, for the words typed so
// far. It is the single source every shell asks, so bash, zsh and fish cannot
// drift apart or fall behind a command added later.
//
// **It never reports an error.** Completion runs on a keypress: a cluster that
// is unreachable, or credentials that have expired, must produce no candidates
// rather than a message the shell would offer as a filename. Exit code is
// always zero for the same reason.
func complete(ctx context.Context, args []string) int {
	// args is everything after `hecate`, with the word being typed last —
	// empty when the cursor sits after a space.
	if len(args) == 0 {
		print(commands)
		return exitOK
	}

	word := args[len(args)-1]
	prior := args[:len(args)-1]

	// Completing the command itself.
	if len(prior) == 0 {
		print(prefixed(commands, word))
		return exitOK
	}
	command := prior[0]

	// A value for the flag immediately before the cursor.
	if len(prior) > 1 {
		switch strings.TrimLeft(prior[len(prior)-1], "-") {
		case "output", "o":
			print(prefixed([]string{formatTable, formatJSON, formatYAML}, word))
			return exitOK
		case "gate":
			print(prefixed(names(ctx, "gates", namespaceIn(prior)), word))
			return exitOK
		case "bundle":
			print(prefixed(names(ctx, "bundles", namespaceIn(prior)), word))
			return exitOK
		}
	}

	if strings.HasPrefix(word, "-") {
		print(prefixed(flagsFor[command], word))
		return exitOK
	}
	if command == "completion" {
		print(prefixed([]string{"bash", "zsh", "fish"}, word))
		return exitOK
	}
	if kind, ok := completesWith[command]; ok {
		print(prefixed(names(ctx, kind, namespaceIn(prior)), word))
	}
	return exitOK
}

func print(candidates []string) {
	for _, c := range candidates {
		fmt.Println(c)
	}
}

func prefixed(candidates []string, word string) []string {
	if word == "" {
		return candidates
	}
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if strings.HasPrefix(c, word) {
			out = append(out, c)
		}
	}
	return out
}

// namespaceIn finds the namespace already typed on the command line.
//
// Without this, `hecate explain -namespace uidemo <TAB>` offers the Gates of
// whatever namespace the kubeconfig selects — which is every namespace except
// the one the user just said they meant.
func namespaceIn(words []string) string {
	for i, w := range words {
		if (w == "-namespace" || w == "--namespace") && i+1 < len(words) {
			return words[i+1]
		}
		for _, prefix := range []string{"-namespace=", "--namespace="} {
			if after, ok := strings.CutPrefix(w, prefix); ok {
				return after
			}
		}
	}
	return kubeconfigNamespace()
}

// ponytail: no completion for -namespace. Listing namespaces needs a
// cluster-wide permission plenty of users do not have, to offer a value they
// already know — the one thing they had to decide before typing the command.
//
// names lists a kind's objects in the current namespace, or nothing at all when
// the cluster cannot be reached.
func names(ctx context.Context, kind, ns string) []string {
	o, err := operations()
	if err != nil {
		return nil
	}

	switch kind {
	case "gates":
		gates, err := o.Gates(ctx, ns)
		if err != nil {
			return nil
		}
		out := make([]string, 0, len(gates))
		for i := range gates {
			out = append(out, gates[i].Name)
		}
		return out
	case "bundles":
		bundles, err := o.Bundles(ctx, ns)
		if err != nil {
			return nil
		}
		out := make([]string, 0, len(bundles))
		for i := range bundles {
			out = append(out, bundles[i].Name)
		}
		return out
	case "passages":
		passages, err := o.Passages(ctx, ns, "", "")
		if err != nil {
			return nil
		}
		out := make([]string, 0, len(passages))
		for i := range passages {
			// Only the ones an abort could still affect. Offering finished
			// Passages would be a list that grows for ever and is wrong every
			// time it is used.
			if !passages[i].Status.Phase.Terminal() {
				out = append(out, passages[i].Name)
			}
		}
		return out
	}
	return nil
}

// completionScript emits the shell glue. Each one does the same thing: hand the
// typed words to `hecate __complete` and offer whatever comes back.
func completionScript(shell string) int {
	switch shell {
	case "bash":
		fmt.Print(`# hecate bash completion. Add to ~/.bashrc:
#   source <(hecate completion bash)
_hecate() {
  local IFS=$'\n'
  COMPREPLY=($(hecate __complete "${COMP_WORDS[@]:1}" 2>/dev/null))
}
complete -o default -F _hecate hecate
`)
	case "zsh":
		fmt.Print(`# hecate zsh completion. Add to ~/.zshrc:
#   source <(hecate completion zsh)
_hecate() {
  local -a candidates
  candidates=(${(f)"$(hecate __complete ${words[2,$CURRENT]} 2>/dev/null)"})
  compadd -a candidates
}
compdef _hecate hecate
`)
	case "fish":
		fmt.Print(`# hecate fish completion. Write to ~/.config/fish/completions/hecate.fish:
#   hecate completion fish > ~/.config/fish/completions/hecate.fish
complete -c hecate -f -a "(hecate __complete (commandline -opc)[2..-1] (commandline -ct) 2>/dev/null)"
`)
	default:
		return fail(exitUsage, "no completion for %q — expected bash, zsh or fish", shell)
	}
	return exitOK
}

// manPage writes a roff man page.
//
// Generated from the same usage text `hecate help` prints, rather than kept as
// a second document. A hand-written man page is one that disagrees with --help
// within a release or two, and the disagreement is always discovered by the
// person trusting the wrong one.
func manPage() int {
	var b strings.Builder
	b.WriteString(`.TH HECATE 1 "" "hecate ` + version + `" "User Commands"
.SH NAME
hecate \- the promotion and release-orchestration layer for FluxCD
.SH SYNOPSIS
.B hecate
.I command
[\fIarguments\fR] [\fIflags\fR]
.SH DESCRIPTION
Hecate moves a Bundle of artifact versions through Gates by writing to git,
and records the evidence for each crossing. This page is generated from the
same text
.B hecate help
prints.
.SH COMMANDS
.nf
`)
	// Indented literally: the usage text is already aligned into columns, and
	// re-flowing it into roff paragraphs would lose that alignment for nothing.
	b.WriteString(roffEscape(usageText()))
	b.WriteString(`.fi
.SH EXIT STATUS
.TP
.B 0
success
.TP
.B 1
usage error
.TP
.B 2
Hecate could not determine the answer
.TP
.B 3
an attestation chain has been tampered with
.TP
.B 4
no trail to verify
.TP
.B 5
the crossing was refused by a rule
.TP
.B 6
the crossing ran and failed
.SH SEE ALSO
.BR flux (1)
.SH BUGS
https://github.com/olafkfreund/Hecate/issues
`)
	if _, err := os.Stdout.WriteString(b.String()); err != nil {
		return fail(exitError, "writing the man page: %s", err)
	}
	return exitOK
}

// roffEscape makes the usage text safe to paste into a man page.
//
// Three things need it. A backslash is roff's escape character, so it has to be
// escaped first or the replacements below would be re-read as markup. A line
// starting with a dot or an apostrophe is a roff request and would vanish.
// And the em dash is emitted as \(em rather than as UTF-8, because a raw one
// renders doubled — the text says "hecate —— the promotion layer" — through
// preconv, which is what man(1) uses.
func roffEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\e`)
	s = strings.ReplaceAll(s, "—", `\(em`)

	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, ".") || strings.HasPrefix(line, "'") {
			lines[i] = `\&` + line
		}
	}
	return strings.Join(lines, "\n")
}
