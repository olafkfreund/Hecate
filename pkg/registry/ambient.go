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
// their own registry's hostname (see isECR and google.Keychain's isGoogle), so
// there is no real precedence question between them, but docker config is what
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
func (k ecrKeychain) ResolveContext(ctx context.Context, target authn.Resource) (authn.Authenticator, error) {
	if !isECR(target.RegistryStr()) {
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

// isECR reports whether host is an ECR registry, public or private:
// <account>.dkr.ecr.<region>.amazonaws.com, or public.ecr.aws.
func isECR(host string) bool {
	return host == "public.ecr.aws" ||
		(strings.Contains(host, ".dkr.ecr.") && strings.HasSuffix(host, ".amazonaws.com"))
}

// IsAmbientCloudRegistry reports whether host is one ambientKeychain actually
// covers — used by the registry-matrix test to tell "not wired" apart from
// "wired but unprovable without real cloud infrastructure" (#50).
func IsAmbientCloudRegistry(host string) bool {
	return isECR(host) || isGoogleRegistry(host)
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
