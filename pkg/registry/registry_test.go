package registry

import (
	"testing"

	"github.com/google/go-containerregistry/pkg/name"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestKeychainFromSecret(t *testing.T) {
	t.Run("dockerconfigjson", func(t *testing.T) {
		kc, err := KeychainFromSecret(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "pull"},
			Data: map[string][]byte{
				corev1.DockerConfigJsonKey: []byte(
					`{"auths":{"https://ghcr.io":{"username":"u","password":"p"}}}`),
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		reg, _ := name.NewRegistry("ghcr.io")
		auth, err := kc.Resolve(reg)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := auth.Authorization()
		if err != nil {
			t.Fatal(err)
		}
		// The https:// prefix real dockerconfigjson files carry must not defeat
		// the lookup.
		if cfg.Username != "u" || cfg.Password != "p" {
			t.Errorf("got %+v, want username u / password p", cfg)
		}
	})

	t.Run("username and password", func(t *testing.T) {
		kc, err := KeychainFromSecret(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "basic"},
			Data:       map[string][]byte{"username": []byte("u"), "password": []byte("p")},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		reg, _ := name.NewRegistry("example.com")
		auth, _ := kc.Resolve(reg)
		cfg, _ := auth.Authorization()
		if cfg.Username != "u" {
			t.Errorf("got %+v, want username u", cfg)
		}
	})

	t.Run("empty secret is an error", func(t *testing.T) {
		_, err := KeychainFromSecret(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "empty"},
			Data:       map[string][]byte{},
		})
		if err == nil {
			t.Error("expected an error for a Secret with no usable credentials")
		}
	})
}

// Real dockerconfigjson files carry `https://index.docker.io/v1/` style keys,
// so the scheme has to come off before the registry can be matched.
func TestStripScheme(t *testing.T) {
	for in, want := range map[string]string{
		"https://index.docker.io/v1/": "index.docker.io/v1/",
		"http://localhost:5000":       "localhost:5000",
		"ghcr.io":                     "ghcr.io",
		"https://":                    "https://",
	} {
		if got := stripScheme(in); got != want {
			t.Errorf("stripScheme(%q) = %q, want %q", in, got, want)
		}
	}
}

// One implementation, one answer: a Beacon resolving a tag and a step pushing an
// artifact must not disagree about which credentials apply.
//
// Asserted on the resulting scheme rather than on the number of options, which
// is what this used to check: a count passes just as happily if NameOptions
// returns the wrong option, and the thing worth defending is the effect.
//
// A *named* host, because that is the only case that discriminates. Loopback
// resolves to http whether or not the option is set — go-containerregistry
// treats it as insecure on its own — so a test using an httptest server would
// pass with the option wired to nothing at all.
func TestInsecureIsOptIn(t *testing.T) {
	for _, tc := range []struct {
		host, secure, insecure string
	}{
		{"reg.internal:5000", "https", "http"},
		{"ghcr.io", "https", "http"},
		// Recorded rather than required: loopback cannot tell the two apart, and
		// anyone writing the obvious test needs to find that out here.
		{"127.0.0.1:5000", "http", "http"},
		{"localhost:5000", "http", "http"},
	} {
		t.Run(tc.host, func(t *testing.T) {
			for insecure, want := range map[bool]string{false: tc.secure, true: tc.insecure} {
				repo, err := name.NewRepository(tc.host+"/acme/api", NameOptions(insecure)...)
				if err != nil {
					t.Fatal(err)
				}
				if got := repo.Scheme(); got != want {
					t.Errorf("insecure=%v: scheme = %s, want %s", insecure, got, want)
				}
			}
		})
	}
}
