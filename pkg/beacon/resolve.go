package beacon

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/registry"
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
		return r.resolveChart(ctx, namespace, *w.Chart)
	case w.Git != nil:
		return r.resolveGit(ctx, namespace, *w.Git)
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

	repo, err := name.NewRepository(w.Repo, registry.NameOptions(w.Insecure)...)
	if err != nil {
		return v1alpha1.Artifact{}, fmt.Errorf("invalid repository %q: %w", w.Repo, err)
	}

	keychain, err := r.keychain(ctx, namespace, w.CredentialsRef)
	if err != nil {
		return v1alpha1.Artifact{}, err
	}
	opts := registry.RemoteOptions(ctx, keychain)

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

// keychain defers to pkg/registry, so a Beacon resolving a tag and a step
// pushing an artifact answer "what credentials apply here?" the same way.
func (r *Resolver) keychain(ctx context.Context, namespace string, ref *v1alpha1.LocalSecretRef) (authn.Keychain, error) {
	return registry.Keychain(ctx, r.Client, namespace, ref)
}
