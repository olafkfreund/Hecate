// Command hecate is the operator's side of Hecate.
//
// Today it does one thing: verify that a promotion's evidence has not been
// tampered with. An audit trail nobody can check is a log file with better
// marketing, so the check belongs in the hands of the person relying on it
// rather than in our documentation.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

// version is set at build time with -ldflags.
var version = "dev"

// Exit codes. Distinct so a pipeline can tell "the evidence is bad" from "I
// could not look", which are very different things to page someone about.
const (
	exitOK      = 0
	exitUsage   = 1
	exitError   = 2
	exitBroken  = 3
	exitNoTrail = 4
	// exitRefused is the rules saying no — a Bundle that has not cleared
	// upstream, a closed window. Distinct from exitError because it is an
	// answer, not a malfunction, and a pipeline branches on the difference.
	exitRefused = 5
	// exitCrossingFailed is a crossing that ran and did not land. Distinct from
	// exitError, which means Hecate could not tell you whether it did — a
	// pipeline treats "the promotion failed" and "I lost the cluster" very
	// differently, and only reports the first as a deployment failure.
	exitCrossingFailed = 6
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(os.Args) < 2 {
		usage()
		os.Exit(exitUsage)
	}

	switch os.Args[1] {
	case "status":
		os.Exit(status(ctx, os.Args[2:]))
	case "explain":
		os.Exit(explain(ctx, os.Args[2:]))
	case "promote":
		os.Exit(promote(ctx, os.Args[2:]))
	case "approve":
		os.Exit(approve(ctx, os.Args[2:]))
	case "abort":
		os.Exit(abort(ctx, os.Args[2:]))
	case "verify":
		os.Exit(verify(ctx, os.Args[2:]))
	case "completion":
		if len(os.Args) != 3 {
			os.Exit(fail(exitUsage, "completion needs a shell: bash, zsh or fish"))
		}
		os.Exit(completionScript(os.Args[2]))
	case "man":
		os.Exit(manPage())
	// Undocumented on purpose: the shells call it, people do not. Listing it in
	// the help would invite someone to script against a format that exists to
	// serve `complete` and may change with it.
	case "__complete":
		os.Exit(complete(ctx, os.Args[2:]))
	case "version", "--version", "-v":
		fmt.Println(version)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "hecate: no command named %q\n\n", os.Args[1])
		usage()
		os.Exit(exitUsage)
	}
}

func usage() { fmt.Println(usageText()) }

// invokedAs is how the user typed this binary.
//
// Installed as a Flux CLI plugin it is reached as `flux hecate`, and telling
// that user to run `hecate promote ...` names a binary that is very likely not
// on their PATH at all — the plugin lives in ~/.fluxcd/plugins and nothing puts
// it there.
//
// Only this command's own text is adjusted. The fixes in pkg/ops still say
// `hecate`, deliberately: the UI renders those same strings, and there is no
// `flux hecate` in a browser.
func invokedAs() string {
	if filepath.Base(os.Args[0]) == "flux-hecate" {
		return "flux hecate"
	}
	return "hecate"
}

// usageText is the help, kept as a value so `hecate man` can render the same
// words rather than a second description that drifts from this one.
func usageText() string {
	// Only the indented command lines are rewritten. The title is the product's
	// name and stays as it is.
	return strings.ReplaceAll(`hecate — the promotion layer for FluxCD

Usage:
  hecate status                     what every Gate is doing
  hecate explain <gate>             why a Gate is not crossing, and what to do
  hecate promote <gate> --bundle B  ask a Gate to cross a Bundle
  hecate approve <bundle> --gate G  approve a Bundle for one Gate
  hecate abort <passage>            stop a crossing that is under way
  hecate verify <bundle>            verify the evidence for every crossing of it
  hecate verify --trail <id>        verify one Fides trail directly
  hecate completion <shell>         bash, zsh or fish completion
  hecate man                        the man page, for a packager to install
  hecate version

Common flags:
  -namespace      namespace to work in  (default: the kubeconfig's)
  -output, -o     table, json or yaml   (every command)
  -watch          follow the crossing to its end  (promote)
  -timeout        give up watching after this     (promote -watch)
  -ai             add a model's summary (explain)
  -actor          who to record         (default: the current user)

Model (optional; explain -ai only). Everything works without one:
  HECATE_LLM_URL    OpenAI-compatible base URL, e.g. http://localhost:11434/v1
  HECATE_LLM_MODEL  model name, e.g. llama3.2
  HECATE_LLM_API_KEY  only if the endpoint wants one

Fides connection (verify only):
  -server         Fides server URL      (or $FIDES_SERVER_URL)
  -token          Fides API key         (or $FIDES_TOKEN)

Exit codes:
  0  fine
  1  usage
  2  could not check
  3  a chain is broken — the evidence has been tampered with
  4  nothing to verify: no trail has been recorded
  5  refused: the rules do not allow it`, "\n  hecate ", "\n  "+invokedAs()+" ")
}

// parseArgs parses flags that appear before *or* after positional arguments,
// and returns the positionals.
//
// Go's flag package stops at the first non-flag argument, so
// `hecate promote production --bundle b1` would treat `--bundle` and `b1` as
// two more positionals and the flag would go unset. That is the natural order,
// and it is the order `hecate explain` prints in its own suggestions — a hint
// the tool then rejects is worse than no hint.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	for rest := fs.Args(); len(rest) > 0; rest = fs.Args() {
		positional = append(positional, rest[0])
		if err := fs.Parse(rest[1:]); err != nil {
			return nil, err
		}
	}
	return positional, nil
}

// fail prints an error the way a CLI should — no stack, no "Error:" prefix
// shouting — and returns the code to exit with.
func fail(code int, format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "hecate: "+format+"\n", args...)
	return code
}

// flagSet gives every command the same connection flags, defaulted from the
// environment so a shell that already talks to Fides needs no arguments.
func flagSet(name string) (*flag.FlagSet, *string, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	server := fs.String("server", os.Getenv("FIDES_SERVER_URL"), "Fides server URL")
	token := fs.String("token", os.Getenv("FIDES_TOKEN"), "Fides API key")
	return fs, server, token
}
