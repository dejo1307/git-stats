// Command git-stats collects GitHub distribution metrics for a repository
// and reports trends over time.
//
// GitHub exposes release download counts only as cumulative totals with no
// history, and keeps traffic data for just 14 days. Neither can be
// reconstructed after the fact, so this tool snapshots them, archives every
// raw response, and derives trends from the archive.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dejo1307/git-stats/internal/collect"
	"github.com/dejo1307/git-stats/internal/dotenv"
	"github.com/dejo1307/git-stats/internal/github"
	"github.com/dejo1307/git-stats/internal/report"
	"github.com/dejo1307/git-stats/internal/store"
)

// Environment variables, all of which may equally come from a .env file.
const (
	// envRepo names the repository to track, as "owner/name". There is no
	// default: pointing the tool at somebody else's repository by accident
	// would silently archive the wrong numbers.
	envRepo = "GIT_STATS_REPO"
	// envAssetPrefix overrides the release asset filename prefix, which
	// otherwise defaults to the repository name.
	envAssetPrefix = "GIT_STATS_ASSET_PREFIX"
	// envDir overrides the default data directory.
	envDir = "GIT_STATS_DIR"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "git-stats:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `git-stats — GitHub distribution metrics over time

usage:
  git-stats collect [-repo R] [-data DIR] [-stars]
  git-stats report  [-repo R] [-since 30d] [-per-day] [-html FILE]
  git-stats rebuild [-repo R] [-data DIR]
  git-stats backfill-stars [-repo R] [-data DIR]

commands:
  collect         snapshot every available endpoint into the archive and database
  report          summarise trends (terminal, or -html for a dashboard)
  rebuild         discard the database and replay the whole archive into a new one
  backfill-stars  fetch the complete star history (each star carries its own date)

environment:
  GIT_STATS_REPO          repository to track, as owner/name. Required; -repo overrides it.
  GIT_STATS_TOKEN         API token; falls back to GH_TOKEN, then GITHUB_TOKEN.
                          Traffic metrics need a CLASSIC token with the public_repo
                          scope; releases and repo counters work unauthenticated.
  GIT_STATS_ASSET_PREFIX  release asset filename prefix, for the per-platform
                          breakdown of <prefix>-<version>-<os>-<arch>.<ext>.
                          Defaults to the repository name.
  GIT_STATS_DIR           default data directory (otherwise ./data)

All of these may instead be set in a .env file, read from the working directory or
from the directory holding the binary. Exported variables take precedence over it.
`)
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("no command given")
	}

	// A .env alongside the tool supplies defaults for anything not already
	// exported, so the token can live in a gitignored file.
	env, err := dotenv.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	switch args[0] {
	case "collect":
		return runCollect(ctx, args[1:], false, env)
	case "backfill-stars":
		return runCollect(ctx, args[1:], true, env)
	case "report":
		return runReport(args[1:])
	case "rebuild":
		return runRebuild(args[1:])
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// dataDirFlag registers the shared -data flag.
func dataDirFlag(fs *flag.FlagSet) *string {
	def := os.Getenv(envDir)
	if def == "" {
		def = "data"
	}
	return fs.String("data", def, "data directory holding raw/ and stats.db")
}

// repoFlag registers the shared -repo flag, defaulting to the configured
// repository so the flag is only needed to override it for one run.
func repoFlag(fs *flag.FlagSet, usage string) *string {
	return fs.String("repo", os.Getenv(envRepo), usage)
}

// resolveRepo validates the repository, because an unset one otherwise
// surfaces as an opaque 404 from an API call to /repos//releases.
func resolveRepo(repo string) (string, error) {
	repo = strings.Trim(strings.TrimSpace(repo), "/")
	if repo == "" {
		return "", fmt.Errorf("no repository configured: set %s=owner/name in .env "+
			"(copy .env.example) or pass -repo owner/name", envRepo)
	}
	if strings.Count(repo, "/") != 1 {
		return "", fmt.Errorf("invalid repository %q: expected owner/name", repo)
	}
	return repo, nil
}

// assetNamer builds the release-asset filename parser for a repository. The
// prefix defaults to the repository name, which is what release tooling
// overwhelmingly uses, and is overridable for projects that publish under a
// different name.
func assetNamer(repo string) github.AssetNamer {
	prefix := strings.TrimSpace(os.Getenv(envAssetPrefix))
	if prefix == "" {
		prefix = github.RepoName(repo)
	}
	return github.NewAssetNamer(prefix)
}

func runCollect(ctx context.Context, args []string, forceStars bool, env dotenv.Loaded) error {
	fs := flag.NewFlagSet("collect", flag.ContinueOnError)
	repoArg := repoFlag(fs, "owner/name to collect")
	data := dataDirFlag(fs)
	stars := fs.Bool("stars", false, "also backfill the full stargazer history")
	if err := fs.Parse(args); err != nil {
		return err
	}
	repo, err := resolveRepo(*repoArg)
	if err != nil {
		return err
	}

	token, source := github.TokenFrom()
	cfg := collect.Config{
		Repo:    repo,
		Token:   token,
		DataDir: *data,
		Assets:  assetNamer(repo),
		Stars:   *stars || forceStars,
		Log:     os.Stdout,
	}

	// Name the credential's origin: a 403 on the traffic endpoints is almost
	// always a broadly-scoped fallback token being used instead of the
	// dedicated one.
	switch {
	case token == "":
		fmt.Printf("note: no token found (looked for %s); collecting unauthenticated "+
			"— releases and repo counters only\n", strings.Join(github.TokenVars, ", "))
	case env.Supplied(source):
		fmt.Printf("token: %s from %s\n", source, env.Path)
	default:
		fmt.Printf("token: %s from the environment\n", source)
	}

	res, err := collect.Run(ctx, cfg)
	if err != nil {
		return err
	}
	fmt.Printf("\nSnapshot %s complete.\n", filepath.Base(res.Snapshot.Dir))
	return nil
}

func runReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	repoArg := repoFlag(fs, "repository label for the report header")
	data := dataDirFlag(fs)
	since := fs.String("since", "", "limit deltas to a trailing window, e.g. 30d or 12h")
	perDay := fs.Bool("per-day", false, "also show deltas normalised to a daily rate")
	html := fs.String("html", "", "write a self-contained HTML dashboard to this file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	repo, err := resolveRepo(*repoArg)
	if err != nil {
		return err
	}

	window, err := parseDuration(*since)
	if err != nil {
		return err
	}

	db, err := store.Open(store.DBPath(*data))
	if err != nil {
		return err
	}
	defer db.Close()

	opts := report.Options{Repo: repo, Since: window, PerDay: *perDay}
	if *html != "" {
		if err := report.HTMLFile(*html, db, opts); err != nil {
			return err
		}
		fmt.Printf("wrote %s\n", *html)
		return nil
	}
	return report.Text(os.Stdout, db, opts)
}

func runRebuild(args []string) error {
	fs := flag.NewFlagSet("rebuild", flag.ContinueOnError)
	repoArg := repoFlag(fs, "owner/name the archive was collected from")
	data := dataDirFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	// The repository is needed even though nothing is fetched: replaying the
	// archive re-derives the per-platform columns from asset filenames.
	repo, err := resolveRepo(*repoArg)
	if err != nil {
		return err
	}
	n, err := store.Rebuild(*data, assetNamer(repo))
	if err != nil {
		return err
	}
	fmt.Printf("replayed %d snapshot(s) from %s into %s\n",
		n, store.ArchiveDir(*data), store.DBPath(*data))
	return nil
}

// parseDuration accepts Go durations plus a "d" suffix for days, which is the
// natural unit for a window measured in snapshots.
func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseFloat(days, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid -since %q: %w", s, err)
		}
		return time.Duration(n * float64(24*time.Hour)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid -since %q: %w", s, err)
	}
	return d, nil
}
