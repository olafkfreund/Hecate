package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// SessionCookie holds the caller's ID token between requests.
const SessionCookie = "hecate_session"

// stateCookie and verifierCookie carry the CSRF state and the PKCE verifier
// across the redirect to the provider. Short-lived and deleted on callback.
const (
	stateCookie    = "hecate_oidc_state"
	verifierCookie = "hecate_oidc_verifier"
)

// LoginConfig is what an OIDC login needs.
type LoginConfig struct {
	// Issuer is the OIDC provider, e.g. https://login.example.com. Discovery
	// happens at startup, so a wrong one fails the rollout rather than the
	// first login.
	Issuer string
	// ClientID and ClientSecret identify Hecate to the provider.
	ClientID     string
	ClientSecret string
	// RedirectURL is where the provider sends the browser back. It must be the
	// public URL of this server plus /auth/callback.
	RedirectURL string
	// Scopes beyond openid. `profile` and `email` are usual; `groups` is often
	// what carries the claim Kubernetes maps to groups.
	Scopes []string
	// Insecure allows the session cookie over plain HTTP. For local
	// development only — it is the difference between a token the network can
	// read and one it cannot.
	Insecure bool
}

// Login runs the OIDC authorization-code flow and puts the resulting ID token
// in a session cookie.
//
// **It adds no identity model, and that is the entire design.** Kubernetes can
// itself be configured to trust an OIDC issuer, and when it is, an ID token
// from that issuer *is* a valid Kubernetes bearer token. So Hecate does not
// verify who you are and then decide what you may do — it obtains a credential
// the cluster already understands and keeps asking the API server exactly the
// questions it asked before (see the package comment, and D32).
//
// **The consequence, which has to be said plainly: the cluster must trust the
// same issuer.** If `kube-apiserver` is not configured with this issuer, every
// login will succeed and every request afterwards will be rejected — the token
// is real, and the cluster has no reason to believe it. The startup check below
// cannot detect that, because whether the API server trusts an issuer is not
// something it exposes. Hecate says so at startup rather than letting it be
// discovered one confused user at a time.
type Login struct {
	cfg      LoginConfig
	provider *oidc.Provider
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
}

// NewLogin performs OIDC discovery and returns a configured login flow.
//
// Discovery at startup rather than on first use: an unreachable or misspelt
// issuer should fail the rollout, where it is obvious, and not the first login,
// where it looks like the user's fault.
func NewLogin(ctx context.Context, cfg LoginConfig) (*Login, error) {
	switch {
	case cfg.Issuer == "":
		return nil, errors.New("no OIDC issuer")
	case cfg.ClientID == "":
		return nil, errors.New("no OIDC client ID")
	case cfg.RedirectURL == "":
		return nil, errors.New("no OIDC redirect URL: it must be this server's public URL plus /auth/callback")
	}

	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("discovering the OIDC provider at %s: %w", cfg.Issuer, err)
	}

	scopes := append([]string{oidc.ScopeOpenID}, cfg.Scopes...)
	return &Login{
		cfg:      cfg,
		provider: provider,
		oauth: &oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  cfg.RedirectURL,
			Scopes:       scopes,
		},
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
	}, nil
}

// Routes registers the login endpoints on a mux.
func (l *Login) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/login", l.start)
	mux.HandleFunc("GET /auth/callback", l.callback)
	mux.HandleFunc("POST /auth/logout", l.logout)
}

