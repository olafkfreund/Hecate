package health

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// kubeconfigKey is where a cluster Secret carries its kubeconfig. The name Flux
// uses for the same thing, so an operator who has wired one for a Kustomization
// does not have to learn a second convention.
const kubeconfigKey = "value"

// Clusters resolves a client for a remote cluster from a kubeconfig Secret.
//
// **The namespace rule is not relaxed by crossing a cluster boundary.** A Gate
// in `team-a` watching a remote cluster is still restricted to `team-a` there,
// because the alternative makes a cluster reference the way around the tenant
// rule (#85, D11) — team-a adds a kubeconfig and watches everything everywhere.
// The check lives in FluxConfig.Validate and needs no special case: it compares
// the resource's namespace with the Gate's, and which cluster the resource is
// in does not enter into it.
type Clusters struct {
	// Local reads the Secrets. Cluster credentials live in the Gate's own
	// namespace and are read with the controller's own rights.
	Local client.Client

	mu      sync.Mutex
	clients map[string]client.Client
}

// For returns a client for the cluster a Gate names, or the local client when
// it names none.
func (c *Clusters) For(ctx context.Context, local client.Client, namespace string, ref *v1alpha1.LocalSecretRef) (client.Client, error) {
	if ref == nil {
		return local, nil
	}
	if c.Local == nil {
		return nil, fmt.Errorf("clusterRef %q set but there is no client to read it with", ref.Name)
	}

	var secret corev1.Secret
	key := client.ObjectKey{Namespace: namespace, Name: ref.Name}
	if err := c.Local.Get(ctx, key, &secret); err != nil {
		return nil, fmt.Errorf("reading cluster Secret %s/%s: %w", namespace, ref.Name, err)
	}
	raw := secret.Data[kubeconfigKey]
	if len(raw) == 0 {
		return nil, fmt.Errorf(
			"the Secret %s has no %q — a cluster reference carries a kubeconfig under that "+
				"key, the same one Flux uses", ref.Name, kubeconfigKey)
	}

	// Keyed by content, not by Secret name: rotating a kubeconfig must produce
	// a new client rather than keep using credentials that have been replaced,
	// and two namespaces with the same Secret name must not share one.
	sum := sha256.Sum256(raw)
	id := hex.EncodeToString(sum[:])

	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.clients[id]; ok {
		return existing, nil
	}

	cfg, err := clientcmd.RESTConfigFromKubeConfig(raw)
	if err != nil {
		return nil, fmt.Errorf("the kubeconfig in Secret %s is unusable: %w", ref.Name, err)
	}
	// The local scheme, because the objects read remotely are the same Flux
	// kinds — and they are read as unstructured anyway (D4), so the scheme only
	// has to know how to talk, not what the fields mean.
	remote, err := client.New(cfg, client.Options{Scheme: c.Local.Scheme()})
	if err != nil {
		return nil, fmt.Errorf("connecting to the cluster in Secret %s: %w", ref.Name, err)
	}

	if c.clients == nil {
		c.clients = map[string]client.Client{}
	}
	c.clients[id] = remote
	return remote, nil
}
