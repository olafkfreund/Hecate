package steps

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/passage"
	"github.com/olafkfreund/hecate/pkg/registry"
)

// Step names.
const (
	StepOCIPush = "oci-push"
	StepOCIPull = "oci-pull"
)

// Failure reasons for the OCI steps.
const (
	// ReasonRegistryAuthFailed means the registry rejected our credentials.
	ReasonRegistryAuthFailed = "RegistryAuthFailed"
	// ReasonRegistryFailed is any other registry failure.
	ReasonRegistryFailed = "RegistryFailed"
)

// The media types Flux's source-controller expects of an OCIRepository.
//
// Taken from an artifact `flux push artifact` produced rather than from
// documentation: an artifact Flux will not read is worth nothing, and these are
// the strings it actually looks for.
const (
	fluxConfigMediaType  = types.MediaType("application/vnd.cncf.flux.config.v1+json")
	fluxContentMediaType = types.MediaType("application/vnd.cncf.flux.content.v1.tar+gzip")
)

// OCIPushConfig is the `with:` block of an oci-push step.
type OCIPushConfig struct {
	// Path is the directory to package, relative to the Passage work dir.
	Path string `json:"path"`
	// Repo is the target repository, e.g. ghcr.io/acme/manifests.
	Repo string `json:"repo"`
	// Tag is what to publish it as.
	Tag string `json:"tag"`
	// Source and Revision are recorded as annotations, and are what an
	// OCIRepository reports as the revision it applied — so this is what makes
	// a deployment traceable back to what produced it.
	Source   string `json:"source,omitempty"`
	Revision string `json:"revision,omitempty"`
	// Insecure allows a plain-HTTP registry. Always opt-in: falling back
	// silently would send registry credentials in clear.
	Insecure       bool                     `json:"insecure,omitempty"`
	CredentialsRef *v1alpha1.LocalSecretRef `json:"credentialsRef,omitempty"`
}

// OCIPush packages a directory and publishes it as a Flux OCI artifact.
//
// This is the other rendezvous. The model is "Hecate writes to a versioned
// content store and Flux reads it back" — git is the usual store, not the
// required one, and this is what makes that true in practice rather than only
// in the documentation.
type OCIPush struct{ client client.Client }

// NewOCIPush returns an oci-push step.
func NewOCIPush(c client.Client) *OCIPush { return &OCIPush{client: c} }

// Name implements passage.Runner.
func (o *OCIPush) Name() string { return StepOCIPush }

// Run implements passage.Runner.
func (o *OCIPush) Run(ctx context.Context, sc *passage.StepContext) (passage.StepResult, error) {
	cfg, err := passage.DecodeConfig[OCIPushConfig](sc)
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepOCIPush, err)
	}
	if err := cfg.check(); err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepOCIPush, err)
	}

	dir, err := checkoutPath(sc.WorkDir, cfg.Path)
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepOCIPush, err)
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return passage.StepResult{}, passage.FailTerminal(ReasonFileNotFound,
			"%s: no directory at %s", StepOCIPush, cfg.Path)
	}

	// Timestamps come from the Passage, not the clock, for the same reason
	// commits do (D23): the artifact's digest is a hash of its content, and a
	// wall-clock timestamp would mint a new digest on every attempt. A re-run
	// would then republish the same manifests under a different digest and
	// Flux would treat it as a new revision.
	stamp := sc.StartedAt.UTC()
	if stamp.IsZero() {
		stamp = time.Unix(0, 0).UTC()
	}

	layer, err := tarball(dir, stamp)
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonRegistryFailed,
			"%s: packaging %s: %s", StepOCIPush, cfg.Path, err)
	}

	image, err := fluxArtifact(layer, cfg, stamp)
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonRegistryFailed,
			"%s: building the artifact: %s", StepOCIPush, err)
	}
	digest, err := image.Digest()
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonRegistryFailed,
			"%s: %s", StepOCIPush, err)
	}

	ref, err := name.NewTag(cfg.Repo+":"+cfg.Tag, registry.NameOptions(cfg.Insecure)...)
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig,
			"%s: %s:%s is not a valid reference: %s", StepOCIPush, cfg.Repo, cfg.Tag, err)
	}

	keychain, err := registry.Keychain(ctx, o.client, sc.Namespace, cfg.CredentialsRef)
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepOCIPush, err)
	}
	if err := remote.Write(ref, image, registry.RemoteOptions(ctx, keychain)...); err != nil {
		return passage.StepResult{}, registryError(StepOCIPush, "pushing to "+ref.String(), err)
	}

	return passage.StepResult{
		Phase:   v1alpha1.StepSucceeded,
		Message: fmt.Sprintf("pushed %s to %s@%s", cfg.Path, cfg.Repo, digest.String()),
		Output: map[string]any{
			"repo": cfg.Repo, "tag": cfg.Tag,
			"digest": digest.String(), "url": "oci://" + cfg.Repo,
		},
	}, nil
}

