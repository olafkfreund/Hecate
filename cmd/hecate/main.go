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
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(os.Args) < 2 {
		usage()
		os.Exit(exitUsage)
	}

	switch os.Args[1] {
	case "verify":
		os.Exit(verify(ctx, os.Args[2:]))
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

func usage() {
	fmt.Println(`hecate — the promotion layer for FluxCD

Usage:
  hecate verify <bundle>     verify the evidence for every crossing of a Bundle
  hecate verify --trail <id> verify one Fides trail directly
  hecate version

Fides connection:
  --server        Fides server URL      (or $FIDES_SERVER_URL)
  --token         Fides API key         (or $FIDES_TOKEN)

Exit codes:
  0  every chain verified
  1  usage
  2  could not check
  3  a chain is broken — the evidence has been tampered with
  4  nothing to verify: no trail has been recorded`)
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
