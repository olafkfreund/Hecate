package steps

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"

	"github.com/olafkfreund/hecate/pkg/passage"
)

// localRegistry runs a real registry in the test process. Plain HTTP, which is
// why every OCI test here sets insecure — and is the same reason a real
// internal registry needs the option.
func localRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

func seedManifests(t *testing.T, work string) {
	t.Helper()
	writeFile(t, work, "repo/rendered/deployment.yaml",
		"apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: api\n")
	writeFile(t, work, "repo/rendered/nested/configmap.yaml",
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: api-config\n")
}

func TestOCIPushAndPull(t *testing.T) {
	host, work := localRegistry(t), t.TempDir()
	seedManifests(t, work)
	repo := host + "/acme/manifests"

	res := mustRun(t, NewOCIPush(nil), gitCtx(t, work, OCIPushConfig{
		Path: "repo/rendered", Repo: repo, Tag: "1.0.0",
		Source: "https://github.com/acme/fleet", Revision: "abc123", Insecure: true,
	}))

	digest, _ := res.Output["digest"].(string)
	if !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("digest = %q", digest)
	}

	// Pull it back into a different directory and check the tree survived.
	pulled := t.TempDir()
	pull := mustRun(t, NewOCIPull(nil), gitCtx(t, pulled, OCIPullConfig{
		Repo: repo, Tag: "1.0.0", Out: "repo/fetched", Insecure: true,
	}))
	if pull.Output["files"] != 2 {
		t.Errorf("pulled %v files, want 2", pull.Output["files"])
	}
	for _, path := range []string{"repo/fetched/deployment.yaml", "repo/fetched/nested/configmap.yaml"} {
		if got := read(t, filepath.Join(pulled, path)); !strings.Contains(got, "apiVersion") {
			t.Errorf("%s did not come back: %q", path, got)
		}
	}
}

