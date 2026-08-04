package collect_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dejo1307/git-stats/internal/collect"
	"github.com/dejo1307/git-stats/internal/github"
	"github.com/dejo1307/git-stats/internal/store"
)

const releasesBody = `[{"tag_name":"v0.3.6","assets":[
	{"id":1,"name":"widget-0.3.6-darwin-arm64.tar.gz","download_count":11,"size":100,"created_at":"2026-08-03T00:00:00Z"},
	{"id":2,"name":"widget-0.3.6-darwin-arm64.sha256","download_count":9,"size":10,"created_at":"2026-08-03T00:00:00Z"}]}]`

// newServer serves the release and repo endpoints, and answers the traffic
// endpoints with trafficStatus.
func newServer(t *testing.T, trafficStatus int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/traffic/"):
			if trafficStatus != http.StatusOK {
				w.Header().Set("X-RateLimit-Remaining", "4999")
				w.WriteHeader(trafficStatus)
				fmt.Fprint(w, `{"message":"Must have push access to repository"}`)
				return
			}
			switch {
			case strings.HasSuffix(r.URL.Path, "/views"):
				fmt.Fprint(w, `{"count":9,"uniques":4,"views":[{"timestamp":"2026-08-03T00:00:00Z","count":9,"uniques":4}]}`)
			case strings.HasSuffix(r.URL.Path, "/clones"):
				fmt.Fprint(w, `{"count":3,"uniques":2,"clones":[{"timestamp":"2026-08-03T00:00:00Z","count":3,"uniques":2}]}`)
			case strings.HasSuffix(r.URL.Path, "/paths"):
				fmt.Fprint(w, `[{"path":"/owner/widget","title":"widget","count":40,"uniques":12}]`)
			default:
				fmt.Fprint(w, `[{"referrer":"news.ycombinator.com","count":22,"uniques":15}]`)
			}
		case strings.HasSuffix(r.URL.Path, "/releases"):
			fmt.Fprint(w, releasesBody)
		default:
			fmt.Fprint(w, `{"stargazers_count":78,"forks_count":11,"subscribers_count":2}`)
		}
	}))
}

