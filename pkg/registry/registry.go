// Package registry is how Hecate authenticates to an OCI registry.
//
// One implementation, because there is one question — "what credentials apply
// to this registry?" — and a Beacon resolving an image tag and a step pushing
// an artifact must not answer it differently. It lives here rather than in
// either caller for the same reason pkg/ops exists (D32).
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// Keychain builds the auth chain: the referenced Secret first, then ambient
// credentials — docker config on disk, AWS IRSA for ECR, and GCP Workload
// Identity for GCR/Artifact Registry. Azure Managed Identity is not wired yet
// (D56); an AKS workload still needs a referenced Secret or a docker config.
func Keychain(
	ctx context.Context, c client.Client, namespace string, ref *v1alpha1.LocalSecretRef,
) (authn.Keychain, error) {
	if ref == nil {
		return ambientKeychain(), nil
	}
	if c == nil {
		return nil, fmt.Errorf("credentialsRef %q set but there is no client to read it with", ref.Name)
	}

	var secret corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, &secret); err != nil {
		return nil, fmt.Errorf("reading credentials Secret %s/%s: %w", namespace, ref.Name, err)
	}

	kc, err := KeychainFromSecret(&secret)
	if err != nil {
		return nil, err
	}
	return authn.NewMultiKeychain(kc, ambientKeychain()), nil
}

// NameOptions are the reference-parsing options for a registry.
//
// Insecure is what allows a plain-HTTP registry — an internal one that
// terminates TLS elsewhere, an air-gapped one, or a local development loop.
// It must always be something the author asked for: silently falling back to
// HTTP would send registry credentials in clear.
func NameOptions(insecure bool) []name.Option {
	if insecure {
		return []name.Option{name.Insecure}
	}
	return nil
}

// RemoteOptions are the transport options for a registry call.
func RemoteOptions(ctx context.Context, kc authn.Keychain) []remote.Option {
	return []remote.Option{remote.WithContext(ctx), remote.WithAuthFromKeychain(kc)}
}

// secretKeychain resolves credentials for registries named in a Secret.
type secretKeychain map[string]authn.AuthConfig

func (s secretKeychain) Resolve(target authn.Resource) (authn.Authenticator, error) {
	for _, key := range []string{target.String(), target.RegistryStr()} {
		if cfg, ok := s[key]; ok {
			return authn.FromConfig(cfg), nil
		}
	}
	return authn.Anonymous, nil
}

// KeychainFromSecret accepts both shapes Kubernetes users actually have: a
// `kubernetes.io/dockerconfigjson` image pull secret, and a plain
// username/password Secret.
func KeychainFromSecret(secret *corev1.Secret) (authn.Keychain, error) {
	if raw, ok := secret.Data[corev1.DockerConfigJsonKey]; ok {
		var cfg struct {
			Auths map[string]authn.AuthConfig `json:"auths"`
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("malformed %s in Secret %s: %w", corev1.DockerConfigJsonKey, secret.Name, err)
		}
		kc := secretKeychain{}
		for registry, auth := range cfg.Auths {
			if reg, err := name.NewRegistry(stripScheme(registry)); err == nil {
				kc[reg.RegistryStr()] = auth
			}
		}
		return kc, nil
	}

	username, password := string(secret.Data["username"]), string(secret.Data["password"])
	if username == "" && password == "" {
		return nil, fmt.Errorf(
			"no usable credentials in Secret %s: expected %s, or username and password",
			secret.Name, corev1.DockerConfigJsonKey)
	}
	// No registry key, so these credentials apply to whatever is asked for.
	return anyRegistryKeychain{authn.AuthConfig{Username: username, Password: password}}, nil
}

type anyRegistryKeychain struct{ cfg authn.AuthConfig }

func (k anyRegistryKeychain) Resolve(authn.Resource) (authn.Authenticator, error) {
	return authn.FromConfig(k.cfg), nil
}

// stripScheme tolerates the `https://index.docker.io/v1/` style entries that
// real dockerconfigjson files contain.
func stripScheme(s string) string {
	for _, prefix := range []string{"https://", "http://"} {
		if after, found := strings.CutPrefix(s, prefix); found && after != "" {
			return after
		}
	}
	return s
}
