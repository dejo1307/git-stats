package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// dirLayout is an RFC3339-shaped, filesystem-safe timestamp for archive
// directory names. Lexical order matches chronological order.
const dirLayout = "2006-01-02T15-04-05Z"

// Archive file names. A file is absent when that endpoint was unavailable
// (for example traffic data without a sufficiently permissioned token).
const (
	FileReleases   = "releases.json"
	FileRepo       = "repo.json"
	FileViews      = "views.json"
	FileClones     = "clones.json"
	FilePaths      = "paths.json"
	FileReferrers  = "referrers.json"
	FileStargazers = "stargazers.json"
)

// Archive is the append-only directory of raw API responses.
type Archive struct{ Root string }

// NewArchive returns an archive rooted at dir.
func NewArchive(dir string) *Archive { return &Archive{Root: dir} }

// Snapshot is one archived collection run.
type Snapshot struct {
	TakenAt time.Time
	Dir     string
}

// Create makes the directory for a new snapshot. Runs are keyed to the second,
// so two collections in the same second reuse one directory rather than
// silently splitting a run in two.
func (a *Archive) Create(takenAt time.Time) (Snapshot, error) {
	dir := filepath.Join(a.Root, takenAt.UTC().Format(dirLayout))
	if err := ensureDir(dir); err != nil {
		return Snapshot{}, err
	}
	return Snapshot{TakenAt: takenAt.UTC().Truncate(time.Second), Dir: dir}, nil
}

// Write stores one raw response verbatim.
func (s Snapshot) Write(name string, raw []byte) error {
	path := filepath.Join(s.Dir, name)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// Read returns an archived response. found is false when the endpoint was not
// captured in this run.
func (s Snapshot) Read(name string) (raw []byte, found bool, err error) {
	raw, err = os.ReadFile(filepath.Join(s.Dir, name))
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}

// List returns every archived snapshot in chronological order. Directories
// whose names are not timestamps are ignored, so notes or scratch files inside
// the archive root are harmless.
func (a *Archive) List() ([]Snapshot, error) {
	entries, err := os.ReadDir(a.Root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading archive %s: %w", a.Root, err)
	}

	var snaps []Snapshot
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		takenAt, err := time.ParseInLocation(dirLayout, e.Name(), time.UTC)
		if err != nil {
			continue
		}
		snaps = append(snaps, Snapshot{TakenAt: takenAt, Dir: filepath.Join(a.Root, e.Name())})
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].TakenAt.Before(snaps[j].TakenAt) })
	return snaps, nil
}

func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	return nil
}
