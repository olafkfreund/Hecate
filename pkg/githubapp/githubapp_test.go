package githubapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

// fakeGitHub answers the token exchange the way the API does: 201, a token and
// an expires_at. Shape taken from GitHub's REST documentation rather than
// invented.
type fakeGitHub struct {
	mu         sync.Mutex
	calls      int
	assertions []string
	status     int
	expires    time.Time
	// omitExpiry drops expires_at entirely, which zeroing `expires` cannot do
	// — the handler substitutes an hour for a zero value, so the test it was
	// meant to drive passed for the wrong reason.
	omitExpiry bool
}

func (f *fakeGitHub) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls++
		n := f.calls
		f.assertions = append(f.assertions, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		f.mu.Unlock()

		if !strings.HasSuffix(r.URL.Path, "/access_tokens") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if f.status != 0 {
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
			return
		}
		body := map[string]any{"token": fmt.Sprintf("ghs_installation_%d", n)}
		if !f.omitExpiry {
			expires := f.expires
			if expires.IsZero() {
				expires = time.Now().Add(time.Hour)
			}
			body["expires_at"] = expires.UTC().Format(time.RFC3339)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func sourceFor(t *testing.T, f *fakeGitHub) *Source {
	t.Helper()
	s, err := New(Credentials{
		Issuer: "Iv1.abc123", InstallationID: "42",
		PrivateKey: testKey(t), BaseURL: f.start(t),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The crypto is the part most likely to be subtly wrong and least likely to
// fail loudly, so the signature is verified here rather than trusted — with
// the app's own public key, exactly as GitHub does.
func TestTheAssertionIsAValidRS256JWT(t *testing.T) {
	f := &fakeGitHub{}
	s := sourceFor(t, f)
	if _, err := s.Token(context.Background()); err != nil {
		t.Fatal(err)
	}

	assertion := f.assertions[0]
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		t.Fatalf("assertion has %d segments, want 3", len(parts))
	}

	// Padding-free base64url. A padded segment is rejected by GitHub rather
	// than tolerated.
	for i, p := range parts {
		if strings.ContainsAny(p, "=+/") {
			t.Errorf("segment %d is not base64url: %q", i, p)
		}
	}

	var header map[string]string
	if err := json.Unmarshal(decode(t, parts[0]), &header); err != nil {
		t.Fatal(err)
	}
	if header["alg"] != "RS256" {
		t.Errorf("alg = %q, want RS256 — GitHub requires it", header["alg"])
	}

	var claims map[string]any
	if err := json.Unmarshal(decode(t, parts[1]), &claims); err != nil {
		t.Fatal(err)
	}
	if claims["iss"] != "Iv1.abc123" {
		t.Errorf("iss = %v, want the client id", claims["iss"])
	}
	iat, exp := int64(claims["iat"].(float64)), int64(claims["exp"].(float64))
	now := time.Now().Unix()
	// Sixty seconds in the past, per GitHub's own guidance: a machine running
	// slightly fast otherwise issues a JWT dated in GitHub's future.
	if iat > now-30 {
		t.Errorf("iat = %d is not in the past relative to %d — clock drift would refuse this", iat, now)
	}
	// Ten minutes is GitHub's hard limit, and breaching it is reported as a
	// generic 401 that says nothing about time.
	if exp-iat > 600 {
		t.Errorf("the assertion lives %ds, and GitHub refuses more than 600", exp-iat)
	}
	if exp <= now {
		t.Errorf("exp = %d is already past at %d", exp, now)
	}

	// The signature itself, checked with the public half of the key that
	// signed it.
	block, _ := pem.Decode(s.creds.PrivateKey)
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], decode(t, parts[2])); err != nil {
		t.Fatalf("the assertion does not verify against its own key: %v", err)
	}
}

func decode(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("segment is not base64url: %v", err)
	}
	return b
}

// A token minted per call would turn every step into an API request and hit
// GitHub's rate limit on a busy Passage.
func TestATokenIsCachedUntilItNearsExpiry(t *testing.T) {
	f := &fakeGitHub{}
	s := sourceFor(t, f)
	ctx := context.Background()

	first, err := s.Token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		again, err := s.Token(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if again != first {
			t.Fatalf("token changed while still valid: %q then %q", first, again)
		}
	}
	if f.calls != 1 {
		t.Errorf("minted %d tokens for six requests", f.calls)
	}
}

// Renewing with a minute to go hands a caller a token that expires mid-push.
func TestATokenIsRenewedBeforeItExpires(t *testing.T) {
	f := &fakeGitHub{}
	s := sourceFor(t, f)
	ctx := context.Background()

	if _, err := s.Token(ctx); err != nil {
		t.Fatal(err)
	}
	// Fifty-seven minutes later: still valid, but inside the refresh window.
	s.now = func() time.Time { return time.Now().Add(57 * time.Minute) }
	second, err := s.Token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if f.calls != 2 {
		t.Fatalf("minted %d tokens, want a refresh before expiry", f.calls)
	}
	if second == "ghs_installation_1" {
		t.Error("the stale token was handed out again")
	}
}

// A token cached without a known expiry is one that fails mid-promotion at an
// unpredictable moment.
func TestATokenWithNoExpiryIsNotCached(t *testing.T) {
	f := &fakeGitHub{}
	s := sourceFor(t, f)
	f.omitExpiry = true
	ctx := context.Background()

	if _, err := s.Token(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Token(ctx); err != nil {
		t.Fatal(err)
	}
	if f.calls != 2 {
		t.Errorf("calls = %d — a token with no expiry was cached", f.calls)
	}
}

// GitHub's body says which of several indistinguishable things went wrong: a
// wrong installation id, a key that does not match the app, a clock too far
// out. The status alone says none of them.
func TestARefusedExchangeCarriesGitHubsReason(t *testing.T) {
	f := &fakeGitHub{status: http.StatusUnauthorized}
	s := sourceFor(t, f)

	_, err := s.Token(context.Background())
	if err == nil {
		t.Fatal("a refused exchange produced a token")
	}
	for _, want := range []string{"Bad credentials", "401"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not carry %q", err, want)
		}
	}
}

// An unusable key found when a promotion is already running turns a
// configuration error into a failed crossing.
func TestAnUnusableKeyIsRefusedAtConstruction(t *testing.T) {
	for name, key := range map[string][]byte{
		"empty":     nil,
		"not PEM":   []byte("MIIEpAIBAAKCAQEA...."),
		"truncated": []byte("-----BEGIN RSA PRIVATE KEY-----\nnope\n-----END RSA PRIVATE KEY-----\n"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(Credentials{Issuer: "x", InstallationID: "1", PrivateKey: key}, nil); err == nil {
				t.Error("an unusable key was accepted")
			}
		})
	}
}

