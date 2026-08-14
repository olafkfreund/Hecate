// Command hecate-mcp lets an LLM client ask Hecate what is going on.
//
// It speaks the Model Context Protocol over stdio. By default it only reads.
// With --allow-writes it can also promote and abort, subject to exactly the
// rules every other path obeys.
//
// It can never approve. Approval is a segregation-of-duties control, and an
// agent that can satisfy it makes four-eyes theatre — so that is not a setting
// this server has.
//
// It is a transport over pkg/ops and holds no rules of its own, so what it
// reports and what `hecate` prints cannot disagree.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	corev1 "k8s.io/api/core/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/mcp"
	"github.com/olafkfreund/hecate/pkg/ops"
)

// version is set at build time with -ldflags.
var version = "dev"

// instructions is what a client shows the model about this server. It is the
// only chance to say what the vocabulary means before the model starts
// guessing from tool names.
// instructionsFor is what a client shows the model about this server. It is the
// only chance to say what the vocabulary means before the model starts guessing
// from tool names — and, when writes are on, the only place to say plainly what
// it must not try to do.
func instructionsFor(allowWrites bool) string {
	instructions := `Hecate is the promotion layer for FluxCD. Its vocabulary:

  Beacon   watches registries and repositories, and emits a Bundle when something new appears
  Bundle   an immutable set of artifact versions — the thing that moves
  Gate     an environment, and the threshold a Bundle must cross to enter it
  Passage  one attempt to move one Bundle through one Gate

Hecate never applies anything to a cluster itself: a Passage writes to git, Flux
syncs, and Hecate reads the result back.

When asked why something has not deployed, call why_stuck on the Gate rather
than piecing it together from list_gates and get_passage — it returns the
blockers already identified, each with the command that would clear it.
`

	if !allowWrites {
		return instructions + `
This server is read-only. It cannot promote, approve or abort; say so rather
than implying an action was taken.`
	}

	return instructions + `
This server can promote and abort. It cannot approve, and there is no setting
that would let it: approval is a segregation-of-duties control, and an agent
that could satisfy it would make the control meaningless. When a Bundle is
waiting for approval, say so and let a person approve it — do not look for
another route.

Promotion is judged by the same rules as an automatic crossing. A refusal is an
answer, not an obstacle: report it rather than retrying or trying a different
Gate.`
}

func main() {
	// Logging goes to stderr because stdout carries the protocol, and one stray
	// line there corrupts the stream in a way whose failure points elsewhere.
	fs := flag.NewFlagSet("hecate-mcp", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	namespace := fs.String("namespace", "", "default namespace for tools that do not name one")
	allowWrites := fs.Bool("allow-writes", false,
		"expose promote and abort. Off by default: a tool a model can call is a tool it will call.")
	actor := fs.String("actor", "",
		"who authorised this server to act, recorded on every write. Required with --allow-writes.")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if *showVersion {
		fmt.Println(version)
		return
	}

	if err := run(*namespace, *allowWrites, *actor); err != nil {
		fmt.Fprintf(os.Stderr, "hecate-mcp: %s\n", err)
		os.Exit(1)
	}
}

func run(namespace string, allowWrites bool, actor string) error {
	if allowWrites && actor == "" {
		// A write with no actor is a write nobody is accountable for, and an
		// agent acting anonymously is exactly the case where that matters.
		return fmt.Errorf("--allow-writes needs --actor: every write records who authorised it")
	}
	if !allowWrites && actor != "" {
		return fmt.Errorf("--actor has no effect without --allow-writes")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("no Kubernetes configuration: %w", err)
	}
	// corev1 as well as v1alpha1: recording an approval reads the Fides
	// credentials from a Secret, and a scheme without it fails at that point
	// with "no kind is registered for the type v1.Secret" rather than at
	// startup. The CLI had the same gap (cmd/hecate/status.go).
	scheme := k8sruntime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return err
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		return err
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("connecting to the cluster: %w", err)
	}

	if namespace == "" {
		namespace = kubeconfigNamespace()
	}

	o := ops.New(c)
	server := mcp.New("hecate", version, instructionsFor(allowWrites))
	server.MustRegister(mcp.ReadTools(o, namespace)...)

	mode := "read-only"
	if allowWrites {
		server.MustRegister(mcp.WriteTools(o, namespace, actor)...)
		mode = "writes enabled as " + mcp.ActorPrefix + actor
	}

	fmt.Fprintf(os.Stderr, "hecate-mcp %s serving %s over stdio (%s)\n", version, namespace, mode)
	return server.Serve(ctx, os.Stdin, os.Stdout)
}

// kubeconfigNamespace is the namespace the current context selects, so the
// server covers the same ground as kubectl in the same shell.
func kubeconfigNamespace() string {
	cfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{})
	if ns, _, err := cfg.Namespace(); err == nil && ns != "" {
		return ns
	}
	return "default"
}