// The artifact has to be one Flux will actually read. These media types are
// taken from an artifact `flux push artifact` produced, not from documentation.
func TestOCIPushProducesAFluxArtifact(t *testing.T) {
	host, work := localRegistry(t), t.TempDir()
	seedManifests(t, work)
	repo := host + "/acme/manifests"

	mustRun(t, NewOCIPush(nil), gitCtx(t, work, OCIPushConfig{
		Path: "repo/rendered", Repo: repo, Tag: "1.0.0",
		Source: "fleet", Revision: "abc123", Insecure: true,
	}))

	ref, err := name.NewTag(repo+":1.0.0", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	image, err := remote.Image(ref)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := image.Manifest()
	if err != nil {
		t.Fatal(err)
	}

	if manifest.Config.MediaType != fluxConfigMediaType {
		t.Errorf("config media type = %s, want %s", manifest.Config.MediaType, fluxConfigMediaType)
	}
	if len(manifest.Layers) != 1 || manifest.Layers[0].MediaType != fluxContentMediaType {
		t.Errorf("layers = %+v", manifest.Layers)
	}
	// The annotations are what an OCIRepository reports as the revision it
	// applied, so they are how a deployment traces back to what produced it.
	if manifest.Annotations["org.opencontainers.image.revision"] != "abc123" {
		t.Errorf("annotations = %v", manifest.Annotations)
	}
	if manifest.Annotations["org.opencontainers.image.source"] != "fleet" {
		t.Errorf("annotations = %v", manifest.Annotations)
	}
}

// The digest is a hash of the content. A wall-clock timestamp anywhere in the
// artifact would mint a new one on every attempt, and Flux would treat a
// re-run of the same crossing as a new revision to deploy (D23).
func TestOCIPushIsDeterministic(t *testing.T) {
	host := localRegistry(t)
	var digests []string

	for i := 0; i < 3; i++ {
		work := t.TempDir()
		seedManifests(t, work)
		res := mustRun(t, NewOCIPush(nil), gitCtx(t, work, OCIPushConfig{
			Path: "repo/rendered", Repo: host + "/acme/manifests", Tag: "1.0.0",
			Revision: "abc123", Insecure: true,
		}))
		digests = append(digests, res.Output["digest"].(string))
	}

	for i := 1; i < len(digests); i++ {
		if digests[i] != digests[0] {
			t.Fatalf("the same content pushed to different digests:\n  %s\n  %s", digests[0], digests[i])
		}
	}
}

// Different content must produce a different digest, or the determinism above
// would be a bug rather than a property.
func TestOCIPushFollowsTheContent(t *testing.T) {
	host := localRegistry(t)
	repo := host + "/acme/manifests"
	cfg := OCIPushConfig{Path: "repo/rendered", Repo: repo, Tag: "1.0.0", Insecure: true}

	first := t.TempDir()
	seedManifests(t, first)
	a := mustRun(t, NewOCIPush(nil), gitCtx(t, first, cfg))

	second := t.TempDir()
	seedManifests(t, second)
	writeFile(t, second, "repo/rendered/deployment.yaml",
		"apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: api\nspec:\n  replicas: 3\n")
	b := mustRun(t, NewOCIPush(nil), gitCtx(t, second, cfg))

	if a.Output["digest"] == b.Output["digest"] {
		t.Error("changed manifests produced the same digest")
	}
}

// A pull is re-entrant (D19): a retry after a partial unpack must not leave a
// mixture of two artifacts.
func TestOCIPullReplacesRatherThanMerges(t *testing.T) {
	host, work := localRegistry(t), t.TempDir()
	seedManifests(t, work)
	repo := host + "/acme/manifests"
	mustRun(t, NewOCIPush(nil), gitCtx(t, work, OCIPushConfig{
		Path: "repo/rendered", Repo: repo, Tag: "1.0.0", Insecure: true,
	}))

	pulled := t.TempDir()
	cfg := OCIPullConfig{Repo: repo, Tag: "1.0.0", Out: "repo/fetched", Insecure: true}
	mustRun(t, NewOCIPull(nil), gitCtx(t, pulled, cfg))
	// Left over from an earlier, different artifact.
	writeFile(t, pulled, "repo/fetched/stale.yaml", "kind: Leftover\n")

	mustRun(t, NewOCIPull(nil), gitCtx(t, pulled, cfg))

	if _, err := os.Stat(filepath.Join(pulled, "repo/fetched/stale.yaml")); err == nil {
		t.Error("a stale file survived the pull — the directory was merged, not replaced")
	}
}

// A tar entry naming ../ would write outside the work dir. The archive is
// remote content, so it is not trusted to stay inside.
func TestOCIPullRefusesAnEscapingEntry(t *testing.T) {
	cases := []struct {
		name  string
		entry string
	}{
		{"climbing out of the directory", "../../escaped.yaml"},
		{"an absolute path", "/tmp/escaped.yaml"},
		// The containment check compares against the target directory plus a
		// separator, and this is the entry that proves the separator is load
		// bearing. Joining `../work-evil/x` onto `<base>/work` yields
		// `<base>/work-evil/x`, which is prefixed by `<base>/work` as a plain
		// string while being a wholly different directory. Without the
		// separator it unpacks, and the two cases above still pass — so this
		// is the only thing standing between the check and a sibling-directory
		// write.
		{"a sibling that shares a name prefix", "../work-evil/escaped.yaml"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Plain tar, not gzipped: untar reads the layer's uncompressed
			// form, and a static layer hands back exactly the bytes it was
			// given.
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			body := []byte("owned")
			if err := tw.WriteHeader(&tar.Header{
				Name: tc.entry, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)),
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write(body); err != nil {
				t.Fatal(err)
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}

			// A named subdirectory rather than the temp dir itself, so there is
			// a real sibling for the third case to aim at.
			base := t.TempDir()
			dir := filepath.Join(base, "work")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}

			layer := static.NewLayer(buf.Bytes(), fluxContentMediaType)
			_, err := untar(layer, dir)
			if err == nil {
				t.Fatal("an entry escaping the target directory was unpacked")
			}
			if !strings.Contains(err.Error(), "escapes") {
				t.Errorf("err = %v", err)
			}

			// The error is the contract, but the file is the damage. Assert
			// nothing landed outside, in case a future guard reports the
			// refusal after having already written.
			if entries, err := os.ReadDir(base); err == nil {
				for _, e := range entries {
					if e.Name() != "work" {
						t.Errorf("%q was created outside the target directory", e.Name())
					}
				}
			}
		})
	}
}

