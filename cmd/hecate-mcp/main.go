// Command hecate-mcp lets an LLM client ask Hecate what is going on.
//
// It speaks the Model Context Protocol over stdio and is read-only: it can
// report what every Gate is doing and why one is stuck, and it cannot promote,
// approve or abort anything. Those are gated separately, because an agent that
// can satisfy a four-eyes approval makes four-eyes theatre.
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
const instructions = `Hecate is the promotion layer for FluxCD. Its vocabulary:

  Beacon   watches registries and repositories, and emits a Bundle when something new appears
  Bundle   an immutable set of artifact versions — the thing that moves
  Gate     an environment, and the threshold a Bundle must cross to enter it
  Passage  one attempt to move one Bundle through one Gate

Hecate never applies anything to a cluster itself: a Passage writes to git, Flux
syncs, and Hecate reads the result back.

When asked why something has not deployed, call why_stuck on the Gate rather
than piecing it together from list_gates and get_passage — it returns the
blockers already identified, each with the command that would clear it.

This server is read-only. It cannot promote, approve or abort; say so rather
than implying an action was taken.`

func main() {
	// Logging goes to stderr because stdout carries the protocol, and one stray
	// line there corrupts the stream in a way whose failure points elsewhere.
	fs := flag.NewFlagSet("hecate-mcp", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	namespace := fs.String("namespace", "", "default namespace for tools that do not name one")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if *showVersion {
		fmt.Println(version)
		return
	}

	if err := run(*namespace); err != nil {
		fmt.Fprintf(os.Stderr, "hecate-mcp: %s\n", err)
		os.Exit(1)
	}
}

func run(namespace string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("no Kubernetes configuration: %w", err)
	}
	scheme := k8sruntime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return err
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("connecting to the cluster: %w", err)
	}

	if namespace == "" {
		namespace = kubeconfigNamespace()
	}

	server := mcp.New("hecate", version, instructions)
	server.MustRegister(mcp.ReadTools(ops.New(c), namespace)...)

	fmt.Fprintf(os.Stderr, "hecate-mcp %s serving %s over stdio (read-only)\n", version, namespace)
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
