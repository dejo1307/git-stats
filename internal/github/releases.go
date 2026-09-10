package github

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Asset is one uploaded file on a release. DownloadCount is a cumulative
// counter maintained by GitHub since the asset was uploaded; it has no
// history, which is the whole reason this tool snapshots it.
type Asset struct {
	ID            int64     `json:"id"`
	Name          string    `json:"name"`
	Label         string    `json:"label"`
	State         string    `json:"state"`
	Size          int64     `json:"size"`
	DownloadCount int64     `json:"download_count"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Release is one published (or draft) release.
type Release struct {
	ID          int64     `json:"id"`
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	CreatedAt   time.Time `json:"created_at"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []Asset   `json:"assets"`
}

// Releases returns every release, newest first, along with the merged raw JSON
// to archive.
func (c *Client) Releases(ctx context.Context) ([]Release, []byte, error) {
	items, err := c.getPaged(ctx, "/repos/"+c.Repo+"/releases?per_page=100", "application/vnd.github+json")
	if err != nil {
		return nil, nil, err
	}
	raw, err := mergeRaw(items)
	if err != nil {
		return nil, nil, err
	}
	var releases []Release
	if err := json.Unmarshal(raw, &releases); err != nil {
		return nil, nil, fmt.Errorf("decoding releases: %w", err)
	}
	return releases, raw, nil
}

// AssetName is the platform breakdown parsed out of a release asset filename.
type AssetName struct {
	Version string // "0.3.6"
	OS      string // "darwin", "linux", "windows"
	Arch    string // "amd64", "arm64"
	Kind    string // "tar.gz", "zip", "sha256", "upgrade.sha256", …
}

// assetKinds are the extensions recognised as either a release artifact or a
// checksum for one. upgrade.sha256 is a checksum only a self-updater fetches,
// which is what lets its downloads be counted apart from installs.
const assetKinds = `tar\.gz|tgz|tar\.xz|tar\.bz2|zip|upgrade\.sha256|sha256|sha512`

// AssetNamer parses release asset filenames for one project.
//
// GoReleaser and hand-rolled release workflows converge on
// <prefix>-<version>-<goos>-<goarch>.<ext>, where the prefix is nearly always
// the repository name — so that is the default, overridable for projects that
// publish under a different name than their repository.
type AssetNamer struct{ re *regexp.Regexp }

// NewAssetNamer returns a namer for assets carrying the given filename prefix.
// An empty prefix yields a namer that matches nothing, so assets are recorded
// with empty platform columns rather than mis-parsed.
func NewAssetNamer(prefix string) AssetNamer {
	if prefix == "" {
		return AssetNamer{}
	}
	return AssetNamer{re: regexp.MustCompile(
		`^` + regexp.QuoteMeta(prefix) + `-(.+)-([a-z0-9]+)-([a-z0-9]+)\.(` + assetKinds + `)$`)}
}

// Parse splits a release asset filename. ok is false for anything that does
// not follow the scheme, which is stored with empty platform columns rather
// than dropped — an unrecognised asset still has a real download count.
func (n AssetNamer) Parse(name string) (AssetName, bool) {
	if n.re == nil {
		return AssetName{}, false
	}
	m := n.re.FindStringSubmatch(name)
	if m == nil {
		return AssetName{}, false
	}
	return AssetName{
		Version: m[1],
		OS:      m[2],
		Arch:    m[3],
		Kind:    m[4],
	}, true
}

// Platform renders the "os-arch" label used across reports.
func (a AssetName) Platform() string {
	if a.OS == "" || a.Arch == "" {
		return "unknown"
	}
	return a.OS + "-" + a.Arch
}

// Version strips a leading "v" from a tag so tags and asset names agree.
func Version(tag string) string { return strings.TrimPrefix(tag, "v") }

// RepoName is the name half of an "owner/name" repository, which is the
// default asset filename prefix.
func RepoName(repo string) string {
	if _, name, found := strings.Cut(repo, "/"); found {
		return name
	}
	return repo
}
