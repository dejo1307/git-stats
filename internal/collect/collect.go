// Package collect performs one metrics collection run: fetch every available
// endpoint, archive the raw responses, then derive database rows from the
// archive it just wrote.
package collect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/dejo1307/git-stats/internal/github"
	"github.com/dejo1307/git-stats/internal/store"
)

// Config parameterises a collection run.
type Config struct {
	Repo    string
	Token   string
	DataDir string
	// Assets splits release asset filenames into platform columns. A zero
	// value records every asset without them.
	Assets github.AssetNamer
	// APIBase overrides the GitHub host; tests point it at an httptest server.
	APIBase string
	// Now is the collection timestamp; zero means time.Now().
	Now time.Time
	// Stars additionally backfills the full stargazer history, which is a
	// paginated crawl and so is off by default.
	Stars bool
	// Forks additionally backfills the full fork history. Like Stars it is a
	// paginated crawl over a list that dates itself, so one run is complete.
	Forks bool
	// TrackPaths are repository paths whose commit history is captured, to date
	// the changes a reader would have noticed — README.md above all. One cheap
	// request each, so this runs on every collection.
	TrackPaths []string
	// CommitDetails additionally fetches each tracked commit's diff size, which
	// costs one request per commit and is cached permanently.
	CommitDetails bool
	// Log receives progress and warnings.
	Log io.Writer
}

// Result summarises what a run captured.
type Result struct {
	Snapshot store.Snapshot
	Releases int
	Assets   int
	Total    int64
	// Commits counts commits captured across every tracked path.
	Commits int
	// Details counts per-commit diff sizes newly fetched this run.
	Details int
	// TrafficSkipped is set when the traffic endpoints were unavailable,
	// which is the expected outcome for anything but a classic token.
	TrafficSkipped bool
	// StarsSkipped is set when the stargazers endpoint rejected the credential.
	StarsSkipped bool
}

// Run executes one collection. Traffic failures are non-fatal: releases are
// cumulative and can be caught up at any time, so a run that captures them is
// worth keeping even when the traffic endpoints are inaccessible.
func Run(ctx context.Context, cfg Config) (Result, error) {
	now := cfg.Now
	if now.IsZero() {
		now = time.Now()
	}

	client := github.New(cfg.Repo, cfg.Token)
	if cfg.APIBase != "" {
		client.BaseURL = cfg.APIBase
	}

	archive := store.NewArchive(store.ArchiveDir(cfg.DataDir))
	snap, err := archive.Create(now)
	if err != nil {
		return Result{}, err
	}
	res := Result{Snapshot: snap}

	// Whatever was captured before a failure is still real data, so it is
	// ingested either way. Otherwise a run that died late would leave an
	// archived snapshot that the database only learns about on the next
	// `rebuild`.
	fetchErr := fetch(ctx, cfg, client, snap, &res)

	db, err := store.Open(store.DBPath(cfg.DataDir))
	if err != nil {
		return res, errors.Join(fetchErr, err)
	}
	defer db.Close()

	if err := store.Ingest(db, snap, cfg.Assets); err != nil {
		return res, errors.Join(fetchErr, fmt.Errorf("ingesting snapshot: %w", err))
	}
	if fetchErr != nil {
		logf(cfg, "archived a partial snapshot at %s before failing\n", snap.Dir)
		return res, fetchErr
	}
	logf(cfg, "archived %s and ingested into %s\n", snap.Dir, db.Path)
	return res, nil
}

