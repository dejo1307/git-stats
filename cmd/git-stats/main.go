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
	"github.com/dejo1307/git-stats/internal/version"
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
	// envTrackPaths lists repository paths whose commit history is captured, so
	// changes to them can be lined up against the acquisition series. Comma
	// separated; a trailing slash tracks a whole directory.
	envTrackPaths = "GIT_STATS_TRACK_PATHS"
	// envDir overrides the default data directory.
	envDir = "GIT_STATS_DIR"
	// envAPIBase overrides the API host. It exists for GitHub Enterprise
	// Server, whose REST API lives under https://HOST/api/v3, and for
	// pointing a run at a local fixture server, which is how the recordings
	// in the README are made.
	envAPIBase = "GIT_STATS_API_BASE"
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
  git-stats collect [-env FILE] [-repo R] [-data DIR] [-stars] [-forks] [-users] [-track PATHS]
  git-stats report  [-env FILE] [-repo R] [-since 30d] [-per-day] [-html FILE] [-contacts]
  git-stats stargazers [-with-email] [-name RE] [-email RE] [-company RE] [-location RE]
                       [-forked] [-people] [-since 30d] [-limit N] [-format table|csv|emails]
  git-stats rebuild [-env FILE] [-repo R] [-data DIR]
  git-stats backfill [-env FILE] [-repo R] [-data DIR]
  git-stats backfill-stars [-env FILE] [-repo R] [-data DIR]
  git-stats backfill-users [-env FILE] [-repo R] [-data DIR] [-user-refresh 30d]
  git-stats version

  -env names a configuration file instead of searching for .env, which is how one
  binary tracks several repositories. Give each its own file, and set
  GIT_STATS_DIR in every one so they cannot share a data directory.

commands:
  collect         snapshot every available endpoint into the archive and database
  report          summarise trends (terminal, or -html for a dashboard). -contacts adds
                  the stargazer list to the dashboard — searchable and paginated, 25
                  rows at a time. Opt-in, because it turns a file about download counts
                  into one holding other people's names and addresses.
  stargazers      list who starred, with whatever public profile was fetched. Every
                  pattern is a case-insensitive regexp and they AND together, so
                  -location berlin -with-email is one list. -format emails prints
                  bare addresses, deduplicated, for piping somewhere else.
                  Needs backfill-users to have run, or every name is blank.
  rebuild         discard the database and replay the whole archive into a new one
  backfill        fetch every history that dates itself — stars, forks, the diff size
                  of each tracked commit, and each stargazer's public profile.
                  Complete back to the first commit and worth running once; the
                  per-commit and per-account halves are cached forever after.
  backfill-stars  the star history alone
  backfill-users  the stargazer profiles alone — one request per account against an
                  hourly budget of 5000, so a large repository takes several runs.
                  Each is cached, so a later run resumes rather than starting over.
                  Only works on repositories you own: GitHub refuses the stargazer
                  list for everyone else's.

