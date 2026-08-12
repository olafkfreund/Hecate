// Command hecate-controller runs the Hecate control plane: the Beacon, Gate and
// Passage controllers, plus the health-checker and step registries they use.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/go-logr/logr"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/discovery"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/beacon"
	"github.com/olafkfreund/hecate/pkg/crds"
	"github.com/olafkfreund/hecate/pkg/gate"
	"github.com/olafkfreund/hecate/pkg/health"
	"github.com/olafkfreund/hecate/pkg/passage"
	"github.com/olafkfreund/hecate/pkg/passage/steps"
	"github.com/olafkfreund/hecate/pkg/telemetry"
)

// warnFluxAPIs reports Flux API versions the cluster does not serve.
//
// The mitigation D4 lacked: reading Flux as unstructured means a removed API
// version cannot break our build, so without this it surfaces at reconcile time
// as "no matches for kind" against a Gate that worked yesterday. CI covers the
// Flux versions we test; this covers the one the operator is actually running.
//
// Nothing here is fatal, including its own failure. A controller that will not
// start because discovery was slow is a worse outcome than an unchecked
// assumption.
func warnFluxAPIs(cfg *rest.Config, logger logr.Logger) {
	d, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		logger.Info("could not check the cluster's Flux API versions", "error", err)
		return
	}
	warnings, err := health.CheckFluxAPIs(d)
	if err != nil {
		logger.Info("could not check the cluster's Flux API versions", "error", err)
		return
	}
	for _, w := range warnings {
		logger.Info("Flux API mismatch: " + w)
	}
}

// version is set at build time with -ldflags.
var version = "dev"

type options struct {
	metricsAddr  string
	probeAddr    string
	leaderElect  bool
	leaderID     string
	workRoot     string
	noCrossNS    bool
	skipCRDCheck bool
	fidesServer  string
	showVersion  bool
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "hecate-controller: %s\n", err)
		os.Exit(1)
	}
}

