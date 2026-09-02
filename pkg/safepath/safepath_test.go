package safepath

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// escapeTarget is a symlink target relative enough to reach outside no matter
// how deeply nested dir happens to be — more ".." components than any real
// checkout is nested. Not an absolute target: some git checkout code rebases
// an absolute symlink target under the checkout root, which would make a
// fixture using one pass without ever exercising this guard.
func escapeTarget(outside string) string {
	return strings.Repeat("../", 12) + strings.TrimPrefix(filepath.ToSlash(outside), "/")
}

// TestJoinRefusesTraversal is the lexical case filepath.Rel already caught.
func TestJoinRefusesTraversal(t *testing.T) {
	dir := t.TempDir()
	if _, err := Join(dir, "../../etc/passwd"); err == nil {
		t.Fatal("expected a refusal — ../../etc/passwd escapes dir")
	}
}

// TestJoinRefusesAnAbsolutePath is the other lexical case.
func TestJoinRefusesAnAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	if _, err := Join(dir, "/etc/passwd"); err == nil {
		t.Fatal("expected a refusal — an absolute path is never relative to dir")
	}
}

// TestJoinRefusesASymlinkEscape is the case filepath.Rel cannot catch: a
// symlink already on disk inside dir, with a relative target, pointing
// outside dir. "apps/foo" contains no ".." and clears the lexical check —
// only resolving the filesystem reveals the escape.
func TestJoinRefusesASymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(escapeTarget(outside), filepath.Join(dir, "apps")); err != nil {
		t.Fatal(err)
	}

	_, err := Join(dir, "apps/foo")
	if err == nil {
		t.Fatal("expected a refusal — apps is a symlink out of dir")
	}
	if _, statErr := os.Stat(filepath.Join(outside, "foo")); !os.IsNotExist(statErr) {
		t.Fatalf("the resolved path escaped dir: %s exists", filepath.Join(outside, "foo"))
	}
}

// TestJoinKeepsANestedPathWhenNoIntermediateDirExists is the trap: realAncestor
// resolves the deepest *existing* ancestor, so dropping the remainder it
// walked past would silently land the result at dir's root instead of the
// requested nested path.
func TestJoinKeepsANestedPathWhenNoIntermediateDirExists(t *testing.T) {
	dir := t.TempDir()

	got, err := Join(dir, "apps/dev/passage.yaml")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	want = filepath.Join(want, "apps", "dev", "passage.yaml")
	if got != want {
		t.Fatalf("Join(%q, %q) = %q, want %q — a nested nonexistent path must not collapse to dir's root",
			dir, "apps/dev/passage.yaml", got, want)
	}
}