environment:
  GIT_STATS_REPO          repository to track, as owner/name. Required; -repo overrides it.
  GIT_STATS_TOKEN         API token; falls back to GH_TOKEN, then GITHUB_TOKEN.
                          Traffic metrics need a CLASSIC token with the public_repo
                          scope; releases and repo counters work unauthenticated.
  GIT_STATS_ASSET_PREFIX  release asset filename prefix, for the per-platform
                          breakdown of <prefix>-<version>-<os>-<arch>.<ext>.
                          Defaults to the repository name.
  GIT_STATS_TRACK_PATHS   comma-separated paths whose commit history is captured, so
                          changes to them can be lined up against the numbers.
                          Defaults to README.md; a trailing slash tracks a directory.
  GIT_STATS_DIR           default data directory (otherwise ./data)
  GIT_STATS_API_BASE      API host (otherwise https://api.github.com). Point it at
                          https://HOST/api/v3 for GitHub Enterprise Server.

All of these may instead be set in a .env file, read from the working directory or
from the directory holding the binary, or from the file named by -env. Exported
variables take precedence over it. A -env file that does not exist is an error,
rather than a silent fall back to whichever .env happens to be nearby.
`)
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("no command given")
	}

	// A .env alongside the tool supplies defaults for anything not already
	// exported, so the token can live in a gitignored file. -env names a
	// different one, which is how one binary tracks several repositories.
	env, err := loadEnv(args[1:])
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	switch args[0] {
	case "collect":
		return runCollect(ctx, args[1:], backfill{}, env)
	case "backfill-stars":
		return runCollect(ctx, args[1:], backfill{Stars: true}, env)
	case "backfill-users":
		return runCollect(ctx, args[1:], backfill{Users: true}, env)
	case "backfill":
		return runCollect(ctx, args[1:],
			backfill{Stars: true, Forks: true, CommitDetails: true, Users: true}, env)
	case "report":
		return runReport(args[1:])
	case "stargazers":
		return runStargazers(args[1:])
	case "rebuild":
		return runRebuild(args[1:])
	case "version", "-version", "--version":
		fmt.Println("git-stats", version.String())
		return nil
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// loadEnv applies the configuration file, honouring -env if it is present.
//
// The flag is read here, by hand, rather than through the FlagSet that later
// parses it. It has to be: every other flag takes its default from the
// environment, and this file is what supplies that environment, so a FlagSet
// cannot compute its own defaults from a value it has not parsed yet.
func loadEnv(args []string) (dotenv.Loaded, error) {
	if path := envFileArg(args); path != "" {
		return dotenv.LoadFile(path)
	}
	return dotenv.Load()
}

// envFileArg extracts -env from a command's arguments, accepting the four
// spellings Go's flag package would.
func envFileArg(args []string) string {
	for i, arg := range args {
		name, value, hasValue := strings.Cut(arg, "=")
		if name != "-env" && name != "--env" {
			continue
		}
		if hasValue {
			return value
		}
		if i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// envFlag registers -env so it parses and appears in -h. Its value is already
// applied by loadEnv; declaring it here is what keeps the FlagSet from
// rejecting it as unknown.
func envFlag(fs *flag.FlagSet) {
	fs.String("env", "",
		"read configuration from this file instead of searching for .env")
}

// dataDirFlag registers the shared -data flag.
func dataDirFlag(fs *flag.FlagSet) *string {
	def := os.Getenv(envDir)
	if def == "" {
		def = "data"
	}
	return fs.String("data", def, "data directory holding raw/ and stats.db")
}

// warnSharedDataDir fires when a named env file leaves the data directory at
// its default.
//
// Tracking two repositories through two -env files is the point of the flag,
// but if neither names a directory both runs land in ./data and interleave
// two repositories into one archive — releases from one and traffic from the
// other, in a single snapshot series that no rebuild can separate afterwards.
// The archive is the source of truth precisely so that cannot happen, so this
// is worth a word before the first collection rather than after the tenth.
func warnSharedDataDir(fs *flag.FlagSet, args []string, dataDir string) {
	if envFileArg(args) == "" || os.Getenv(envDir) != "" {
		return
	}
	explicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "data" {
			explicit = true
		}
	})
	if explicit {
		return
	}
	fmt.Printf("WARNING: -env was given but %s is not set, so this run uses the default\n"+
		"         data directory %q, relative to the working directory. Two repositories\n"+
		"         collected this way share one archive and cannot be separated later.\n"+
		"         Set %s in each env file, or pass -data.\n",
		envDir, dataDir, envDir)
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

// backfill selects the retroactive crawls a run performs on top of the ordinary
// per-snapshot fetches. Each is a paginated crawl or a per-commit request, so
// none of them runs by default.
type backfill struct {
	Stars         bool
	Forks         bool
	CommitDetails bool
	Users         bool
}

// trackPaths returns the repository paths whose commit history is captured.
// README.md is the default because it is the one file every visitor reads and
// the one whose rewrites plausibly move the numbers this tool collects.
func trackPaths() []string {
	raw := strings.TrimSpace(os.Getenv(envTrackPaths))
	if raw == "" {
		return []string{"README.md"}
	}
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func runCollect(ctx context.Context, args []string, force backfill, env dotenv.Loaded) error {
	fs := flag.NewFlagSet("collect", flag.ContinueOnError)
	envFlag(fs)
	repoArg := repoFlag(fs, "owner/name to collect")
	data := dataDirFlag(fs)
	stars := fs.Bool("stars", false, "also backfill the full stargazer history")
	forks := fs.Bool("forks", false, "also backfill the full fork history")
	details := fs.Bool("commit-details", false,
		"also fetch each tracked commit's diff size (one request per commit, cached)")
	users := fs.Bool("users", false,
		"also fetch each stargazer's public profile (one request per account, cached; implies -stars)")
	// A string rather than a flag.Duration, so this takes the same "30d"
	// spelling -since does. Days are the unit anyone reaches for here, and Go's
	// own duration syntax has no suffix for them.
	userRefresh := fs.String("user-refresh", "",
		"re-check cached profiles older than this, e.g. 30d; empty fetches only the ones never fetched")
	track := fs.String("track", strings.Join(trackPaths(), ","),
		"comma-separated paths whose commit history is captured")
	if err := fs.Parse(args); err != nil {
		return err
	}
	repo, err := resolveRepo(*repoArg)
	if err != nil {
		return err
	}
	warnSharedDataDir(fs, args, *data)

	refresh, err := parseDuration("-user-refresh", *userRefresh)
	if err != nil {
		return err
	}

	var paths []string
	for _, p := range strings.Split(*track, ",") {
		if p = strings.TrimSpace(p); p != "" {
			paths = append(paths, p)
		}
	}

	token, source := github.TokenFrom()
	cfg := collect.Config{
		Repo:          repo,
		Token:         token,
		DataDir:       *data,
		Assets:        assetNamer(repo),
		APIBase:       strings.TrimRight(strings.TrimSpace(os.Getenv(envAPIBase)), "/"),
		Stars:         *stars || force.Stars,
		Forks:         *forks || force.Forks,
		CommitDetails: *details || force.CommitDetails,
		Users:         *users || force.Users,
		UserRefresh:   refresh,
		TrackPaths:    paths,
		Log:           os.Stdout,
	}
	if cfg.APIBase != "" {
		fmt.Printf("api: %s\n", cfg.APIBase)
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
	envFlag(fs)
	repoArg := repoFlag(fs, "repository label for the report header")
	data := dataDirFlag(fs)
	since := fs.String("since", "", "limit deltas to a trailing window, e.g. 30d or 12h")
	perDay := fs.Bool("per-day", false, "also show deltas normalised to a daily rate")
	html := fs.String("html", "", "write a self-contained HTML dashboard to this file")
	contacts := fs.Bool("contacts", false,
		"add the stargazer list to the dashboard (-html only); it holds personal data")
	maxContacts := fs.Int("max-contacts", 0,
		"cap the embedded stargazer list, newest first; 0 uses the default of 2000")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *contacts && *html == "" {
		return errors.New("-contacts adds a section to the dashboard, so it needs -html FILE")
	}
	repo, err := resolveRepo(*repoArg)
	if err != nil {
		return err
	}

	window, err := parseDuration("-since", *since)
	if err != nil {
		return err
	}

	db, err := store.Open(store.DBPath(*data))
	if err != nil {
		return err
	}
	defer db.Close()

	opts := report.Options{
		Repo: repo, Since: window, PerDay: *perDay,
		Contacts: *contacts, MaxContacts: *maxContacts,
	}
	if *html != "" {
		if err := report.HTMLFile(*html, db, opts); err != nil {
			return err
		}
		fmt.Printf("wrote %s\n", *html)
		if *contacts {
			// Worth one line: this file was a chart of download counts and is
			// now also a contact list, and it is the artefact most likely to be
			// mailed on or dropped in a shared folder.
			coverage, err := db.ProfileCoverage()
			if err != nil {
				return err
			}
			fmt.Printf("note: it holds %d public profile(s), %d with an email address. "+
				"Treat it as personal data.\n", coverage.Profiles, coverage.WithEmail)
		}
		return nil
	}
	return report.Text(os.Stdout, db, opts)
}

// runStargazers lists who starred the repository, filtered.
//
// Separate from `report` rather than a section of it: a report is a summary
// somebody reads, while this is a list somebody pipes. Folding it in would
// mean either printing a contact list every time anyone asks for download
// trends, or burying it behind a flag on a command whose output is prose.
func runStargazers(args []string) error {
	fs := flag.NewFlagSet("stargazers", flag.ContinueOnError)
	envFlag(fs)
	data := dataDirFlag(fs)
	name := fs.String("name", "", "keep accounts whose name or login matches this regexp")
	email := fs.String("email", "", "keep accounts whose public email matches this regexp")
	company := fs.String("company", "", "keep accounts whose company matches this regexp")
	location := fs.String("location", "", "keep accounts whose location matches this regexp")
	withEmail := fs.Bool("with-email", false, "keep only accounts that publish an email")
	forked := fs.Bool("forked", false, "keep only accounts that also forked the repository")
	people := fs.Bool("people", false, "drop organisations and bots")
	since := fs.String("since", "", "keep only stars given inside a trailing window, e.g. 30d")
	limit := fs.Int("limit", 0, "stop after this many rows; 0 is all of them")
	format := fs.String("format", report.FormatTable,
		"table, csv, or emails for bare addresses one per line")
	if err := fs.Parse(args); err != nil {
		return err
	}

	window, err := parseDuration("-since", *since)
	if err != nil {
		return err
	}

	db, err := store.Open(store.DBPath(*data))
	if err != nil {
		return err
	}
	defer db.Close()

	return report.Stargazers(os.Stdout, db, report.StargazerOptions{
		Name: *name, Email: *email, Company: *company, Location: *location,
		WithEmail: *withEmail, Forked: *forked, People: *people,
		Since: window, Limit: *limit, Format: *format,
	})
}

func runRebuild(args []string) error {
	fs := flag.NewFlagSet("rebuild", flag.ContinueOnError)
	envFlag(fs)
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
// natural unit for both a window measured in snapshots and a cache lifetime.
// flag names the option in the error, since Go's own duration syntax rejects
// the "30d" spelling a reader would reach for first.
func parseDuration(flag, s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseFloat(days, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid %s %q: %w", flag, s, err)
		}
		return time.Duration(n * float64(24*time.Hour)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", flag, s, err)
	}
	return d, nil
}
