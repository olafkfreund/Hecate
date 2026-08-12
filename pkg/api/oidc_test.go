package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// fakeProvider is a real OIDC provider: real discovery, real JWKS, real signed
// ID tokens. A stub returning a fixed string would exercise none of the
// verification this flow depends on.
type fakeProvider struct {
	srv    *httptest.Server
	key    *rsa.PrivateKey
	issuer string
	// lastAuth records what the browser was redirected with, so the PKCE and
	// state parameters can be asserted.
	lastAuth url.Values
	// noIDToken makes the token endpoint answer without one, which is what a
	// provider does when the openid scope was not granted.
	noIDToken bool
}

func newProvider(t *testing.T) *fakeProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeProvider{key: key}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                p.issuer,
			"authorization_endpoint":                p.issuer + "/authorize",
			"token_endpoint":                        p.issuer + "/token",
			"jwks_uri":                              p.issuer + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: key.Public(), Use: "sig", Algorithm: "RS256", KeyID: "k1"},
		}})
	})
	mux.HandleFunc("/authorize", func(_ http.ResponseWriter, r *http.Request) {
		p.lastAuth = r.URL.Query()
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		body := map[string]any{"access_token": "at", "token_type": "Bearer"}
		if !p.noIDToken {
			body["id_token"] = p.idToken(t, time.Now().Add(time.Hour))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})

	p.srv = httptest.NewServer(mux)
	p.issuer = p.srv.URL
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakeProvider) idToken(t *testing.T, expiry time.Time) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: p.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.Signed(signer).Claims(jwt.Claims{
		Issuer:   p.issuer,
		Subject:  "olaf@example.com",
		Audience: jwt.Audience{"hecate"},
		Expiry:   jwt.NewNumericDate(expiry),
		IssuedAt: jwt.NewNumericDate(time.Now()),
	}).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func newLogin(t *testing.T, p *fakeProvider) *Login {
	t.Helper()
	l, err := NewLogin(context.Background(), LoginConfig{
		Issuer: p.issuer, ClientID: "hecate", ClientSecret: "s",
		RedirectURL: "https://hecate.example/auth/callback", Insecure: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// Discovery at startup, so a misspelt issuer fails the rollout rather than the
// first person to try logging in.
func TestNewLoginFailsOnAnUnreachableIssuer(t *testing.T) {
	_, err := NewLogin(context.Background(), LoginConfig{
		Issuer: "https://nowhere.invalid", ClientID: "hecate",
		RedirectURL: "https://hecate.example/auth/callback",
	})
	if err == nil {
		t.Fatal("want an error for an issuer that does not resolve")
	}
	if !strings.Contains(err.Error(), "discovering") {
		t.Errorf("err = %v, want it to say discovery failed", err)
	}
}

func TestNewLoginRequiresItsConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		cfg        LoginConfig
	}{
		{"no issuer", "issuer", LoginConfig{ClientID: "x", RedirectURL: "y"}},
		{"no client id", "client ID", LoginConfig{Issuer: "x", RedirectURL: "y"}},
		{"no redirect", "redirect URL", LoginConfig{Issuer: "x", ClientID: "y"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewLogin(context.Background(), tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

// PKCE is not optional here: without it, an intercepted authorization code can
// be exchanged by whoever holds it.
func TestLoginStartsWithPKCEAndState(t *testing.T) {
	p := newProvider(t)
	l := newLogin(t, p)

	rec := httptest.NewRecorder()
	l.start(rec, httptest.NewRequest("GET", "/auth/login", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want a redirect", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := loc.Query()
	if q.Get("code_challenge_method") != "S256" {
		t.Errorf("code_challenge_method = %q, want S256", q.Get("code_challenge_method"))
	}
	if q.Get("code_challenge") == "" {
		t.Error("no code_challenge — an intercepted code could be exchanged by anyone")
	}
	if q.Get("state") == "" {
		t.Error("no state — the callback could be forged")
	}

	// The challenge must actually be the hash of the verifier we stored, not
	// merely present.
	var verifier, state string
	for _, c := range rec.Result().Cookies() {
		switch c.Name {
		case verifierCookie:
			verifier = c.Value
		case stateCookie:
			state = c.Value
		}
	}
	if verifier == "" || state == "" {
		t.Fatal("the verifier and state must be carried across the redirect")
	}
	sum := sha256.Sum256([]byte(verifier))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); q.Get("code_challenge") != want {
		t.Error("code_challenge is not the S256 hash of the verifier")
	}
	if q.Get("state") != state {
		t.Error("the state in the URL is not the one in the cookie")
	}
}

// A callback whose state does not match the cookie is how an attacker makes a
// victim's browser finish the attacker's login and end up as them.
func TestCallbackRefusesAMismatchedState(t *testing.T) {
	p := newProvider(t)
	l := newLogin(t, p)

	for _, tc := range []struct {
		name   string
		cookie string
		query  string
	}{
		{"no cookie at all", "", "abc"},
		{"a different state", "abc", "xyz"},
		{"an empty state", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/auth/callback?code=c&state="+tc.query, nil)
			if tc.cookie != "" {
				r.AddCookie(&http.Cookie{Name: stateCookie, Value: tc.cookie})
				r.AddCookie(&http.Cookie{Name: verifierCookie, Value: "v"})
			}
			rec := httptest.NewRecorder()
			l.callback(rec, r)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
			if sessionFrom(rec) != "" {
				t.Error("a session was issued for a login that did not start here")
			}
		})
	}
}

// The happy path: the session cookie ends up holding the ID token, which is the
// credential Kubernetes will be asked about.
func TestCallbackIssuesASessionHoldingTheIDToken(t *testing.T) {
	p := newProvider(t)
	l := newLogin(t, p)

	rec := httptest.NewRecorder()
	l.callback(rec, callbackRequest("s"))

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d (%s), want a redirect", rec.Code, rec.Body)
	}
	session := sessionFrom(rec)
	if session == "" {
		t.Fatal("no session cookie")
	}
	if _, err := l.verifier.Verify(context.Background(), session); err != nil {
		t.Errorf("the session does not hold a valid ID token: %v", err)
	}

	// The cookie is a bearer token: script that can read it can act as the user
	// against the cluster, which is a great deal more than this UI.
	c := cookieFrom(rec, SessionCookie)
	if !c.HttpOnly {
		t.Error("the session cookie is readable by script")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax so the callback navigation keeps it", c.SameSite)
	}
}

// An access token is not what kube-apiserver's OIDC authenticator reads, so a
// provider that returns none has to be reported rather than half-accepted.
func TestCallbackRefusesWhenThereIsNoIDToken(t *testing.T) {
	p := newProvider(t)
	p.noIDToken = true
	l := newLogin(t, p)

	rec := httptest.NewRecorder()
	l.callback(rec, callbackRequest("s"))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "openid") {
		t.Errorf("body = %q, want it to point at the missing scope", rec.Body)
	}
	if sessionFrom(rec) != "" {
		t.Error("a session was issued without an ID token")
	}
}

// The whole point of the design: a browser session and a kubectl-style token
// are the same credential arriving differently, so Authenticate cannot tell
// them apart.
func TestTheSessionCookieIsReadAsABearerToken(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/v1alpha1/namespaces/acme/gates", nil)
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: "an-id-token"})
	if got := bearerToken(r); got != "an-id-token" {
		t.Errorf("bearerToken = %q, want the cookie's value", got)
	}
}

// An explicit header must never be overridden by a cookie the browser
// happened to attach.
func TestTheHeaderWinsOverTheCookie(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/v1alpha1/namespaces/acme/gates", nil)
	r.Header.Set("Authorization", "Bearer explicit")
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: "cookie"})
	if got := bearerToken(r); got != "explicit" {
		t.Errorf("bearerToken = %q, want the header", got)
	}
}

func TestLogoutDropsTheSession(t *testing.T) {
	l := newLogin(t, newProvider(t))
	rec := httptest.NewRecorder()
	l.logout(rec, httptest.NewRequest("POST", "/auth/logout", nil))

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d", rec.Code)
	}
	if c := cookieFrom(rec, SessionCookie); c == nil || c.MaxAge >= 0 {
		t.Error("logout must expire the session cookie")
	}
}

func callbackRequest(state string) *http.Request {
	r := httptest.NewRequest("GET", fmt.Sprintf("/auth/callback?code=c&state=%s", state), nil)
	r.AddCookie(&http.Cookie{Name: stateCookie, Value: state})
	r.AddCookie(&http.Cookie{Name: verifierCookie, Value: "a-verifier"})
	return r
}

func cookieFrom(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func sessionFrom(rec *httptest.ResponseRecorder) string {
	if c := cookieFrom(rec, SessionCookie); c != nil {
		return c.Value
	}
	return ""
}