// start sends the browser to the provider.
func (l *Login) start(w http.ResponseWriter, r *http.Request) {
	state, err := randomString()
	if err != nil {
		http.Error(w, "could not start a login", http.StatusInternalServerError)
		return
	}
	verifier, err := randomString()
	if err != nil {
		http.Error(w, "could not start a login", http.StatusInternalServerError)
		return
	}

	// Both are cookies rather than server-side state, so any replica can
	// complete a login that another one started. Neither is a credential: the
	// state is a nonce, and the verifier is useless without the code that only
	// the provider will send to the registered redirect URL.
	l.setCookie(w, stateCookie, state, 10*time.Minute)
	l.setCookie(w, verifierCookie, verifier, 10*time.Minute)

	challenge := sha256.Sum256([]byte(verifier))
	http.Redirect(w, r, l.oauth.AuthCodeURL(state,
		oauth2.SetAuthURLParam("code_challenge", base64.RawURLEncoding.EncodeToString(challenge[:])),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	), http.StatusFound)
}

// callback completes the flow and sets the session cookie.
func (l *Login) callback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Checked before anything else is read: without it, an attacker can make a
	// victim's browser complete *their* login and end up authenticated as them.
	state, err := r.Cookie(stateCookie)
	if err != nil || state.Value == "" || state.Value != r.URL.Query().Get("state") {
		http.Error(w, "this login did not start here", http.StatusBadRequest)
		return
	}
	verifier, err := r.Cookie(verifierCookie)
	if err != nil || verifier.Value == "" {
		http.Error(w, "this login did not start here", http.StatusBadRequest)
		return
	}
	l.clearCookie(w, stateCookie)
	l.clearCookie(w, verifierCookie)

	if e := r.URL.Query().Get("error"); e != "" {
		// The provider's own refusal — consent declined, account disabled.
		// Reported as-is because it is the provider's message to give.
		http.Error(w, "the identity provider refused this login: "+e, http.StatusForbidden)
		return
	}

	token, err := l.oauth.Exchange(ctx, r.URL.Query().Get("code"),
		oauth2.SetAuthURLParam("code_verifier", verifier.Value))
	if err != nil {
		http.Error(w, "could not complete the login", http.StatusBadGateway)
		return
	}

	raw, ok := token.Extra("id_token").(string)
	if !ok || raw == "" {
		// Without an ID token there is nothing Kubernetes can consume: an
		// access token is not what kube-apiserver's OIDC authenticator reads.
		http.Error(w, "the provider returned no ID token — check that the openid scope is granted",
			http.StatusBadGateway)
		return
	}

	// Verified here as well as by the cluster later. Not redundant: it fails a
	// bad token now, with a message, rather than as an opaque 401 on the user's
	// next request.
	verified, err := l.verifier.Verify(ctx, raw)
	if err != nil {
		http.Error(w, "the ID token did not verify", http.StatusForbidden)
		return
	}

	// The cookie outlives the token by nothing: when the ID token expires the
	// session is over, because the cookie *is* the credential and a stale one
	// would only produce 401s that look like a bug.
	l.setCookie(w, SessionCookie, raw, time.Until(verified.Expiry))
	http.Redirect(w, r, "/", http.StatusFound)
}

// logout drops the session cookie.
//
// POST, not GET: a GET logout can be triggered by any page that can make the
// browser fetch a URL.
func (l *Login) logout(w http.ResponseWriter, _ *http.Request) {
	l.clearCookie(w, SessionCookie)
	w.WriteHeader(http.StatusNoContent)
}

func (l *Login) setCookie(w http.ResponseWriter, name, value string, ttl time.Duration) {
	if ttl < 0 {
		ttl = 0
	}
	http.SetCookie(w, &http.Cookie{
		Name:  name,
		Value: value,
		Path:  "/",
		// HttpOnly because the cookie holds a bearer token: script that can
		// read it can act as the user anywhere the cluster accepts it, which
		// is a great deal more than this UI.
		HttpOnly: true,
		Secure:   !l.cfg.Insecure,
		// Lax rather than Strict: the callback arrives as a top-level
		// navigation from the provider, and Strict would drop the cookie
		// exactly then.
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(ttl.Seconds()),
	})
}

func (l *Login) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: "/",
		HttpOnly: true, Secure: !l.cfg.Insecure,
		SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

func randomString() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