func run() error {
	var opts options
	flag.StringVar(&opts.metricsAddr, "metrics-bind-address", ":8080",
		"Address the metrics endpoint binds to. Set to 0 to disable.")
	flag.StringVar(&opts.probeAddr, "health-probe-bind-address", ":8081",
		"Address the health and readiness probes bind to.")
	flag.BoolVar(&opts.leaderElect, "leader-elect", false,
		"Elect a leader before acting, so only one replica reconciles at a time.")
	flag.StringVar(&opts.leaderID, "leader-election-id", "hecate.hecate.dev",
		"Name of the resource used for leader election.")
	flag.StringVar(&opts.workRoot, "work-root", filepath.Join(os.TempDir(), "hecate-passages"),
		"Base directory for Passage scratch space. Contents are disposable; steps must tolerate it being empty.")
	flag.BoolVar(&opts.noCrossNS, "no-cross-namespace-refs", true,
		"Refuse a Gate watching resources outside its own namespace. Matches Flux's own "+
			"posture; set false only on a single-tenant cluster.")
	flag.StringVar(&opts.fidesServer, "fides-server", os.Getenv("FIDES_SERVER_URL"),
		"Default Fides server for evidence-gate steps. A Gate may override it with "+
			"evidence.serverURL; a fleet with one Fides sets it here instead of on every Gate.")
	flag.BoolVar(&opts.skipCRDCheck, "skip-crd-check", false,
		"Start even if the cluster's CRDs are older than this build. The escape hatch for "+
			"someone who cannot apply CRDs; expect fields to be silently dropped.")
	flag.BoolVar(&opts.showVersion, "version", false, "Print the version and exit.")

	zapOpts := zap.Options{Development: false}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	if opts.showVersion {
		fmt.Println(version)
		return nil
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))
	logger := ctrl.Log.WithName("setup")

	// GetConfig rather than GetConfigOrDie: the "OrDie" form calls os.Exit and
	// bypasses run()'s error path, producing a stack-flavoured message instead
	// of "hecate-controller: <what went wrong>".
	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("loading Kubernetes configuration: %w", err)
	}

	// Before the manager, so a stale API is a failed rollout rather than a
	// controller that runs and misbehaves. Helm never upgrades a chart's CRDs,
	// and the API server prunes unknown fields silently (#117).
	ctx := ctrl.SetupSignalHandler()
	if err := checkCRDs(ctx, restCfg, opts.skipCRDCheck, logger); err != nil {
		return err
	}

	// After our own CRDs and before the manager: a Flux we do not recognise is
	// a warning rather than a failure, because refusing to start would turn one
	// degraded watch into a total outage (#92).
	warnFluxAPIs(restCfg, logger)

	// Tracing is configured entirely by the standard OTEL_* environment and is
	// off unless one of them is set, so this is a no-op for anyone not running a
	// collector.
	shutdownTracing, tracing, err := telemetry.Start(ctx, "hecate-controller", version)
	if err != nil {
		return err
	}
	defer func() {
		// A fresh context: the signal context is already cancelled by the time
		// this runs, and flushing the last spans is the whole point.
		if err := shutdownTracing(context.WithoutCancel(ctx)); err != nil {
			logger.Error(err, "could not flush traces on shutdown")
		}
	}()

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                 newScheme(),
		Metrics:                metricsserver.Options{BindAddress: opts.metricsAddr},
		HealthProbeBindAddress: opts.probeAddr,
		LeaderElection:         opts.leaderElect,
		LeaderElectionID:       opts.leaderID,
	})
	if err != nil {
		return fmt.Errorf("creating manager: %w", err)
	}

	// One Flux checker, used in two places: the health registry assesses Gates
	// with it, and the flux-wait step reuses it so a Passage waits on exactly
	// what the Gate will go on to watch.
	fluxChecker := health.NewFluxChecker(mgr.GetClient()).
		AllowingCrossNamespace(!opts.noCrossNS)

	checkers := health.NewRegistry()
	checkers.MustRegister(fluxChecker)

	stepRunners := passage.NewRegistry()
	stepRunners.MustRegister(steps.NewFluxWait(fluxChecker))
	stepRunners.MustRegister(steps.NewGitClone(mgr.GetClient()))
	stepRunners.MustRegister(steps.NewGitCommit())
	stepRunners.MustRegister(steps.NewGitPush(mgr.GetClient()))
	stepRunners.MustRegister(steps.NewEditYAML())
	stepRunners.MustRegister(steps.NewSetImage())
	stepRunners.MustRegister(steps.NewRenderKustomize())
	stepRunners.MustRegister(steps.NewRenderHelm())
	stepRunners.MustRegister(steps.NewOCIPush(mgr.GetClient()))
	stepRunners.MustRegister(steps.NewOCIPull(mgr.GetClient()))
	stepRunners.MustRegister(steps.NewGitPullRequest(mgr.GetClient()))
	stepRunners.MustRegister(steps.NewFluxReconcile(mgr.GetClient(), !opts.noCrossNS))
	stepRunners.MustRegister(steps.NewHTTP(mgr.GetClient()))
	stepRunners.MustRegister(steps.NewEvidenceGate(mgr.GetClient(), opts.fidesServer))
	stepRunners.MustRegister(steps.NewCommitStatus(mgr.GetClient()))

	if err := (&beacon.Reconciler{
		Client:   mgr.GetClient(),
		Resolver: &beacon.Resolver{Client: mgr.GetClient()},
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setting up the Beacon controller: %w", err)
	}

	if err := (&gate.Reconciler{
		Client: mgr.GetClient(),
		Health: checkers,
		// The same registry the engine runs from, so a Gate is judged against
		// exactly the steps that would execute it.
		Steps: stepRunners,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setting up the Gate controller: %w", err)
	}

	if err := (&passage.Reconciler{
		Client:   mgr.GetClient(),
		Engine:   &passage.Engine{Registry: stepRunners},
		WorkRoot: opts.workRoot,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setting up the Passage controller: %w", err)
	}

	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return fmt.Errorf("adding health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("ping", healthz.Ping); err != nil {
		return fmt.Errorf("adding readiness check: %w", err)
	}

	logger.Info("starting hecate-controller",
		"version", version,
		"checkers", checkers.Names(),
		"steps", stepRunners.Names(),
		"workRoot", opts.workRoot,
		"tracing", map[bool]string{true: "enabled", false: "off"}[tracing],
		"crossNamespaceRefs", map[bool]string{true: "refused", false: "allowed"}[opts.noCrossNS])

	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("running manager: %w", err)
	}
	return nil
}

// newScheme registers everything the controllers read or write: Hecate's own
// types, plus core types for the Secrets credentials are read from and the
// Events the controllers emit.
// checkCRDs compares the cluster's CRDs against the ones this build ships.
//
// Its own client rather than the manager's: the manager's is cache-backed and
// the cache is not running yet, and this has to answer before anything starts.
func checkCRDs(ctx context.Context, restCfg *rest.Config, skip bool, logger logr.Logger) error {
	scheme := k8sruntime.NewScheme()
	utilruntime.Must(apiextensionsv1.AddToScheme(scheme))
	c, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("connecting to the cluster: %w", err)
	}

	err = crds.Check(ctx, c)
	switch {
	case err == nil:
		return nil
	case skip:
		// Logged at every start, not once: someone who set this flag to get
		// past an upgrade should keep being told what they are running with.
		logger.Info("WARNING: starting against CRDs this build does not match, "+
			"because --skip-crd-check was set. Fields will be dropped silently.",
			"detail", err.Error())
		return nil
	default:
		return err
	}
}

func newScheme() *k8sruntime.Scheme {
	scheme := k8sruntime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
	return scheme
}
