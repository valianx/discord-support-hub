// Package domain_test verifies the import-boundary constraint (AC-3):
// internal/domain must not import any infrastructure package.
package domain_test

import (
	"go/build"
	"strings"
	"testing"
)

// TestDomainImportsNoInfra asserts that the domain package does not import
// any infra package: store, queue, discord, ratelimit, lock, cache, secrets, oauth.
// This is the AC-3 compile-time boundary check.
func TestDomainImportsNoInfra(t *testing.T) {
	pkg, err := build.Import("github.com/valianx/discord-support-hub/internal/domain", ".", build.ImportComment)
	if err != nil {
		t.Fatalf("build.Import domain: %v", err)
	}

	forbidden := []string{
		"store",
		"queue",
		"discord",
		"ratelimit",
		"lock",
		"cache",
		"secrets",
		"oauth",
		"pgx",
		"asynq",
		"discordgo",
		"go-redis",
	}

	for _, imp := range pkg.Imports {
		for _, f := range forbidden {
			if strings.Contains(imp, f) {
				t.Errorf("domain imports infra package %q (forbidden: %q)", imp, f)
			}
		}
	}
}
