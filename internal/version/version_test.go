// Package version_test verifies the service version constant (M5, AC-6).
//
// The Version variable must be set to "v0.1.0" in the source so that:
//   - The startup log always emits the correct version.
//   - Build tooling can override it at release time with -ldflags without a default
//     that would silently use a wrong version on un-tagged builds.
package version_test

import (
	"strings"
	"testing"

	"github.com/valianx/discord-support-hub/internal/version"
)

// TestVersion_IsV010 verifies that the Version constant is set to "v0.1.0" (AC-6).
// This is the source-level value; release builds override it via
// -ldflags="-X github.com/valianx/discord-support-hub/internal/version.Version=<tag>".
func TestVersion_IsV010(t *testing.T) {
	if version.Version != "v0.1.0" {
		t.Errorf("version.Version: want %q, got %q", "v0.1.0", version.Version)
	}
}

// TestVersion_SemverFormat verifies the Version string uses the semver "vMAJOR.MINOR.PATCH"
// format (AC-6 — semver followed). A release candidate or pre-release suffix is also
// acceptable (e.g. "v0.1.0-rc1"), so we only check the "v" prefix and numeric components.
func TestVersion_SemverFormat(t *testing.T) {
	v := version.Version
	if !strings.HasPrefix(v, "v") {
		t.Errorf("version.Version must start with 'v'; got %q", v)
	}
	// Strip the 'v' and verify the remainder has at least two dots (major.minor.patch).
	rest := v[1:]
	if strings.Count(rest, ".") < 2 {
		t.Errorf("version.Version must follow vMAJOR.MINOR.PATCH format; got %q", v)
	}
}