func TestFromSecretNamesWhatIsMissing(t *testing.T) {
	key := testKey(t)
	for name, tc := range map[string]struct {
		data map[string][]byte
		want string
	}{
		"no issuer":       {map[string][]byte{KeyInstallationID: []byte("1"), KeyPrivateKey: key}, "clientID"},
		"no installation": {map[string][]byte{KeyClientID: []byte("Iv1"), KeyPrivateKey: key}, "installationID"},
		"no key":          {map[string][]byte{KeyClientID: []byte("Iv1"), KeyInstallationID: []byte("1")}, "privateKey"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := FromSecret(tc.data, "")
			if err == nil {
				t.Fatal("an incomplete Secret was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
		})
	}

	// appID is accepted where clientID is absent: GitHub recommends the client
	// id and accepts both, and refusing an app id would refuse a valid setup.
	got, err := FromSecret(map[string][]byte{
		KeyAppID: []byte("12345"), KeyInstallationID: []byte("1"), KeyPrivateKey: key,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Issuer != "12345" {
		t.Errorf("issuer = %q, want the app id as a fallback", got.Issuer)
	}
}

// The segments must never be padded, whatever their length.
//
// Asserting this on a real assertion is not enough and passed a mutant: this
// header and claim set happen to encode to multiples of three bytes, so padded
// and unpadded output are byte-identical and swapping the encoder changes
// nothing. A payload whose length is deliberately not a multiple of three is
// what makes the difference visible — and GitHub rejects a padded segment
// rather than tolerating it.
func TestSegmentsAreNeverPadded(t *testing.T) {
	for i := range 6 {
		claims := map[string]any{"iss": strings.Repeat("x", i), "iat": 1, "exp": 2}
		joined, err := joinSegments(map[string]string{"alg": "RS256", "typ": "JWT"}, claims)
		if err != nil {
			t.Fatal(err)
		}
		for n, seg := range strings.Split(joined, ".") {
			if strings.Contains(seg, "=") {
				t.Errorf("issuer of length %d padded segment %d: %q", i, n, seg)
			}
			if _, err := base64.RawURLEncoding.DecodeString(seg); err != nil {
				t.Errorf("issuer of length %d: segment %d is not raw base64url: %v", i, n, err)
			}
		}
	}
}