func TestRunCapturesEverything(t *testing.T) {
	srv := newServer(t, http.StatusOK)
	defer srv.Close()
	dir := t.TempDir()

	res, err := collect.Run(context.Background(), collect.Config{
		Repo:    "o/r",
		DataDir: dir,
		APIBase: srv.URL,
		Assets:  github.NewAssetNamer("widget"),
		Now:     time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC),
		Log:     io.Discard,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.TrafficSkipped {
		t.Error("traffic reported as skipped despite a permissioned token")
	}
	if res.Total != 20 {
		t.Errorf("cumulative total = %d, want 20", res.Total)
	}

	// Every response must land in the archive verbatim, since that archive is
	// what a rebuild replays.
	for _, name := range []string{
		store.FileReleases, store.FileRepo, store.FileViews,
		store.FileClones, store.FilePaths, store.FileReferrers,
	} {
		if _, err := os.Stat(filepath.Join(res.Snapshot.Dir, name)); err != nil {
			t.Errorf("archive is missing %s: %v", name, err)
		}
	}

	db, err := store.Open(store.DBPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	totals, err := db.LatestTotals()
	if err != nil {
		t.Fatal(err)
	}
	if totals.ByPlatform["darwin-arm64"] != 20 {
		t.Errorf("darwin-arm64 total = %d, want 20", totals.ByPlatform["darwin-arm64"])
	}
	if totals.ByKind["tar.gz"] != 11 || totals.ByKind["sha256"] != 9 {
		t.Errorf("kind split = %d/%d, want 11/9", totals.ByKind["tar.gz"], totals.ByKind["sha256"])
	}

	views, err := db.Traffic("views")
	if err != nil || len(views) != 1 || views[0].Count != 9 {
		t.Errorf("views = %+v (err %v), want one day with count 9", views, err)
	}
	referrers, err := db.LatestTop("referrer")
	if err != nil || len(referrers) != 1 || referrers[0].Name != "news.ycombinator.com" {
		t.Errorf("referrers = %+v (err %v)", referrers, err)
	}
}

func TestRunSurvivesForbiddenTraffic(t *testing.T) {
	srv := newServer(t, http.StatusForbidden)
	defer srv.Close()
	dir := t.TempDir()

	var log strings.Builder
	res, err := collect.Run(context.Background(), collect.Config{
		Repo:    "o/r",
		DataDir: dir,
		APIBase: srv.URL,
		Assets:  github.NewAssetNamer("widget"),
		Now:     time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC),
		Log:     &log,
	})
	// Releases are cumulative and can be caught up later, so a run that
	// captured them is worth keeping even with no traffic access.
	if err != nil {
		t.Fatalf("Run must not fail when only traffic is forbidden: %v", err)
	}
	if !res.TrafficSkipped {
		t.Error("TrafficSkipped not set despite 403s")
	}
	// The warning must point at the fix that actually works. A fine-grained
	// token is refused by the traffic endpoints regardless of its permissions,
	// so naming a permission to add would send the reader in circles.
	for _, want := range []string{"CLASSIC", "public_repo"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("warning does not mention %q:\n%s", want, log.String())
		}
	}
	if _, err := os.Stat(filepath.Join(res.Snapshot.Dir, store.FileReleases)); err != nil {
		t.Errorf("releases were not archived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(res.Snapshot.Dir, store.FileViews)); !os.IsNotExist(err) {
		t.Error("an unavailable endpoint must not leave a file behind")
	}
}

func TestPartialRunIsStillIngested(t *testing.T) {
	// Releases succeed, then a later endpoint fails hard. What was captured is
	// real data and must reach the database — otherwise the archive and the
	// database silently diverge until someone runs `rebuild`.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/releases") {
			fmt.Fprint(w, releasesBody)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"message":"nope"}`)
	}))
	defer srv.Close()
	dir := t.TempDir()

	_, err := collect.Run(context.Background(), collect.Config{
		Repo:    "o/r",
		DataDir: dir,
		APIBase: srv.URL,
		Assets:  github.NewAssetNamer("widget"),
		Now:     time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC),
		Log:     io.Discard,
	})
	if err == nil {
		t.Fatal("expected the run to report the failure")
	}

	db, dbErr := store.Open(store.DBPath(dir))
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	defer db.Close()

	totals, dbErr := db.LatestTotals()
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	if totals.Total != 20 {
		t.Errorf("partial snapshot total = %d, want 20 (releases were captured before the failure)",
			totals.Total)
	}
}

func TestRunTwiceProducesTwoSnapshotsAndNoPhantomDownloads(t *testing.T) {
	srv := newServer(t, http.StatusOK)
	defer srv.Close()
	dir := t.TempDir()

	base := collect.Config{Repo: "o/r", DataDir: dir, APIBase: srv.URL,
		Assets: github.NewAssetNamer("widget"), Log: io.Discard}
	for _, at := range []time.Time{
		time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC),
	} {
		cfg := base
		cfg.Now = at
		if _, err := collect.Run(context.Background(), cfg); err != nil {
			t.Fatalf("Run at %s: %v", at, err)
		}
	}

	db, err := store.Open(store.DBPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	snaps, err := db.Snapshots()
	if err != nil || len(snaps) != 2 {
		t.Fatalf("snapshots = %d (err %v), want 2", len(snaps), err)
	}
	intervals, err := db.Intervals()
	if err != nil {
		t.Fatal(err)
	}
	if len(intervals) != 1 {
		t.Fatalf("intervals = %d, want 1", len(intervals))
	}
	// Unchanged counters between runs mean zero downloads, not a re-count of
	// the all-time total.
	if intervals[0].Total != 0 {
		t.Errorf("interval total = %d, want 0 for identical counters", intervals[0].Total)
	}
}
