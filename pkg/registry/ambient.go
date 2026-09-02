package registry

import (
	"context"
	"encoding/base64"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/google/go-containerregistry/pkg/authn"
	googlekeychain "github.com/google/go-containerregistry/pkg/v1/google"
)

// ambientKeychain is what a workload identity provides, beyond docker config
// on disk: AWS IRSA for ECR and GCP Workload Identity for GCR/Artifact
// Registry. Azure Managed Identity is not wired — see D56.
//
// authn.DefaultKeychain stays first. Both cloud keychains only ever fire for
// their own registry's hostname (see isPrivateECR and google.Keychain's
// isGoogle), so there is no real precedence question between them, but
// docker config is what
// CI's `docker login` and a developer's laptop already rely on, and a Secret
// left over from either must keep working exactly as it did before this file
// existed.
func ambientKeychain() authn.Keychain {
	return authn.NewMultiKeychain(authn.DefaultKeychain, googlekeychain.Keychain, ecrKeychain{})
}

// ecrAuthTimeout bounds the one network call this keychain can make. Without
// it, an environment with no AWS credentials at all — no IRSA env vars, no
// shared config, no EC2 instance — falls through the SDK's credential chain to
// the EC2 IMDS endpoint, which is unreachable outside AWS and would otherwise
// leave a non-ECR-hosted registry push hanging on a timeout nobody asked for.
//
// ponytail: a flat timeout, not a context derived from the caller's. Good
// enough for a credential fetch; revisit if a slow IMDS response ever needs to
// race the caller's own deadline instead of its own.
const ecrAuthTimeout = 5 * time.Second

// ecrKeychain resolves ECR credentials from whatever ambient AWS identity is
// available — IRSA on EKS, or a shared config/instance profile anywhere else
// the AWS SDK's default chain looks.
type ecrKeychain struct{}

func (k ecrKeychain) Resolve(target authn.Resource) (authn.Authenticator, error) {
	return k.ResolveContext(context.Background(), target)
}

// ResolveContext implements authn.ContextKeychain.
//
// No caching: this fires once per WatchSource per Beacon reconcile (default
// interval 5m, no per-layer or per-tag looping — pkg/beacon/resolve.go calls
// Keychain once per watch, not once per artifact) and once per oci-push/pull
// step execution. That is nowhere near GetAuthorizationToken's rate limit.
//
// ponytail: no token cache/expiry, add one keyed by account+region if a
// Beacon's poll interval ever drops low enough, or enough Beacons exist, to
// make this call frequently — GetAuthorizationToken is throttled harder than
// the ~12h token lifetime implies.
func (k ecrKeychain) ResolveContext(ctx context.Context, target authn.Resource) (authn.Authenticator, error) {
	host := target.RegistryStr()
	if host == ecrPublicHost {
		// public.ecr.aws is a different service (ecr-public, pinned to
		// us-east-1) that this keychain does not call — and Anonymous is the
		// right credential for a public registry anyway, so there is nothing to
		// wire here rather than something to fall back from.
		return authn.Anonymous, nil
	}
	if !isPrivateECR(host) {
		return authn.Anonymous, nil
	}

	ctx, cancel := context.WithTimeout(ctx, ecrAuthTimeout)
	defer cancel()

	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		// No usable AWS identity. Not this keychain's job to report that as an
		// error — DefaultKeychain or a referenced Secret gets the next say.
		return authn.Anonymous, nil
	}

	out, err := ecr.NewFromConfig(cfg).GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil || len(out.AuthorizationData) == 0 || out.AuthorizationData[0].AuthorizationToken == nil {
		return authn.Anonymous, nil
	}

	decoded, err := base64.StdEncoding.DecodeString(aws.ToString(out.AuthorizationData[0].AuthorizationToken))
	if err != nil {
		return authn.Anonymous, nil
	}
	user, pass, ok := strings.Cut(string(decoded), ":")
	if !ok {
		return authn.Anonymous, nil
	}
	return authn.FromConfig(authn.AuthConfig{Username: user, Password: pass}), nil
}

// ecrPublicHost is public ECR's registry host. It needs no AWS call at all:
// anonymous pulls are how public ECR is meant to be used, and its
// authorization API (ecr-public, region-pinned to us-east-1) is a different
// service from private ECR's — wiring it is future work, not this keychain's.
const ecrPublicHost = "public.ecr.aws"

// isPrivateECR reports whether host is a private ECR registry:
// <account>.dkr.ecr.<region>.amazonaws.com.
func isPrivateECR(host string) bool {
	return strings.Contains(host, ".dkr.ecr.") && strings.HasSuffix(host, ".amazonaws.com")
}

// IsAmbientCloudRegistry reports whether host is one ambientKeychain actually
// covers — used by the registry-matrix test to tell "not wired" apart from
// "wired but unprovable without real cloud infrastructure" (#50).
//
// public.ecr.aws counts: Anonymous is the credential ambientKeychain gives it,
// deliberately, not by omission.
func IsAmbientCloudRegistry(host string) bool {
	return host == ecrPublicHost || isPrivateECR(host) || isGoogleRegistry(host)
}

// isGoogleRegistry mirrors google.Keychain's own unexported host match —
// duplicated rather than imported because it isn't exported, and it is four
// suffix checks that change only if Google renames a product.
func isGoogleRegistry(host string) bool {
	return host == "gcr.io" ||
		strings.HasSuffix(host, ".gcr.io") ||
		strings.HasSuffix(host, ".pkg.dev") ||
		strings.HasSuffix(host, ".google.com")
}
