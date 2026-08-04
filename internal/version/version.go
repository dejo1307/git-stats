// Package version carries the build's version string.
package version

// Version is the release this binary was built from, without a leading "v".
// The release workflow injects the tag with
//
//	-ldflags "-X github.com/dejo1307/git-stats/internal/version.Version=1.2.3"
//
// so an unstamped local build reports "dev" rather than claiming a release.
var Version = "dev"
