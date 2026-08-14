package store

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dejo1307/git-stats/internal/github"
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
	FileForks      = "forks.json"

	// commitsPrefix marks the per-tracked-path commit histories. One file per
	// path, each naming its own path inside, so replaying an archive does not
	// depend on the tracked-path setting still holding its old value.
	commitsPrefix = "commits-"

	// commitCacheSubdir holds per-commit diff stats, shared by every snapshot
	// rather than stored per run: a commit is immutable, so one fetch is good
	// forever. It sits inside the archive root, which List ignores because its
	// name is not a timestamp.
	commitCacheSubdir = "commits"

	// userCacheSubdir holds one stargazer profile per file, shared across
	// snapshots for the same reason the commit cache is. A profile is not
	// immutable the way a commit is, so these records carry a fetch time and an
	// ETag and are revalidated rather than trusted forever.
	userCacheSubdir = "users"
)

// Archive is the append-only directory of raw API responses.
type Archive struct{ Root string }

// NewArchive returns an archive rooted at dir.
func NewArchive(dir string) *Archive { return &Archive{Root: dir} }

// Snapshot is one archived collection run.
type Snapshot struct {
	TakenAt time.Time
	Dir     string
	// Root is the archive the snapshot belongs to, which is where per-commit
	// data shared across runs lives.
	Root string
}

// Create makes the directory for a new snapshot. Runs are keyed to the second,
// so two collections in the same second reuse one directory rather than
// silently splitting a run in two.
func (a *Archive) Create(takenAt time.Time) (Snapshot, error) {
	dir := filepath.Join(a.Root, takenAt.UTC().Format(dirLayout))
	if err := ensureDir(dir); err != nil {
		return Snapshot{}, err
	}
	return Snapshot{TakenAt: takenAt.UTC().Truncate(time.Second), Dir: dir, Root: a.Root}, nil
}

// CommitsFile is the archive filename holding one tracked path's history.
// Characters a path may contain but a filename should not are replaced, and a
// hash of the original is appended whenever that substitution loses
// information, so two paths cannot collide onto one file.
func CommitsFile(path string) string {
	var b strings.Builder
	lossy := false
	for _, r := range path {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
			lossy = true
		}
	}
	name := b.String()
	if lossy {
		h := fnv.New32a()
		fmt.Fprint(h, path)
		name = fmt.Sprintf("%s-%08x", name, h.Sum32())
	}
	return commitsPrefix + name + ".json"
}

// CommitsFiles lists the tracked-path histories captured in this snapshot.
func (s Snapshot) CommitsFiles() ([]string, error) {
	entries, err := os.ReadDir(s.Dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading snapshot %s: %w", s.Dir, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), commitsPrefix) &&
			strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// CommitDetailPath is where one commit's diff stats are cached.
func (s Snapshot) CommitDetailPath(sha string) string {
	return filepath.Join(s.Root, commitCacheSubdir, sha+".json")
}

// WriteCommitDetail caches one commit's diff stats for every snapshot to use.
func (s Snapshot) WriteCommitDetail(sha string, raw []byte) error {
	path := s.CommitDetailPath(sha)
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return err
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// ReadCommitDetail returns a cached commit's diff stats. found is false when
// the commit has not been fetched in detail, which is the normal state until
// `backfill` runs.
func (s Snapshot) ReadCommitDetail(sha string) (raw []byte, found bool, err error) {
	raw, err = os.ReadFile(s.CommitDetailPath(sha))
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}

// UserPath is where one account's archived profile record lives.
//
// The login is checked rather than escaped: GitHub logins are letters, digits
// and hyphens only, so anything else did not come from the API and has no
// business being turned into a path.
func (s Snapshot) UserPath(login string) (string, error) {
	if !github.ValidLogin(login) {
		return "", fmt.Errorf("invalid login %q", login)
	}
	return filepath.Join(s.Root, userCacheSubdir, login+".json"), nil
}

// WriteUser caches one account's profile record for every snapshot to use.
func (s Snapshot) WriteUser(login string, raw []byte) error {
	path, err := s.UserPath(login)
	if err != nil {
		return err
	}
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return err
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// ReadUser returns a cached profile record. found is false when the account has
// not been fetched, which is the normal state until `backfill-users` runs.
func (s Snapshot) ReadUser(login string) (rec github.UserRecord, found bool, err error) {
	path, err := s.UserPath(login)
	if err != nil {
		return rec, false, err
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return rec, false, nil
	}
	if err != nil {
		return rec, false, err
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		return rec, false, fmt.Errorf("decoding %s: %w", path, err)
	}
	return rec, true, nil
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
		snaps = append(snaps, Snapshot{
			TakenAt: takenAt, Dir: filepath.Join(a.Root, e.Name()), Root: a.Root})
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
