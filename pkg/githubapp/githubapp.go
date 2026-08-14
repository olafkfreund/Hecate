// Package githubapp turns a GitHub App's private key into short-lived
// installation tokens.
//
// **Why this exists.** A personal access token in a Secret is long-lived,
// broadly scoped, and rotated by whoever remembers. An installation token is
// scoped to the installation and expires in an hour, which is the difference
// between a leaked credential being an incident and being a nuisance. For a
// tool whose pitch is that promotion should be evidence-gated rather than
// merge-rights-gated, holding a permanent write credential to every fleet
// repository is the weakest part of the threat model (#118).
//
// An installation token works both as an API token and as a git password, so
// one source feeds the provider APIs and the git steps that push. Two
// credential paths would defeat the point.
package githubapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Secret keys a GitHub App credential is read from. Deliberately not `token`:
// a Secret carrying these is not interchangeable with one carrying a PAT, and
// a key that silently meant something else would be worse than a missing one.
const (
	KeyAppID          = "appID"
	KeyClientID       = "clientID"
	KeyInstallationID = "installationID"
	KeyPrivateKey     = "privateKey"
)

// jwtLifetime is how long the signing JWT is valid for.
//
// GitHub refuses anything more than ten minutes into the future. Five leaves
// room for the request itself without sitting near a limit whose breach is
// reported as a generic 401.
const jwtLifetime = 5 * time.Minute

// clockSkew is how far into the past `iat` is set.
//
// GitHub's own guidance: sixty seconds, to allow for drift between our clock
// and theirs. Without it a machine running slightly fast issues a JWT dated in
// GitHub's future, which is refused.
const clockSkew = 60 * time.Second

// refreshBefore is how long before expiry a cached token is replaced.
//
// Installation tokens last an hour. Renewing with a minute to go would hand a
// caller a token that expires mid-push; five is enough for any single step and
// still uses most of the hour.
const refreshBefore = 5 * time.Minute

// Credentials are what a GitHub App needs to mint installation tokens.
type Credentials struct {
	// Issuer is the App's client ID, or its numeric app ID. GitHub recommends
	// the client ID and accepts both.
	Issuer string
	// InstallationID identifies which installation to mint a token for. An App
	// installed on two organisations has two, and they are not interchangeable.
	InstallationID string
	// PrivateKey is the App's PEM-encoded RSA key.
	PrivateKey []byte
	// BaseURL is the API root, for GitHub Enterprise Server. Empty is
	// api.github.com.
	BaseURL string
}

// FromSecret reads credentials from a Secret's data.
func FromSecret(data map[string][]byte, baseURL string) (Credentials, error) {
	issuer := strings.TrimSpace(string(data[KeyClientID]))
	if issuer == "" {
		issuer = strings.TrimSpace(string(data[KeyAppID]))
	}
	c := Credentials{
		Issuer:         issuer,
		InstallationID: strings.TrimSpace(string(data[KeyInstallationID])),
		PrivateKey:     data[KeyPrivateKey],
		BaseURL:        baseURL,
	}
	switch {
	case c.Issuer == "":
		return Credentials{}, fmt.Errorf(
			"a GitHub App Secret needs %s or %s", KeyClientID, KeyAppID)
	case c.InstallationID == "":
		return Credentials{}, fmt.Errorf(
			"a GitHub App Secret needs %s — an App installed on two organisations has "+
				"two, and they are not interchangeable", KeyInstallationID)
	case len(c.PrivateKey) == 0:
		return Credentials{}, fmt.Errorf("a GitHub App Secret needs %s", KeyPrivateKey)
	}
	return c, nil
}

// HasAppKeys reports whether a Secret looks like a GitHub App credential
// rather than a static token.
func HasAppKeys(data map[string][]byte) bool {
	return len(data[KeyPrivateKey]) > 0
}

// Source hands out a currently-valid installation token, minting and caching
// as needed.
//
// Safe for concurrent use: several steps in one Passage can ask at once, and
// each mint is an API call worth not making twice.
type Source struct {
	creds  Credentials
	client *http.Client
	// now is injectable so expiry can be tested without waiting an hour.
	now func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
}

// New builds a Source. It does not contact GitHub; the first Token call does.
func New(creds Credentials, client *http.Client) (*Source, error) {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if _, err := parseKey(creds.PrivateKey); err != nil {
		// Checked here rather than on first use: an unusable key is a
		// configuration error, and finding it when a promotion is already
		// running turns it into a failed crossing.
		return nil, err
	}
	return &Source{creds: creds, client: client, now: time.Now}, nil
}

