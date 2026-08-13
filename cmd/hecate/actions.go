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

// actionResult is what a write command did, for a caller that has to act on it.
//
// The prose these commands print is for a person. A script promoting needs the
// Passage name to watch or verify it afterwards, and parsing it back out of
// "crossing X through Y as Z" is the failure the CLI principle names: if it
// cannot be scripted, it is not done.
type actionResult struct {
	Action    string `json:"action"`
	Namespace string `json:"namespace"`
	Gate      string `json:"gate,omitempty"`
	Bundle    string `json:"bundle,omitempty"`
	Passage   string `json:"passage,omitempty"`
	Actor     string `json:"actor,omitempty"`
}

// promote asks a Gate to cross a Bundle.
func promote(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("promote", flag.ContinueOnError)
	namespace := namespaceFlag(fs)
	bundle := fs.String("bundle", "", "the Bundle to promote")
	actor := actorFlag(fs)
	watch := fs.Bool("watch", false, "follow the crossing and exit on its outcome")
	format := outputFlag(fs)
	timeout := fs.Duration("timeout", 0, "give up watching after this; zero waits indefinitely")
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
	// The Passage name is the whole reason a script calls this: without it the
	// caller has to parse the sentence below to learn what to watch or verify.
	code := render(*format, actionResult{
		Action: "promote", Namespace: p.Namespace,
		Passage: p.Name, Gate: p.Spec.Gate, Bundle: p.Spec.Bundle, Actor: p.Spec.Actor,
	}, func() int {
		fmt.Printf("crossing %s through %s as %s\n", p.Spec.Bundle, p.Spec.Gate, p.Name)
		return exitOK
	})
	if code != exitOK {
		return code
	}
	if !*watch {
		return exitOK
	}
	return watchPassage(ctx, o, p.Namespace, p.Name, *timeout)
}

// approve records a human approval of a Bundle for a Gate.
func approve(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("approve", flag.ContinueOnError)
	namespace := namespaceFlag(fs)
	gate := fs.String("gate", "", "the Gate to approve it for")
	actor := actorFlag(fs)
	format := outputFlag(fs)
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
	return render(*format, actionResult{
		Action: "approve", Namespace: *namespace, Bundle: rest[0], Gate: *gate, Actor: who,
	}, func() int {
		fmt.Printf("%s approved for %s by %s\n", rest[0], *gate, who)
		return exitOK
	})
}

// abort asks a running Passage to stop.
func abort(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("abort", flag.ContinueOnError)
	namespace := namespaceFlag(fs)
	actor := actorFlag(fs)
	format := outputFlag(fs)
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

	who := whoami(*actor)
	if err := o.Abort(ctx, *namespace, rest[0], who); err != nil {
		return actionFailed("abort", err)
	}
	return render(*format, actionResult{
		Action: "abort", Namespace: *namespace, Passage: rest[0], Actor: who,
	}, func() int {
		fmt.Printf("asked %s to stop; the controller will mark its remaining steps aborted\n", rest[0])
		return exitOK
	})
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
