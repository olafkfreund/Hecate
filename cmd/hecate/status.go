package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/ops"
)

// operations builds an Ops against the cluster the kubeconfig points at.
//
// Every command here goes through it: the CLI formats, it does not decide.
// A rule implemented here would be a second answer to a question the API server
// and the MCP server also answer.
func operations() (*ops.Ops, error) {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("no Kubernetes configuration: %w", err)
	}
	scheme := k8sruntime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("connecting to the cluster: %w", err)
	}
	return ops.New(c), nil
}

// namespaceFlag adds -n, defaulted from the kubeconfig's current context so the
// CLI behaves like kubectl rather than demanding a flag kubectl would infer.
func namespaceFlag(fs *flag.FlagSet) *string {
	return fs.String("namespace", kubeconfigNamespace(), "namespace to work in")
}

// kubeconfigNamespace is the namespace the current context selects, so `hecate
// status` covers the same ground as `kubectl get` in the same shell.
func kubeconfigNamespace() string {
	cfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{})
	if ns, _, err := cfg.Namespace(); err == nil && ns != "" {
		return ns
	}
	return "default"
}

// status lists Gates and what each is doing.
func status(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	namespace := namespaceFlag(fs)
	asJSON := fs.Bool("json", false, "emit JSON")
	fs.Usage = usage
	rest, err := parseArgs(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(rest) > 0 {
		return fail(exitUsage, "status takes no arguments — did you mean `hecate explain %s`?", rest[0])
	}

	o, err := operations()
	if err != nil {
		return fail(exitError, "%s", err)
	}

	gates, err := o.Gates(ctx, *namespace)
	if err != nil {
		return fail(exitError, "%s", err)
	}
	if len(gates) == 0 {
		fmt.Printf("no Gates in %s\n", *namespace)
		return exitOK
	}

	// One explanation per Gate: the state is derived, and deriving it here
	// rather than reading status.conditions is what keeps the CLI and the UI
	// saying the same thing.
	explanations := make([]*ops.Explanation, 0, len(gates))
	for i := range gates {
		ex, err := o.Explain(ctx, *namespace, gates[i].Name)
		if err != nil {
			return fail(exitError, "%s", err)
		}
		explanations = append(explanations, ex)
	}

	if *asJSON {
		return emit(explanations)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	// Writes to a tabwriter are buffered; Flush below reports any failure.
	_, _ = fmt.Fprintln(w, "GATE\tSTATE\tCURRENT\tHEALTH\tSUMMARY")
	for _, ex := range explanations {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			ex.Gate, ex.State, dash(ex.Current), dash(string(ex.Health)), ex.Summary)
	}
	return flush(w)
}

// explain answers "why is this Gate not crossing?".
func explain(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("explain", flag.ContinueOnError)
	namespace := namespaceFlag(fs)
	asJSON := fs.Bool("json", false, "emit JSON")
	fs.Usage = usage
	rest, err := parseArgs(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(rest) != 1 {
		return fail(exitUsage, "explain needs a Gate name")
	}

	o, err := operations()
	if err != nil {
		return fail(exitError, "%s", err)
	}

	ex, err := o.Explain(ctx, *namespace, rest[0])
	if err != nil {
		return fail(exitError, "%s", err)
	}
	if *asJSON {
		return emit(ex)
	}

	fmt.Printf("%s is %s\n%s\n", ex.Gate, ex.State, ex.Summary)

	if len(ex.Blockers) > 0 {
		fmt.Println()
		for _, b := range ex.Blockers {
			fmt.Printf("  [%s] %s\n", b.Kind, b.Detail)
			if b.Fix != "" {
				fmt.Printf("      → %s\n", b.Fix)
			}
		}
	}
	if len(ex.Eligible) > 0 {
		fmt.Printf("\n  eligible: %s\n", strings.Join(ex.Eligible, ", "))
	}
	for _, w := range ex.Waiting {
		fmt.Printf("  waiting:  %s — %s\n", w.Bundle, w.Reason)
	}
	return exitOK
}

// emit writes a value as indented JSON, for scripts and for the UI's benefit
// during development.
func emit(v any) int {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fail(exitError, "%s", err)
	}
	return exitOK
}

func flush(w *tabwriter.Writer) int {
	if err := w.Flush(); err != nil {
		return fail(exitError, "%s", err)
	}
	return exitOK
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
