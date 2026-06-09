// Package version exposes the service version string.
//
// The canonical version is the constant below. When building release binaries,
// the version can be overridden at link time:
//
//	go build -ldflags="-X github.com/valianx/discord-support-hub/internal/version.Version=v0.1.0"
//
// Runners that consume this value (e.g. the API startup log) should import this package.
package version

// Version is the service version. Override with -ldflags at release time.
// The value below matches the current CHANGELOG.md [0.1.0] entry.
var Version = "v0.1.0"
