// Package beacon discovers artifacts and emits Bundles.
package beacon

import (
	"fmt"
	"regexp"
	"slices"
	"sort"

	"github.com/Masterminds/semver/v3"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// Selection is the "which tag do we want" half of a watch, shared by
// ImageWatch and TagWatch because both express it identically.
type Selection struct {
	Strategy   v1alpha1.TagSelection
	Constraint string
	Allow      string
	Ignore     []string
}

// ErrNoMatch reports that nothing survived filtering and selection. It is a
// normal outcome, not a failure: a watch with a constraint no published tag
// satisfies is correctly configured and simply has nothing to offer yet.
type ErrNoMatch struct{ Reason string }

func (e *ErrNoMatch) Error() string { return e.Reason }

// Pick returns the tag this Selection resolves to.
func (s Selection) Pick(tags []string) (string, error) {
	candidates, err := s.filter(tags)
	if err != nil {
		return "", err
	}
	if len(candidates) == 0 {
		return "", &ErrNoMatch{Reason: "no tags matched the allow pattern and ignore list"}
	}

	switch s.strategy() {
	case v1alpha1.SelectSemVer:
		return s.pickSemVer(candidates)

	case v1alpha1.SelectLexical:
		// Correct for zero-padded date and build-number schemes, wrong for
		// anything else — which is why SemVer is the default.
		return slices.Max(candidates), nil

	case v1alpha1.SelectDigest:
		// Track one fixed tag and react when its digest moves. The constraint
		// names the tag, since there is nothing to choose between.
		if s.Constraint == "" {
			return "", fmt.Errorf("selection strategy Digest requires constraint to name the tag to track")
		}
		if !slices.Contains(candidates, s.Constraint) {
			return "", &ErrNoMatch{Reason: fmt.Sprintf("tag %q not found", s.Constraint)}
		}
		return s.Constraint, nil

	case v1alpha1.SelectNewestBuild:
		// Deliberately unimplemented rather than approximated. Ordering by push
		// time means fetching every manifest, and registry timestamps are not
		// always what people assume. Returning a plausible-but-wrong tag here
		// would silently promote the wrong image.
		return "", fmt.Errorf(
			"selection strategy NewestBuild is not implemented; use SemVer with a constraint, " +
				"or Lexical for zero-padded date or build-number tags")

	default:
		return "", fmt.Errorf("unknown selection strategy %q", s.Strategy)
	}
}

func (s Selection) strategy() v1alpha1.TagSelection {
	if s.Strategy == "" {
		return v1alpha1.SelectSemVer
	}
	return s.Strategy
}

// filter applies Allow then Ignore. Both exist to keep mutable tags — latest,
// main, nightly — from causing surprise crossings.
func (s Selection) filter(tags []string) ([]string, error) {
	var allow *regexp.Regexp
	if s.Allow != "" {
		var err error
		if allow, err = regexp.Compile(s.Allow); err != nil {
			return nil, fmt.Errorf("invalid allow pattern %q: %w", s.Allow, err)
		}
	}

	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if allow != nil && !allow.MatchString(t) {
			continue
		}
		if slices.Contains(s.Ignore, t) {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// pickSemVer returns the highest version satisfying the constraint.
//
// Tags that are not valid semver are skipped rather than failing the whole
// selection: real repositories carry `latest`, `main` and dated tags alongside
// releases, and one unparseable tag must not stop discovery.
func (s Selection) pickSemVer(tags []string) (string, error) {
	var constraint *semver.Constraints
	if s.Constraint != "" {
		var err error
		if constraint, err = semver.NewConstraint(s.Constraint); err != nil {
			return "", fmt.Errorf("invalid semver constraint %q: %w", s.Constraint, err)
		}
	}

	type candidate struct {
		tag string
		ver *semver.Version
	}
	var matched []candidate

	for _, t := range tags {
		v, err := semver.NewVersion(t)
		if err != nil {
			continue // not a version; ignore
		}
		if constraint != nil {
			// A constraint already excludes prereleases unless it opts into
			// them (">= 6.0.0-0"), so defer to it.
			if !constraint.Check(v) {
				continue
			}
		} else if v.Prerelease() != "" {
			// No constraint: exclude prereleases explicitly, so the
			// unconstrained path has the same semantics as the constrained
			// one. Otherwise `7.0.0-beta.1` outranks `6.2.0` and a Beacon with
			// no constraint quietly ships a beta. To opt in, set a constraint
			// such as ">= 0.0.0-0".
			continue
		}
		matched = append(matched, candidate{tag: t, ver: v})
	}

	if len(matched) == 0 {
		reason := "no tags parsed as semantic versions"
		if s.Constraint != "" {
			reason = fmt.Sprintf("no semver tags satisfied constraint %q", s.Constraint)
		}
		return "", &ErrNoMatch{Reason: reason}
	}

	sort.Slice(matched, func(i, j int) bool { return matched[i].ver.LessThan(matched[j].ver) })
	return matched[len(matched)-1].tag, nil
}