// OCIPullConfig is the `with:` block of an oci-pull step.
type OCIPullConfig struct {
	// Repo is the source repository.
	Repo string `json:"repo"`
	// Tag or Digest identifies what to pull. A digest is exact; a tag can move.
	Tag    string `json:"tag,omitempty"`
	Digest string `json:"digest,omitempty"`
	// Out is where to unpack it, relative to the Passage work dir.
	Out string `json:"out"`

	Insecure       bool                     `json:"insecure,omitempty"`
	CredentialsRef *v1alpha1.LocalSecretRef `json:"credentialsRef,omitempty"`
}

// OCIPull unpacks a Flux OCI artifact into the work dir, so the edit steps can
// change it and oci-push can publish it again.
type OCIPull struct{ client client.Client }

// NewOCIPull returns an oci-pull step.
func NewOCIPull(c client.Client) *OCIPull { return &OCIPull{client: c} }

// Name implements passage.Runner.
func (o *OCIPull) Name() string { return StepOCIPull }

// Run implements passage.Runner.
func (o *OCIPull) Run(ctx context.Context, sc *passage.StepContext) (passage.StepResult, error) {
	cfg, err := passage.DecodeConfig[OCIPullConfig](sc)
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepOCIPull, err)
	}
	if err := cfg.check(); err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepOCIPull, err)
	}

	out, err := checkoutPath(sc.WorkDir, cfg.Out)
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepOCIPull, err)
	}

	ref, err := cfg.reference()
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepOCIPull, err)
	}

	keychain, err := registry.Keychain(ctx, o.client, sc.Namespace, cfg.CredentialsRef)
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepOCIPull, err)
	}
	image, err := remote.Image(ref, registry.RemoteOptions(ctx, keychain)...)
	if err != nil {
		return passage.StepResult{}, registryError(StepOCIPull, "pulling "+ref.String(), err)
	}

	layers, err := image.Layers()
	if err != nil || len(layers) == 0 {
		return passage.StepResult{}, passage.FailTerminal(ReasonRegistryFailed,
			"%s: %s carries no content layer", StepOCIPull, ref.String())
	}

	// Re-entrant (D19): the directory is replaced rather than merged, so a
	// retry after a partial unpack does not leave a mixture of two artifacts.
	if err := os.RemoveAll(out); err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonRegistryFailed,
			"%s: clearing %s: %s", StepOCIPull, cfg.Out, err)
	}
	files, err := untar(layers[len(layers)-1], out)
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonRegistryFailed,
			"%s: unpacking into %s: %s", StepOCIPull, cfg.Out, err)
	}

	digest, _ := image.Digest()
	return passage.StepResult{
		Phase:   v1alpha1.StepSucceeded,
		Message: fmt.Sprintf("pulled %s into %s (%s)", ref.String(), cfg.Out, plural(files, "file")),
		Output:  map[string]any{"digest": digest.String(), "out": cfg.Out, "files": files},
	}, nil
}

// fluxArtifact assembles the image Flux's source-controller will accept.
func fluxArtifact(layer v1.Layer, cfg OCIPushConfig, stamp time.Time) (v1.Image, error) {
	image := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
	image = mutate.ConfigMediaType(image, fluxConfigMediaType)

	image, err := mutate.Append(image, mutate.Addendum{Layer: layer, MediaType: fluxContentMediaType})
	if err != nil {
		return nil, err
	}

	annotations := map[string]string{
		"org.opencontainers.image.created": stamp.Format(time.RFC3339),
	}
	if cfg.Source != "" {
		annotations["org.opencontainers.image.source"] = cfg.Source
	}
	if cfg.Revision != "" {
		annotations["org.opencontainers.image.revision"] = cfg.Revision
	}
	return mutate.Annotations(image, annotations).(v1.Image), nil
}

