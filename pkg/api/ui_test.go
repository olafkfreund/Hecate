package api

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The API routes must keep winning over the catch-all that serves the UI.
// Getting this wrong would answer /healthz with an HTML page, and a probe would
// call that healthy.
// mustSub is the embedded UI as a filesystem, so a test can ask whether a real
// export is present rather than assuming either way.
func mustSub(t *testing.T) fs.FS {
	t.Helper()
	sub, err := fs.Sub(assets, "ui")
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

func TestAPIRoutesWinOverTheUI(t *testing.T) {
	s := &Server{Version: "test"}
	h := s.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Errorf("/healthz answered %q — the UI catch-all swallowed it", ct)
	}
	if !strings.Contains(rec.Body.String(), `"version"`) {
		t.Errorf("body = %q, want the health JSON", rec.Body)
	}
}

// An unauthenticated API route must stay unauthenticated-and-refused, not fall
// through to the UI. Serving HTML for a data request turns a 401 the client
// knows how to handle into a parse error.
func TestAnAPIPathIsNeverAnsweredWithHTML(t *testing.T) {
	s := &Server{Version: "test", Auth: &Authenticator{}}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET",
		"/api/v1alpha1/namespaces/acme/gates", nil))

	if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "html") {
		t.Errorf("an API path was answered with HTML (%q, status %d)", ct, rec.Code)
	}
}

// Without a built UI the server still runs and says so. It must not be an error
// status: the API is working, and a 500 would make a proxy or a probe treat the
// whole server as unhealthy.
func TestThePlaceholderIsServedWhenTheUIIsNotBuilt(t *testing.T) {
	if built(mustSub(t)) {
		t.Skip("a real UI is embedded in this build; the placeholder path cannot be exercised")
	}

	s := &Server{Version: "test"}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — the API is fine, only the UI is absent", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "make ui") {
		t.Errorf("the placeholder should say how to build it:\n%s", rec.Body)
	}
}

// Hashed bundles are immutable and cached for a year; index.html must not be,
// or a browser runs last week's app against this week's API.
func TestCachingIsAppliedOnlyToFingerprintedAssets(t *testing.T) {
	s := &Server{Version: "test"}
	h := s.Handler()

	// A real asset out of the embedded export, not an invented path: a 404
	// would pass this test for the wrong reason.
	asset := ""
	if built(mustSub(t)) {
		_ = fs.WalkDir(mustSub(t), "_next/static", func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && asset == "" && strings.HasSuffix(p, ".js") {
				asset = "/_next/static/" + strings.TrimPrefix(p, "_next/static/")
			}
			return nil
		})
		if asset == "" {
			t.Fatal("the export has no fingerprinted JavaScript; the assumption behind the caching rule is wrong")
		}
	}

	cases := []struct{ path, want string }{{"/", "no-cache"}}
	if asset != "" {
		cases = append(cases, struct{ path, want string }{asset, "immutable"})
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", tc.path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s status = %d", tc.path, rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, tc.want) {
			t.Errorf("%s Cache-Control = %q, want %q", tc.path, got, tc.want)
		}
	}
}
