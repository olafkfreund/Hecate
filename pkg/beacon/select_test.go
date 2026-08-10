package beacon

import (
	"errors"
	"testing"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// A realistic tag list: releases mixed with the mutable and prerelease tags
// every real repository actually carries.
var realistic = []string{
	"latest", "main", "6.0.0", "6.1.0", "6.2.0-rc.1", "6.2.0", "7.0.0-beta.1", "5.9.3", "nightly-20260810",
}

func TestPickSemVer(t *testing.T) {
	tests := []struct {
		name       string
		tags       []string
		constraint string
		want       string
	}{
		{
			name: "highest release wins, mutable tags ignored",
			tags: realistic, want: "6.2.0",
		},
		{
			name: "constraint pins the major", tags: realistic,
			constraint: "^6.0.0", want: "6.2.0",
		},
		{
			name: "constraint pins the minor", tags: realistic,
			constraint: "~6.1.0", want: "6.1.0",
		},
		{
			// A prerelease must not be selected by a plain range — shipping
			// 7.0.0-beta.1 because it sorts highest is exactly the surprise
			// this guards against.
			name: "prereleases are excluded from plain ranges", tags: realistic,
			constraint: ">= 6.0.0", want: "6.2.0",
		},
		{
			name: "v prefix is tolerated", tags: []string{"v1.2.3", "v1.3.0"},
			want: "v1.3.0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Selection{Constraint: tt.constraint}.Pick(tt.tags)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("Pick = %q, want %q", got, tt.want)
			}
		})
	}
}

// SemVer is the default so an unset strategy behaves sensibly.
func TestPickDefaultsToSemVer(t *testing.T) {
	got, err := Selection{Strategy: ""}.Pick(realistic)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "6.2.0" {
		t.Errorf("Pick = %q, want 6.2.0", got)
	}
}

func TestAllowAndIgnore(t *testing.T) {
	t.Run("allow pattern restricts candidates", func(t *testing.T) {
		got, err := Selection{
			Strategy: v1alpha1.SelectLexical,
			Allow:    `^\d+\.\d+\.\d+$`, // releases only: no latest, main, nightly
		}.Pick(realistic)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "6.2.0" {
			t.Errorf("Pick = %q, want 6.2.0", got)
		}
	})

	t.Run("ignore removes specific tags", func(t *testing.T) {
		got, err := Selection{Ignore: []string{"6.2.0"}}.Pick(realistic)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "6.1.0" {
			t.Errorf("Pick = %q, want 6.1.0 after ignoring 6.2.0", got)
		}
	})

	t.Run("invalid allow pattern is an error, not a silent pass", func(t *testing.T) {
		if _, err := (Selection{Allow: "["}).Pick(realistic); err == nil {
			t.Error("expected an error for an uncompilable allow pattern")
		}
	})
}

func TestPickLexical(t *testing.T) {
	// The scheme Lexical is actually for: zero-padded dates.
	tags := []string{"20260101", "20260810", "20251231"}
	got, err := Selection{Strategy: v1alpha1.SelectLexical}.Pick(tags)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "20260810" {
		t.Errorf("Pick = %q, want 20260810", got)
	}
}

func TestPickDigest(t *testing.T) {
	t.Run("tracks the named tag", func(t *testing.T) {
		got, err := Selection{Strategy: v1alpha1.SelectDigest, Constraint: "stable"}.
			Pick([]string{"stable", "6.2.0"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "stable" {
			t.Errorf("Pick = %q, want stable", got)
		}
	})

	t.Run("requires a tag to track", func(t *testing.T) {
		if _, err := (Selection{Strategy: v1alpha1.SelectDigest}).Pick(realistic); err == nil {
			t.Error("expected an error when no tag is named")
		}
	})

	t.Run("missing tag is no-match, not an error", func(t *testing.T) {
		_, err := Selection{Strategy: v1alpha1.SelectDigest, Constraint: "absent"}.Pick(realistic)
		var noMatch *ErrNoMatch
		if !errors.As(err, &noMatch) {
			t.Errorf("want ErrNoMatch, got %T: %v", err, err)
		}
	})
}

// Better to refuse than to promote a plausible-but-wrong image.
func TestNewestBuildIsRefusedNotApproximated(t *testing.T) {
	_, err := Selection{Strategy: v1alpha1.SelectNewestBuild}.Pick(realistic)
	if err == nil {
		t.Fatal("NewestBuild must not silently return a guess")
	}
	var noMatch *ErrNoMatch
	if errors.As(err, &noMatch) {
		t.Error("unimplemented is a configuration error, not a no-match")
	}
}

// "Nothing matched" must be distinguishable from "something broke": the first
// is a normal state a Beacon sits in, the second needs a human.
func TestNoMatchIsDistinguishableFromFailure(t *testing.T) {
	t.Run("unsatisfiable constraint is a no-match", func(t *testing.T) {
		_, err := Selection{Constraint: "^99.0.0"}.Pick(realistic)
		var noMatch *ErrNoMatch
		if !errors.As(err, &noMatch) {
			t.Errorf("want ErrNoMatch, got %T: %v", err, err)
		}
	})

	t.Run("no semver tags at all is a no-match", func(t *testing.T) {
		_, err := Selection{}.Pick([]string{"latest", "main"})
		var noMatch *ErrNoMatch
		if !errors.As(err, &noMatch) {
			t.Errorf("want ErrNoMatch, got %T: %v", err, err)
		}
	})

	t.Run("invalid constraint is a real error", func(t *testing.T) {
		_, err := Selection{Constraint: "not-a-constraint"}.Pick(realistic)
		var noMatch *ErrNoMatch
		if err == nil || errors.As(err, &noMatch) {
			t.Errorf("a malformed constraint must be a hard error, got %T: %v", err, err)
		}
	})

	t.Run("empty tag list is a no-match", func(t *testing.T) {
		_, err := Selection{}.Pick(nil)
		var noMatch *ErrNoMatch
		if !errors.As(err, &noMatch) {
			t.Errorf("want ErrNoMatch, got %T: %v", err, err)
		}
	})
}

// The bug this guards: with no constraint, a prerelease sorts highest and a
// Beacon quietly ships a beta. Opting in must still be possible.
func TestPrereleasesExcludedWithoutConstraint(t *testing.T) {
	tags := []string{"6.2.0", "7.0.0-beta.1"}

	got, err := Selection{}.Pick(tags)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "6.2.0" {
		t.Errorf("Pick = %q, want 6.2.0: an unconstrained watch must not select a prerelease", got)
	}

	got, err = Selection{Constraint: ">= 0.0.0-0"}.Pick(tags)
	if err != nil {
		t.Fatalf("unexpected error opting in: %v", err)
	}
	if got != "7.0.0-beta.1" {
		t.Errorf("Pick = %q, want 7.0.0-beta.1 when the constraint opts into prereleases", got)
	}
}
