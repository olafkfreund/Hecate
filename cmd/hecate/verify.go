package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/fides"
)

// verify checks the attestation chain behind a promotion.
func verify(ctx context.Context, args []string) int {
	fs, server, token := flagSet("verify")
	trail := fs.String("trail", "", "verify this Fides trail directly, without looking at a Bundle")
	namespace := fs.String("namespace", "", "namespace of the Bundle (default: the kubeconfig's)")
	fs.Usage = usage
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	client, err := fides.New(fides.Config{BaseURL: *server, Token: *token})
	if err != nil {
		return fail(exitUsage, "%s\n\nSet --server and --token, or FIDES_SERVER_URL and FIDES_TOKEN.", err)
	}

	if *trail != "" {
		if fs.NArg() > 0 {
			return fail(exitUsage, "give either a Bundle or --trail, not both")
		}
		return report(one(ctx, client, crossing{Trail: *trail}))
	}
	if fs.NArg() != 1 {
		return fail(exitUsage, "verify needs a Bundle name, or --trail <id>")
	}

	crossings, code := crossingsOf(ctx, fs.Arg(0), *namespace)
	if code != exitOK {
		return code
	}
	if len(crossings) == 0 {
		// Said plainly rather than reported as success. "Nothing to verify" and
		// "verified" are the same output to a careless reader and opposite
		// facts to an auditor.
		fmt.Printf("%s has no recorded evidence — no crossing of it wrote a Fides trail.\n", fs.Arg(0))
		return exitNoTrail
	}

	results := make([]result, 0, len(crossings))
	for _, c := range crossings {
		results = append(results, one(ctx, client, c))
	}
	return report(results...)
}

// crossing is one Passage's evidence: which Gate it crossed, and the trail it
// recorded.
type crossing struct {
	Gate    string
	Passage string
	Trail   string
}

// result is what verification concluded about one crossing.
type result struct {
	crossing
	chain *fides.Chain
	err   error
}

func one(ctx context.Context, c *fides.Client, x crossing) result {
	chain, err := c.VerifyChain(ctx, x.Trail)
	return result{crossing: x, chain: chain, err: err}
}

// report prints every result and returns the exit code for the worst of them.
// Every trail is checked before returning: stopping at the first broken chain
// would hide how much of the history is affected.
func report(results ...result) int {
	code := exitOK
	for _, r := range results {
		where := r.Gate
		if where == "" {
			where = "trail " + short(r.Trail)
		} else {
			where = fmt.Sprintf("%s (trail %s)", r.Gate, short(r.Trail))
		}

		switch {
		case r.err != nil:
			fmt.Printf("? %s\n  %s\n", where, r.err)
			code = worst(code, exitError)

		case !r.chain.Valid:
			fmt.Printf("✗ %s\n  chain BROKEN at entry %d: %s\n", where, r.chain.BrokenAt, reason(r.chain))
			code = worst(code, exitBroken)

		case r.chain.Count == 0:
			// Fides answers 200 with {"valid":true,"count":0} for a trail that
			// does not exist — verifying an empty chain is vacuously true. An
			// empty trail proves nothing either way, so calling it verified is
			// the false green this command exists to avoid.
			fmt.Printf("? %s\n  no attestations — the trail is empty, or does not exist\n", where)
			code = worst(code, exitNoTrail)

		default:
			fmt.Printf("✓ %s\n  chain valid — %s%s\n", where,
				plural(r.chain.Count, "attestation"), anchorNote(r.chain.ExternalAnchor))
		}
	}
	return code
}

func reason(c *fides.Chain) string {
	if c.Reason != "" {
		return c.Reason
	}
	return "no reason given"
}

// anchorNote reports the external timestamp, because a chain only we vouch for
// is a weaker claim than one an independent authority saw at a point in time.
func anchorNote(a *fides.Anchor) string {
	switch {
	case a == nil || !a.Anchored:
		return ", not externally anchored"
	case !a.HeadMatches:
		// The anchor exists but covers a different head: either the chain has
		// grown since, or it was rewritten under the anchor.
		return fmt.Sprintf(", anchored %s but the anchor covers a different chain head",
			a.AnchoredAt.Format("2006-01-02"))
	default:
		return fmt.Sprintf(", anchored %s", a.AnchoredAt.Format("2006-01-02"))
	}
}

// crossingsOf finds every Passage that moved this Bundle and recorded a trail.
func crossingsOf(ctx context.Context, bundle, namespace string) ([]crossing, int) {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, fail(exitError, "no Kubernetes configuration: %s", err)
	}
	scheme := k8sruntime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, fail(exitError, "%s", err)
	}
	kube, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fail(exitError, "connecting to the cluster: %s", err)
	}

	var opts []client.ListOption
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	var passages v1alpha1.PassageList
	if err := kube.List(ctx, &passages, opts...); err != nil {
		return nil, fail(exitError, "listing Passages: %s", err)
	}

	var found []crossing
	for _, p := range passages.Items {
		if p.Spec.Bundle != bundle || p.Status.Evidence == nil || p.Status.Evidence.Trail == "" {
			continue
		}
		found = append(found, crossing{Gate: p.Spec.Gate, Passage: p.Name, Trail: p.Status.Evidence.Trail})
	}
	// Ordered by Gate so repeated runs print the same thing — a diffable
	// report is worth more to an auditor than one in list order.
	sort.Slice(found, func(i, j int) bool {
		if found[i].Gate != found[j].Gate {
			return found[i].Gate < found[j].Gate
		}
		return found[i].Passage < found[j].Passage
	})
	return found, exitOK
}

func worst(a, b int) int {
	if b > a {
		return b
	}
	return a
}

func short(id string) string {
	if i := strings.Index(id, "-"); i > 0 {
		return id[:i]
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