// A tar entry over the size limit must be refused outright, not written
// truncated. maxEntrySize is a package var precisely so this test can lower
// it and prove the logic with a small fixture rather than a real 64 MiB one;
// the production default stays 64 << 20 (asserted below).
func TestOCIPullRefusesAnOversizeEntry(t *testing.T) {
	if maxEntrySize != 64<<20 {
		t.Fatalf("maxEntrySize = %d, want the production default of 64 MiB", maxEntrySize)
	}

	orig := maxEntrySize
	maxEntrySize = 8
	t.Cleanup(func() { maxEntrySize = orig })

	body := []byte("way too big for the limit")
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name: "big.yaml", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	layer := static.NewLayer(buf.Bytes(), fluxContentMediaType)
	_, err := untar(layer, dir)
	if err == nil {
		t.Fatal("an entry over the size limit was unpacked")
	}
	if !strings.Contains(err.Error(), "over the") {
		t.Errorf("err = %v", err)
	}

	// The refusal must mean nothing landed, not a truncated file quietly left
	// behind — that quiet leftover is the bug this guards against.
	target := filepath.Join(dir, "big.yaml")
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Errorf("big.yaml was written to disk despite the refusal (stat err = %v)", statErr)
	}
}

// An archive with more entries than the limit must be refused outright,
// counting directories and files alike — a flood of either is the same
// disk-exhaustion shape. maxArchiveEntries is a package var for the same
// reason maxEntrySize is: a small fixture proves the logic without building a
// real 10,000-entry archive.
func TestOCIPullRefusesTooManyEntries(t *testing.T) {
	if maxArchiveEntries != 10000 {
		t.Fatalf("maxArchiveEntries = %d, want the production default of 10000", maxArchiveEntries)
	}

	orig := maxArchiveEntries
	maxArchiveEntries = 3
	t.Cleanup(func() { maxArchiveEntries = orig })

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("file-%d.yaml", i)
		body := []byte("x")
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	layer := static.NewLayer(buf.Bytes(), fluxContentMediaType)
	_, err := untar(layer, dir)
	if err == nil {
		t.Fatal("an archive over the entry limit was unpacked")
	}
	if !strings.Contains(err.Error(), "more than") {
		t.Errorf("err = %v", err)
	}

	// Proof the refusal actually stopped the unpack partway through, rather
	// than counting entries without acting on it: fewer files landed than the
	// archive carried.
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) >= 5 {
		t.Errorf("all 5 entries landed despite the limit of %d", maxArchiveEntries)
	}
}

