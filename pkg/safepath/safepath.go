// Package safepath answers one question — "does this path stay inside that
// directory, even once symlinks are resolved?" — for everything in Hecate
// that joins an untrusted path onto a git checkout.
//
// It exists because two call sites needed the identical guard for the
// identical reason: the Passage steps' checkoutPath (a `path` comes from a
// Gate's spec, not necessarily authored by whoever runs the controller) and
// the authoring endpoint's gitPublish (`repo` and `path` come from the same
// HTTP caller, so the caller controls what the checkout's symlinks resolve
// to). A `filepath.Rel` check alone is purely lexical: it never touches the
// filesystem, so a symlink committed into the checkout — `apps -> ../../etc`
// — clears it while the OS resolves the result somewhere else entirely. One
// implementation, because a second one would eventually disagree with the
// first about what "escapes" means — see D32 and D61.
package safepath

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Join joins path onto dir and refuses anything that would land outside dir
// — including via a symlink already present on disk.
//
// The deepest existing ancestor of the parent directory is resolved with
// EvalSymlinks and re-checked for containment; what the walk stepped past to
// get there is carried back down and joined onto the resolved ancestor, since
// most of a freshly checked-out path's parent directories do not exist yet —
// dropping that remainder would silently relocate a nested path
// (`apps/dev/passage.yaml`) up to the ancestor alone (`passage.yaml` at the
// checkout root). The returned path is built from that resolved ancestor, not
// from the original join again, so callers use the value actually proven
// contained rather than a second, unchecked copy of the same tainted string.
//
// dir itself must already exist; path need not.
func Join(dir, path string) (string, error) {
	if filepath.IsAbs(path) {
		return "", fmt.Errorf("path %q must be relative", path)
	}
	full := filepath.Join(dir, filepath.FromSlash(path))
	// A `path` of "../../etc" would otherwise write outside dir.
	if rel, err := filepath.Rel(dir, full); err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("path %q escapes the work directory", path)
	}

	realParent, remainder, err := realAncestor(filepath.Dir(full))
	if err != nil {
		return "", fmt.Errorf("path %q: %w", path, err)
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", dir, err)
	}
	// A separator-boundary compare, not a bare HasPrefix: realParent
	// "/foo" must not match a realDir of "/foobar".
	if realParent != realDir && !strings.HasPrefix(realParent, realDir+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the work directory", path)
	}
	return filepath.Join(realParent, remainder, filepath.Base(full)), nil
}

// realAncestor resolves the deepest existing ancestor of dir to its
// symlink-free path, plus the not-yet-created remainder between that
// ancestor and dir. filepath.EvalSymlinks fails on a path that does not exist
// yet — true of most of a freshly checked-out path's parent directories — so
// this walks up until it finds one that does, the same way a shell resolving
// a not-yet-created path would, and carries what it walked past back down.
func realAncestor(dir string) (resolved, remainder string, err error) {
	var walked []string
	for {
		r, err := filepath.EvalSymlinks(dir)
		if err == nil {
			return r, filepath.Join(walked...), nil
		}
		if !os.IsNotExist(err) {
			return "", "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", fmt.Errorf("no existing ancestor of %s", dir)
		}
		// Prepended, not appended: walked accumulates outside-in as the walk
		// climbs, so it has to end up in the order the path actually reads.
		walked = append([]string{filepath.Base(dir)}, walked...)
		dir = parent
	}
}
