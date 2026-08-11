package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/user"

	"github.com/olafkfreund/hecate/pkg/ops"
)

// whoami is who an action is recorded as having been taken by.
//
// The OS user is a default, not an identity claim: anyone can set $USER. Real
// authentication belongs to the API server (#73), and until it exists this is
// honest about being a convenience — it records who ran the command on the
// machine, which is what a local kubectl-style tool can know.
func whoami(override string) string {
	if override != "" {
		return override
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if name := os.Getenv("USER"); name != "" {
		return name
	}
	return ""
}

func actorFlag(fs *flag.FlagSet) *string {
	return fs.String("actor", "", "who to record as taking this action (default: the current user)")
}

// promote asks a Gate to cross a Bundle.
func promote(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("promote", flag.ContinueOnError)
	namespace := namespaceFlag(fs)
	bundle := fs.String("bundle", "", "the Bundle to promote")
	actor := actorFlag(fs)
	fs.Usage = usage
	rest, err := parseArgs(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(rest) != 1 {
		return fail(exitUsage, "promote needs a Gate name")
	}
	if *bundle == "" {
		return fail(exitUsage, "promote needs --bundle")
	}

	o, err := operations()
	if err != nil {
		return fail(exitError, "%s", err)
	}

	p, err := o.Promote(ctx, *namespace, rest[0], *bundle, whoami(*actor))
	if err != nil {
		return actionFailed("promote", err)
	}
	fmt.Printf("crossing %s through %s as %s\n", p.Spec.Bundle, p.Spec.Gate, p.Name)
	return exitOK
}

// approve records a human approval of a Bundle for a Gate.
func approve(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("approve", flag.ContinueOnError)
	namespace := namespaceFlag(fs)
	gate := fs.String("gate", "", "the Gate to approve it for")
	actor := actorFlag(fs)
	fs.Usage = usage
	rest, err := parseArgs(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(rest) != 1 {
		return fail(exitUsage, "approve needs a Bundle name")
	}
	if *gate == "" {
		// Deliberately required: approval is per Gate, and guessing would
		// approve a Bundle for somewhere nobody agreed to.
		return fail(exitUsage, "approve needs --gate — an approval is for one Gate, not for the Bundle")
	}

	o, err := operations()
	if err != nil {
		return fail(exitError, "%s", err)
	}

	who := whoami(*actor)
	if err := o.Approve(ctx, *namespace, rest[0], *gate, who); err != nil {
		return actionFailed("approve", err)
	}
	fmt.Printf("%s approved for %s by %s\n", rest[0], *gate, who)
	return exitOK
}

// abort asks a running Passage to stop.
func abort(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("abort", flag.ContinueOnError)
	namespace := namespaceFlag(fs)
	actor := actorFlag(fs)
	fs.Usage = usage
	rest, err := parseArgs(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(rest) != 1 {
		return fail(exitUsage, "abort needs a Passage name")
	}

	o, err := operations()
	if err != nil {
		return fail(exitError, "%s", err)
	}

	if err := o.Abort(ctx, *namespace, rest[0], whoami(*actor)); err != nil {
		return actionFailed("abort", err)
	}
	fmt.Printf("asked %s to stop; the controller will mark its remaining steps aborted\n", rest[0])
	return exitOK
}

// actionFailed distinguishes "the rules say no" from "it did not work".
//
// A refusal is an answer — the Bundle has not cleared staging, the window is
// closed — and printing it as an error with a stack of context would bury the
// one line that matters.
func actionFailed(action string, err error) int {
	if ops.IsRefused(err) || ops.IsNotFound(err) {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		return exitRefused
	}
	return fail(exitError, "%s: %s", action, err)
}