// An archive whose entries individually pass the per-entry cap but whose sum
// exceeds the archive-wide cap must still be refused — a per-entry limit alone
// bounds nothing about the total. maxArchiveBytes is a package var for the
// same testability reason as the other two limits.
func TestOCIPullRefusesTooManyTotalBytes(t *testing.T) {
	if maxArchiveBytes != 512<<20 {
		t.Fatalf("maxArchiveBytes = %d, want the production default of 512 MiB", maxArchiveBytes)
	}

	origBytes, origEntries := maxArchiveBytes, maxArchiveEntries
	maxArchiveBytes = 10
	maxArchiveEntries = 1000 // isolate the bytes check from the entries check
	t.Cleanup(func() { maxArchiveBytes, maxArchiveEntries = origBytes, origEntries })

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	// Three entries at 4 bytes each: each one is far under maxEntrySize, but
	// the second entry alone would already be within budget (8 <= 10) while
	// the third tips the running total over it (12 > 10).
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("file-%d.yaml", i)
		body := []byte("abcd")
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	layer := static.NewLayer(buf.Bytes(), fluxContentMediaType)
	_, err := untar(layer, dir)
	if err == nil {
		t.Fatal("an archive over the total byte limit was unpacked")
	}
	if !strings.Contains(err.Error(), "total limit") {
		t.Errorf("err = %v", err)
	}

	// The entry that would have tipped the total over the limit must not have
	// landed at all — checked before the write, not after, so nothing beyond
	// the limit ever touches disk.
	var onDisk int64
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		onDisk += fi.Size()
	}
	if onDisk > maxArchiveBytes {
		t.Errorf("%d bytes landed on disk, over the %d byte limit", onDisk, maxArchiveBytes)
	}
	if len(entries) >= 3 {
		t.Errorf("all 3 entries landed despite the byte limit")
	}
}

func TestOCIRefusals(t *testing.T) {
	work := t.TempDir()
	seedManifests(t, work)

	for _, tc := range []struct {
		name   string
		runner passage.Runner
		cfg    any
		says   string
	}{
		{"push with no repo", NewOCIPush(nil),
			OCIPushConfig{Path: "repo/rendered", Tag: "1.0.0"}, "repo is required"},
		{"push with no tag", NewOCIPush(nil),
			OCIPushConfig{Path: "repo/rendered", Repo: "example.test/x"}, "tag is required"},
		{"push of a directory that is not there", NewOCIPush(nil),
			OCIPushConfig{Path: "repo/ghost", Repo: "example.test/x", Tag: "1"}, "no directory"},
		{"push outside the work dir", NewOCIPush(nil),
			OCIPushConfig{Path: "../escape", Repo: "example.test/x", Tag: "1"}, "escapes"},
		{"pull with neither tag nor digest", NewOCIPull(nil),
			OCIPullConfig{Repo: "example.test/x", Out: "repo/o"}, "one of tag or digest"},
		{"pull with both", NewOCIPull(nil),
			OCIPullConfig{Repo: "example.test/x", Out: "repo/o", Tag: "1", Digest: "sha256:abc"}, "not both"},
		{"pull with no out", NewOCIPull(nil),
			OCIPullConfig{Repo: "example.test/x", Tag: "1"}, "out is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.runner.Run(context.Background(), gitCtx(t, work, tc.cfg))
			if !passage.IsTerminal(err) {
				t.Fatalf("err = %v, want a terminal refusal", err)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("error does not mention %q: %v", tc.says, err)
			}
		})
	}
}

// What `insecure` is actually for, recorded because the obvious test cannot
// show it: go-containerregistry already treats a loopback registry as plain
// HTTP, so an httptest server reaches without the option. The option matters for
// a *named* host — an internal registry that terminates TLS elsewhere, or the
// in-cluster one at hecate-registry:5000 — which cannot be stood up in a unit
// test. That it is opt-in is asserted in pkg/registry.
func TestALoopbackRegistryIsAlreadyPlainHTTP(t *testing.T) {
	host, work := localRegistry(t), t.TempDir()
	seedManifests(t, work)

	// No Insecure, and it still works, because the host is loopback.
	mustRun(t, NewOCIPush(nil), gitCtx(t, work, OCIPushConfig{
		Path: "repo/rendered", Repo: host + "/acme/manifests", Tag: "1.0.0",
	}))

	// A named host would need it, and asking for it must be explicit — anything
	// else would send registry credentials in clear.
	if _, err := NewOCIPush(nil).Run(context.Background(), gitCtx(t, work, OCIPushConfig{
		Path: "repo/rendered", Repo: "registry.invalid/acme/manifests", Tag: "1.0.0",
	})); err == nil {
		t.Error("a push to an unreachable named host reported success")
	}
}