// Token returns a valid installation token, minting one if the cached token is
// missing or close to expiry.
func (s *Source) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.token != "" && s.now().Add(refreshBefore).Before(s.expires) {
		return s.token, nil
	}

	assertion, err := s.jwt()
	if err != nil {
		return "", err
	}
	token, expires, err := s.exchange(ctx, assertion)
	if err != nil {
		return "", err
	}
	s.token, s.expires = token, expires
	return token, nil
}

// jwt signs the assertion GitHub exchanges for an installation token.
//
// Hand-rolled rather than pulling in a JWT library, and the asymmetry is the
// reason: this only ever *signs*. The dangerous parts of JWT — algorithm
// confusion, `alg: none`, unverified claims — all live in verification, which
// this never does. Signing is a header, a claim set and an RSA signature over
// their base64url join.
func (s *Source) jwt() (string, error) {
	key, err := parseKey(s.creds.PrivateKey)
	if err != nil {
		return "", err
	}

	now := s.now()
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	claims := map[string]any{
		// Sixty seconds in the past, as GitHub advises: a machine running
		// slightly fast otherwise issues a JWT dated in GitHub's future, which
		// is refused with a message that does not mention clocks.
		"iat": now.Add(-clockSkew).Unix(),
		"exp": now.Add(jwtLifetime).Unix(),
		"iss": s.creds.Issuer,
	}

	signing, err := joinSegments(header, claims)
	if err != nil {
		return "", err
	}
	digest := sha256Sum(signing)
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest)
	if err != nil {
		return "", fmt.Errorf("github app: signing the assertion: %w", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// exchange trades the assertion for an installation token.
func (s *Source) exchange(ctx context.Context, assertion string) (string, time.Time, error) {
	base := strings.TrimSuffix(s.creds.BaseURL, "/")
	if base == "" {
		base = "https://api.github.com"
	}
	url := fmt.Sprintf("%s/app/installations/%s/access_tokens", base, s.creds.InstallationID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("github app: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+assertion)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := s.client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("github app: requesting an installation token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated {
		// The body carries GitHub's own reason — a wrong installation id, a
		// key that does not match the app, a clock too far out — and none of
		// those is guessable from the status alone.
		return "", time.Time{}, fmt.Errorf(
			"github app: installation %s: %s: %s",
			s.creds.InstallationID, resp.Status, strings.TrimSpace(string(body)))
	}

	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", time.Time{}, fmt.Errorf("github app: decoding the token response: %w", err)
	}
	if out.Token == "" {
		return "", time.Time{}, errors.New("github app: the token response carried no token")
	}
	if out.ExpiresAt.IsZero() {
		// Treated as already stale rather than assumed to last an hour: a
		// token cached without a known expiry is one that fails mid-promotion
		// at an unpredictable moment.
		out.ExpiresAt = s.now()
	}
	return out.Token, out.ExpiresAt, nil
}

func parseKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("github app: the private key is not PEM — GitHub issues a " +
			"PKCS#1 .pem file, and pasting its base64 body alone will not parse")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	// GitHub issues PKCS#1, but a key round-tripped through some tools comes
	// back as PKCS#8, and refusing that would be refusing the same key.
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("github app: unusable private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("github app: the private key is %T, and GitHub requires RSA", parsed)
	}
	return key, nil
}

// joinSegments base64url-encodes the header and claims and joins them, which
// is the JWT signing input.
func joinSegments(header map[string]string, claims map[string]any) (string, error) {
	h, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("github app: encoding the JWT header: %w", err)
	}
	c, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("github app: encoding the JWT claims: %w", err)
	}
	// Raw encoding: JWT uses base64url without padding, and `=` in a segment
	// is rejected rather than ignored.
	return base64.RawURLEncoding.EncodeToString(h) + "." +
		base64.RawURLEncoding.EncodeToString(c), nil
}

func sha256Sum(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// SourceFor returns a shared Source for a credential, minting the hourly token
// once however many callers ask.
//
// Package-level because the callers are deliberately independent: a Passage's
// git steps and its provider steps each resolve credentials on their own, and
// a cache per caller would mint a token per step against a rate limit shared
// with everything else the App does.
//
// Keyed by the credential's content rather than by Secret name: rotating the
// key must produce a new Source, and two namespaces with the same Secret name
// must not share one.
func SourceFor(creds Credentials) (*Source, error) {
	sum := sha256.Sum256([]byte(creds.Issuer + "\x00" + creds.InstallationID +
		"\x00" + creds.BaseURL + "\x00" + string(creds.PrivateKey)))
	key := hex.EncodeToString(sum[:])

	sources.mu.Lock()
	defer sources.mu.Unlock()
	if s, ok := sources.byKey[key]; ok {
		return s, nil
	}
	s, err := New(creds, nil)
	if err != nil {
		return nil, err
	}
	sources.byKey[key] = s
	return s, nil
}

var sources = struct {
	mu    sync.Mutex
	byKey map[string]*Source
}{byKey: map[string]*Source{}}
