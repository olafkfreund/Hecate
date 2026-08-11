package beacon

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// Resolver turns a WatchSource into the concrete Artifact currently at its head.
//
// One struct rather than an interface with a registry: there are three watch
// kinds, they are enumerated in the API, and nothing outside this package
// implements one. An abstraction here would be indirection with no second
// implementation to justify it.
type Resolver struct {
	// Client reads credential Secrets. May be nil for public sources only.
	Client client.Client
}

// ErrUnsupported reports a watch kind or option that is declared in the API but
// not implemented. Distinct from a failure: the configuration is valid, we just
// cannot honour it yet, and saying so beats resolving something plausible and
// wrong.
type ErrUnsupported struct{ What string }

func (e *ErrUnsupported) Error() string { return e.What + " is not implemented yet" }

// Resolve returns the Artifact a WatchSource currently points at.
func (r *Resolver) Resolve(ctx context.Context, namespace string, w v1alpha1.WatchSource) (v1alpha1.Artifact, error) {
	switch {
	case w.Image != nil:
		return r.resolveImage(ctx, namespace, *w.Image)
	case w.Chart != nil:
		return v1alpha1.Artifact{}, &ErrUnsupported{What: "chart watches"}
	case w.Git != nil:
		return v1alpha1.Artifact{}, &ErrUnsupported{What: "git watches"}
	default:
		return v1alpha1.Artifact{}, fmt.Errorf("watch source has no image, chart or git set")
	}
}

func (r *Resolver) resolveImage(ctx context.Context, namespace string, w v1alpha1.ImageWatch) (v1alpha1.Artifact, error) {
	if w.Platform != "" {
		// Declared in the API but unimplemented. Silently ignoring it would pin
		// the index digest while the user believes they pinned one platform.
		return v1alpha1.Artifact{}, &ErrUnsupported{What: "platform-specific digest resolution"}
	}

	repo, err := name.NewRepository(w.Repo)
	if err != nil {
		return v1alpha1.Artifact{}, fmt.Errorf("invalid repository %q: %w", w.Repo, err)
	}

	keychain, err := r.keychain(ctx, namespace, w.CredentialsRef)
	if err != nil {
		return v1alpha1.Artifact{}, err
	}
	opts := []remote.Option{remote.WithContext(ctx), remote.WithAuthFromKeychain(keychain)}

	tags, err := remote.List(repo, opts...)
	if err != nil {
		return v1alpha1.Artifact{}, fmt.Errorf("listing tags for %s: %w", w.Repo, err)
	}

	tag, err := Selection{
		Strategy:   w.Select,
		Constraint: w.Constraint,
		Allow:      w.Allow,
		Ignore:     w.Ignore,
	}.Pick(tags)
	if err != nil {
		return v1alpha1.Artifact{}, err
	}

	// Head rather than Get: we want the digest, not the manifest body. For a
	// multi-arch image this is the index digest, which is the correct thing to
	// pin — it is what a cluster would pull.
	desc, err := remote.Head(repo.Tag(tag), opts...)
	if err != nil {
		return v1alpha1.Artifact{}, fmt.Errorf("resolving digest for %s:%s: %w", w.Repo, tag, err)
	}

	return v1alpha1.Artifact{Image: &v1alpha1.ImageArtifact{
		Repo:   w.Repo,
		Tag:    tag,
		Digest: desc.Digest.String(),
	}}, nil
}

// keychain builds the auth chain: the referenced Secret first, then ambient
// credentials (docker config on disk, and the cloud keychains that cover IRSA,
// Workload Identity and Managed Identity).
func (r *Resolver) keychain(ctx context.Context, namespace string, ref *v1alpha1.LocalSecretRef) (authn.Keychain, error) {
	if ref == nil {
		return authn.DefaultKeychain, nil
	}
	if r.Client == nil {
		return nil, fmt.Errorf("credentialsRef %q set but the resolver has no client", ref.Name)
	}

	var secret corev1.Secret
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, &secret); err != nil {
		return nil, fmt.Errorf("reading credentials Secret %s/%s: %w", namespace, ref.Name, err)
	}

	kc, err := keychainFromSecret(&secret)
	if err != nil {
		return nil, err
	}
	return authn.NewMultiKeychain(kc, authn.DefaultKeychain), nil
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

// keychainFromSecret accepts both shapes Kubernetes users actually have: a
// `kubernetes.io/dockerconfigjson` image pull secret, and a plain
// username/password Secret.
func keychainFromSecret(secret *corev1.Secret) (authn.Keychain, error) {
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
		if len(s) > len(prefix) && s[:len(prefix)] == prefix {
			return s[len(prefix):]
		}
	}
	return s
}
