// Package git answers "what credentials apply to this repository?" for
// everything in Hecate that talks to one.
//
// It exists because two things do: a step cloning and pushing a promotion, and
// a Beacon watching a repository for new commits. They are given the same
// `credentialsRef` pointing at the same Secret, so a second implementation
// would eventually disagree with the first about what that Secret means —
// which shows up as a Beacon that cannot see a repository its own promotion
// step writes to every day.
package git

import (
	"context"
	"fmt"
	"os"

	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// Auth builds a transport auth method from a Secret, or nil for public
// repositories and ambient credentials.
func Auth(
	ctx context.Context, c client.Client, namespace string, ref *v1alpha1.LocalSecretRef,
) (transport.AuthMethod, error) {
	if ref == nil {
		return nil, nil
	}
	if c == nil {
		return nil, fmt.Errorf("credentialsRef %q set but there is no client to read it with", ref.Name)
	}

	var secret corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, &secret); err != nil {
		return nil, fmt.Errorf("reading credentials Secret %s/%s: %w", namespace, ref.Name, err)
	}
	return AuthFromSecret(&secret)
}

// AuthFromSecret is Auth once the Secret is in hand.
func AuthFromSecret(secret *corev1.Secret) (transport.AuthMethod, error) {
	// An SSH key wins when present: a Secret carrying both is almost certainly
	// an SSH secret with a username left over from a template.
	if key, ok := secret.Data["identity"]; ok {
		user := string(secret.Data["username"])
		if user == "" {
			user = "git"
		}
		auth, err := gitssh.NewPublicKeys(user, key, string(secret.Data["password"]))
		if err != nil {
			return nil, fmt.Errorf("secret %s: unusable SSH key: %w", secret.Name, err)
		}
		if hosts, ok := secret.Data["known_hosts"]; ok {
			cb, err := KnownHostsCallback(hosts)
			if err != nil {
				return nil, fmt.Errorf("secret %s: %w", secret.Name, err)
			}
			auth.HostKeyCallback = cb
		}
		return auth, nil
	}

	username, password := string(secret.Data["username"]), string(secret.Data["password"])
	if password == "" {
		return nil, fmt.Errorf(
			"no usable credentials in Secret %s: expected identity, or username and password",
			secret.Name)
	}
	if username == "" {
		// Most hosts accept any username with a token; GitHub wants a literal
		// placeholder rather than an empty string.
		username = "git"
	}
	return &githttp.BasicAuth{Username: username, Password: password}, nil
}

// KnownHostsCallback builds a host-key checker from a known_hosts file.
//
// Written to a temp file because golang.org/x/crypto/ssh's parser takes a path.
// Verification is not optional: skipping it would accept any host key and make
// the SSH transport trivially interceptable.
func KnownHostsCallback(hosts []byte) (ssh.HostKeyCallback, error) {
	f, err := os.CreateTemp("", "hecate-known-hosts-*")
	if err != nil {
		return nil, fmt.Errorf("known_hosts: %w", err)
	}
	defer func() { _ = os.Remove(f.Name()) }()

	if _, err := f.Write(hosts); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("known_hosts: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("known_hosts: %w", err)
	}
	cb, err := knownhosts.New(f.Name())
	if err != nil {
		return nil, fmt.Errorf("known_hosts is unusable: %w", err)
	}
	return cb, nil
}