// tarball packages a directory as a deterministic gzipped tar.
//
// Every field that could vary between runs is fixed: entries are walked in
// lexical order, timestamps come from the Passage, and ownership is zeroed.
// Without that the digest changes on every attempt and Flux sees a new revision
// each time, which would make a retry look like a deployment.
func tarball(dir string, stamp time.Time) (v1.Layer, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == "." {
			return err
		}
		// Symlinks are skipped rather than followed: a link out of the packaged
		// directory would publish something the author did not choose.
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}

		header := &tar.Header{
			Name:    filepath.ToSlash(rel),
			Mode:    0o644,
			ModTime: stamp,
			Format:  tar.FormatGNU,
		}
		if info.IsDir() {
			header.Typeflag, header.Name, header.Mode = tar.TypeDir, header.Name+"/", 0o755
		} else {
			header.Typeflag, header.Size = tar.TypeReg, info.Size()
		}
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, err = tw.Write(body)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return static.NewLayer(buf.Bytes(), fluxContentMediaType), nil
}

// untar unpacks a layer into dir, and returns how many files it wrote.
func untar(layer v1.Layer, dir string) (int, error) {
	rc, err := layer.Uncompressed()
	if err != nil {
		return 0, err
	}
	defer func() { _ = rc.Close() }()

	var files int
	tr := tar.NewReader(rc)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return files, nil
		}
		if err != nil {
			return files, err
		}

		// The archive is remote content, so it is not trusted to stay inside the
		// directory. Refused rather than normalised: joining against a cleaned
		// absolute path would silently relocate `../../x` to `x`, which is safe
		// but hides that the artifact was malformed — and an artifact carrying
		// traversal entries is worth failing over, not quietly repairing.
		clean := filepath.Clean(header.Name)
		if clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) || filepath.IsAbs(clean) {
			return files, fmt.Errorf("entry %q escapes the target directory", header.Name)
		}
		target := filepath.Join(dir, clean)

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return files, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return files, err
			}
			body, err := io.ReadAll(io.LimitReader(tr, 64<<20))
			if err != nil {
				return files, err
			}
			if err := os.WriteFile(target, body, 0o644); err != nil {
				return files, err
			}
			files++
		}
	}
}

// reference resolves what to pull: a digest is exact, a tag can move.
func (c OCIPullConfig) reference() (name.Reference, error) {
	opts := registry.NameOptions(c.Insecure)
	if c.Digest != "" {
		return name.NewDigest(c.Repo+"@"+c.Digest, opts...)
	}
	return name.NewTag(c.Repo+":"+c.Tag, opts...)
}

func (c OCIPushConfig) check() error {
	switch {
	case c.Path == "":
		return fmt.Errorf("path is required")
	case c.Repo == "":
		return fmt.Errorf("repo is required")
	case c.Tag == "":
		return fmt.Errorf("tag is required")
	}
	return nil
}

func (c OCIPullConfig) check() error {
	switch {
	case c.Repo == "":
		return fmt.Errorf("repo is required")
	case c.Out == "":
		return fmt.Errorf("out is required")
	case c.Tag == "" && c.Digest == "":
		return fmt.Errorf("one of tag or digest is required")
	case c.Tag != "" && c.Digest != "":
		return fmt.Errorf("give one of tag or digest, not both")
	}
	return nil
}

// registryError separates a rejected credential from a registry having a bad
// day: one needs a new secret, the other needs a retry.
func registryError(step, what string, err error) error {
	text := err.Error()
	for _, sign := range []string{"UNAUTHORIZED", "DENIED", "401", "403", "authentication required"} {
		if strings.Contains(text, sign) {
			return passage.FailTerminal(ReasonRegistryAuthFailed, "%s: %s: %s", step, what, err)
		}
	}
	return passage.Fail(ReasonRegistryFailed, "%s: %s: %s", step, what, err)
}

// CheckConfig implements passage.ConfigChecker.
func (o *OCIPush) CheckConfig(raw json.RawMessage) error {
	cfg, err := passage.CheckConfig[OCIPushConfig](raw)
	if err != nil {
		return err
	}
	return cfg.check()
}

// CheckConfig implements passage.ConfigChecker.
func (o *OCIPull) CheckConfig(raw json.RawMessage) error {
	cfg, err := passage.CheckConfig[OCIPullConfig](raw)
	if err != nil {
		return err
	}
	return cfg.check()
}