// fetch performs every endpoint call, writing each response into the archive
// as it arrives.
func fetch(ctx context.Context, cfg Config, client *github.Client, snap store.Snapshot, res *Result) error {
	releases, raw, err := client.Releases(ctx)
	if err != nil {
		return fmt.Errorf("fetching releases: %w", err)
	}
	if err := snap.Write(store.FileReleases, raw); err != nil {
		return err
	}
	res.Releases = len(releases)
	for _, r := range releases {
		res.Assets += len(r.Assets)
		for _, a := range r.Assets {
			res.Total += a.DownloadCount
		}
	}
	logf(cfg, "releases: %d releases, %d assets, %d cumulative downloads\n",
		res.Releases, res.Assets, res.Total)

	if _, raw, err := client.Repository(ctx); err != nil {
		return fmt.Errorf("fetching repository: %w", err)
	} else if err := snap.Write(store.FileRepo, raw); err != nil {
		return err
	}

	// Traffic is the perishable half: GitHub keeps only 14 days, so anything
	// not captured here is gone permanently.
	traffic := []struct {
		name  string
		file  string
		fetch func(context.Context) ([]byte, error)
	}{
		{"views", store.FileViews, func(ctx context.Context) ([]byte, error) {
			_, raw, err := client.Views(ctx)
			return raw, err
		}},
		{"clones", store.FileClones, func(ctx context.Context) ([]byte, error) {
			_, raw, err := client.Clones(ctx)
			return raw, err
		}},
		{"popular paths", store.FilePaths, func(ctx context.Context) ([]byte, error) {
			_, raw, err := client.PopularPaths(ctx)
			return raw, err
		}},
		{"referrers", store.FileReferrers, func(ctx context.Context) ([]byte, error) {
			_, raw, err := client.PopularReferrers(ctx)
			return raw, err
		}},
	}
	for _, t := range traffic {
		raw, err := t.fetch(ctx)
		if github.IsForbidden(err) {
			res.TrafficSkipped = true
			continue
		}
		if err != nil {
			return fmt.Errorf("fetching %s: %w", t.name, err)
		}
		if err := snap.Write(t.file, raw); err != nil {
			return err
		}
	}
	if res.TrafficSkipped {
		// Fine-grained PATs are rejected by the traffic endpoints even when
		// they hold the administration=read permission those endpoints
		// advertise. Only a classic token works, so say so rather than
		// sending the reader back to the permission checkboxes.
		logf(cfg, "WARNING: traffic metrics unavailable for %s.\n"+
			"         The Traffic API requires a CLASSIC personal access token with the\n"+
			"         'public_repo' scope. Fine-grained tokens are refused here even with\n"+
			"         Administration:read — no permission setting changes that.\n"+
			"         Views, clones, paths and referrers are kept by GitHub for only 14 days;\n"+
			"         days not captured in that window cannot be recovered.\n", cfg.Repo)
	} else {
		logf(cfg, "traffic: views, clones, paths and referrers captured\n")
	}

	if err := fetchTracked(ctx, cfg, client, snap, res); err != nil {
		return err
	}

	if cfg.Forks {
		forks, raw, err := client.Forks(ctx)
		switch {
		case github.IsForbidden(err):
			logf(cfg, "WARNING: fork history unavailable — the forks endpoint rejected "+
				"this credential.\n"+
				"         Fork counts are still tracked per snapshot from the repository "+
				"endpoint.\n")
		case err != nil:
			return fmt.Errorf("fetching forks: %w", err)
		default:
			if err := snap.Write(store.FileForks, raw); err != nil {
				return err
			}
			logf(cfg, "forks: %d forks with timestamps\n", len(forks))
		}
	}

	if cfg.Stars {
		stars, raw, err := client.Stargazers(ctx)
		switch {
		case github.IsForbidden(err):
			// The star *count* over time still comes from the repository
			// endpoint above; only the retroactive per-star timeline is lost.
			res.StarsSkipped = true
			logf(cfg, "WARNING: star history unavailable — the stargazers endpoint rejected "+
				"this credential.\n"+
				"         Star counts are still tracked per snapshot from the repository "+
				"endpoint;\n"+
				"         only the backfilled per-star timeline is missing.\n")
		case err != nil:
			return fmt.Errorf("fetching stargazers: %w", err)
		default:
			if err := snap.Write(store.FileStargazers, raw); err != nil {
				return err
			}
			logf(cfg, "stars: %d stargazers with timestamps\n", len(stars))
		}
	}

	return nil
}

// fetchTracked captures the commit history of every tracked path.
//
// This is the one series here that is both complete and cheap: GitHub imposes
// no retention on commit history, so a single request per path reaches back to
// the repository's first commit, and re-running it costs the same request
// again. That makes README changes datable retroactively, unlike traffic.
func fetchTracked(ctx context.Context, cfg Config, client *github.Client,
	snap store.Snapshot, res *Result) error {
	for _, path := range cfg.TrackPaths {
		commits, raw, err := client.CommitsForPath(ctx, path)
		if github.IsForbidden(err) {
			logf(cfg, "WARNING: no commit history for %s — the endpoint rejected this "+
				"credential or the path does not exist.\n", path)
			continue
		}
		if err != nil {
			return fmt.Errorf("fetching commits for %s: %w", path, err)
		}
		if err := snap.Write(store.CommitsFile(path), raw); err != nil {
			return err
		}
		res.Commits += len(commits)
		logf(cfg, "tracked: %s changed in %d commit(s)\n", path, len(commits))

		if !cfg.CommitDetails {
			continue
		}
		// A commit is immutable, so a cached detail is never refetched. Only
		// the first backfill pays the per-commit request.
		for _, c := range commits {
			if _, found, err := snap.ReadCommitDetail(c.SHA); err != nil {
				return err
			} else if found {
				continue
			}
			_, detailRaw, err := client.CommitDetail(ctx, c.SHA)
			if err != nil {
				return fmt.Errorf("fetching commit %s: %w", c.SHA, err)
			}
			if err := snap.WriteCommitDetail(c.SHA, detailRaw); err != nil {
				return err
			}
			res.Details++
		}
	}
	if res.Details > 0 {
		logf(cfg, "tracked: fetched %d new commit diff size(s)\n", res.Details)
	}
	return nil
}

func logf(cfg Config, format string, args ...any) {
	if cfg.Log == nil {
		return
	}
	fmt.Fprintf(cfg.Log, format, args...)
}
