package store

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/dejo1307/git-stats/internal/github"
)

// Layout of a data directory:
//
//	<data>/raw/<timestamp>/*.json   immutable archive, the source of truth
//	<data>/stats.db                 derived, disposable, rebuildable
const (
	rawSubdir = "raw"
	dbName    = "stats.db"
)

// ArchiveDir returns the raw archive root inside a data directory.
func ArchiveDir(dataDir string) string { return filepath.Join(dataDir, rawSubdir) }

// DBPath returns the derived database path inside a data directory.
func DBPath(dataDir string) string { return filepath.Join(dataDir, dbName) }

// Rebuild discards the derived database and replays the entire archive into a
// fresh one. Because both `collect` and `rebuild` go through Ingest, a rebuild
// reproduces exactly what collection produced — which is what makes the
// database safe to delete.
func Rebuild(dataDir string, namer github.AssetNamer) (snapshots int, err error) {
	path := DBPath(dataDir)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return 0, fmt.Errorf("removing %s: %w", path, err)
	}
	// SQLite side files would otherwise survive the rebuild.
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			return 0, fmt.Errorf("removing %s: %w", path+suffix, err)
		}
	}

	db, err := Open(path)
	if err != nil {
		return 0, err
	}
	defer db.Close()

	snaps, err := NewArchive(ArchiveDir(dataDir)).List()
	if err != nil {
		return 0, err
	}
	for _, snap := range snaps {
		if err := Ingest(db, snap, namer); err != nil {
			return 0, fmt.Errorf("replaying %s: %w", snap.Dir, err)
		}
	}
	return len(snaps), nil
}
