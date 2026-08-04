// Package version reports the build's version string.
package version

import (
	"runtime/debug"
	"strings"
)

// Version is the release this binary was built from, without a leading "v".
// The release workflow injects the tag with
//
//	-ldflags "-X github.com/dejo1307/git-stats/internal/version.Version=1.2.3"
//
// It is empty in any build that did not go through that workflow; call String
// rather than reading it directly.
var Version = ""

// String reports the version, falling back to the module version the Go
// toolchain records and finally to "dev".
//
// The fallback is what makes `go install github.com/dejo1307/git-stats/...@v1.2.3`
// honest: that path applies no ldflags, so without it every proxy-installed
// binary would claim to be "dev" while being a tagged release. It also gives a
// build from a git working tree something better than "dev" — the toolchain
// derives a version from the nearest tag and marks uncommitted changes, so a
// local build reports e.g. "1.2.3+dirty".
func String() string {
	if Version != "" {
		return Version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		// "(devel)" is what the toolchain records when it has no version to
		// report at all — no tag, or no VCS information stamped in.
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return strings.TrimPrefix(v, "v")
		}
	}
	return "dev"
}
