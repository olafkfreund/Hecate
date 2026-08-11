// Command hecate-controller runs the Hecate control plane: the Beacon, Gate and
// Passage controllers, plus the health-checker and step registries they use.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/beacon"
	"github.com/olafkfreund/hecate/pkg/gate"
	"github.com/olafkfreund/hecate/pkg/health"
	"github.com/olafkfreund/hecate/pkg/passage"
	"github.com/olafkfreund/hecate/pkg/passage/steps"
)

// version is set at build time with -ldflags.
var version = "dev"

type options struct {
	metricsAddr string
	probeAddr   string
	leaderElect bool
	leaderID    string
	workRoot    string
	noCrossNS   bool
	fidesServer string
	showVersion bool
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
	stepRunners.MustRegister(steps.NewGitPullRequest(mgr.GetClient()))
	stepRunners.MustRegister(steps.NewFluxReconcile(mgr.GetClient(), !opts.noCrossNS))
	stepRunners.MustRegister(steps.NewHTTP(mgr.GetClient()))
	stepRunners.MustRegister(steps.NewEvidenceGate(mgr.GetClient(), opts.fidesServer))

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
		"crossNamespaceRefs", map[bool]string{true: "refused", false: "allowed"}[opts.noCrossNS])

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		return fmt.Errorf("running manager: %w", err)
	}
	return nil
}

// newScheme registers everything the controllers read or write: Hecate's own
// types, plus core types for the Secrets credentials are read from and the
// Events the controllers emit.
func newScheme() *k8sruntime.Scheme {
	scheme := k8sruntime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
	return scheme
}
