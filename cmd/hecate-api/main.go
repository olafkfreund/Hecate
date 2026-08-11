// Command hecate-api serves Hecate's HTTP API.
//
// It exists for the surfaces that cannot hold a kubeconfig — a web UI, a
// dashboard, a bot — and it is a transport over pkg/ops, so what it answers and
// what `hecate` prints cannot disagree.
//
// It authenticates nobody itself. A caller presents a Kubernetes bearer token,
// and Hecate asks the API server who they are and whether they may act. If your
// cluster authenticates with OIDC then those are OIDC tokens and single sign-on
// already works; Hecate did not have to know (#73).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/api"
	"github.com/olafkfreund/hecate/pkg/ops"
)

// version is set at build time with -ldflags.
var version = "dev"

func main() {
	fs := flag.NewFlagSet("hecate-api", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	addr := fs.String("addr", ":8080", "address to listen on")
	certFile := fs.String("tls-cert-file", "", "serve HTTPS with this certificate")
	keyFile := fs.String("tls-private-key-file", "", "the key for --tls-cert-file")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if *showVersion {
		fmt.Println(version)
		return
	}

	if err := run(*addr, *certFile, *keyFile); err != nil {
		fmt.Fprintf(os.Stderr, "hecate-api: %s\n", err)
		os.Exit(1)
	}
}

func run(addr, certFile, keyFile string) error {
	if (certFile == "") != (keyFile == "") {
		return errors.New("--tls-cert-file and --tls-private-key-file go together")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("no Kubernetes configuration: %w", err)
	}
	sch := k8sruntime.NewScheme()
	if err := v1alpha1.AddToScheme(sch); err != nil {
		return err
	}
	// TokenReview and SubjectAccessReview live in the built-in groups, so the
	// scheme needs them too — without this the server would start and then fail
	// on the first request it tried to authenticate.
	if err := scheme.AddToScheme(sch); err != nil {
		return err
	}
	c, err := client.New(cfg, client.Options{Scheme: sch})
	if err != nil {
		return fmt.Errorf("connecting to the cluster: %w", err)
	}

	server := &api.Server{
		Ops:     ops.New(c),
		Auth:    &api.Authenticator{Client: c},
		Version: version,
	}

	httpServer := &http.Server{
		Addr:    addr,
		Handler: server.Handler(),
		// A caller that opens a connection and says nothing must not hold a slot
		// indefinitely.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	protocol := "http"
	if certFile != "" {
		protocol = "https"
	}
	fmt.Fprintf(os.Stderr, "hecate-api %s listening on %s (%s)\n", version, addr, protocol)
	if certFile == "" {
		fmt.Fprintln(os.Stderr,
			"warning: serving plain HTTP — bearer tokens will cross the network in clear. "+
				"Use --tls-cert-file, or terminate TLS in front of this.")
	}

	errs := make(chan error, 1)
	go func() {
		if certFile != "" {
			errs <- httpServer.ListenAndServeTLS(certFile, keyFile)
			return
		}
		errs <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-errs:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		// In-flight requests get a moment to finish: a promotion cut off
		// mid-write is the one thing a restart should not do.
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdown)
	}
}
