package collect_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dejo1307/git-stats/internal/collect"
	"github.com/dejo1307/git-stats/internal/github"
	"github.com/dejo1307/git-stats/internal/store"
)

// profileServer serves releases, repo, one stargazer list and the profiles
// behind it, counting how often each profile was actually fetched.
type profileServer struct {
	*httptest.Server
	profileHits atomic.Int64
	// etag is what the profile endpoint claims; an If-None-Match matching it is
	// answered 304, which is what the real API does and what makes a refresh
	// free.
	etag string
	// email is the profile email served, so a test can assert the difference
	// between an authenticated and an unauthenticated fetch.
	email string
}

func newProfileServer(t *testing.T, logins ...string) *profileServer {
	t.Helper()
	ps := &profileServer{etag: `"v1"`, email: "ada@example.org"}
	ps.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/traffic/"):
			// Not what these tests are about; a token without traffic access is
			// the ordinary case anyway.
			w.Header().Set("X-RateLimit-Remaining", "4999")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"Must have push access to repository"}`)
		case strings.HasSuffix(r.URL.Path, "/releases"):
			fmt.Fprint(w, releasesBody)
		case strings.HasSuffix(r.URL.Path, "/stargazers"):
			var entries []string
			for i, l := range logins {
				entries = append(entries, fmt.Sprintf(
					`{"starred_at":"2026-08-0%dT00:00:00Z","user":{"login":%q}}`, i+1, l))
			}
			fmt.Fprintf(w, "[%s]", strings.Join(entries, ","))
		case strings.HasPrefix(r.URL.Path, "/users/"):
			ps.profileHits.Add(1)
			w.Header().Set("ETag", ps.etag)
			if r.Header.Get("If-None-Match") == ps.etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			login := strings.TrimPrefix(r.URL.Path, "/users/")
			// GitHub withholds the email from an unauthenticated caller while
			// still answering 200 with the rest of the profile.
			email := "null"
			if r.Header.Get("Authorization") != "" {
				email = fmt.Sprintf("%q", ps.email)
			}
			fmt.Fprintf(w, `{"login":%q,"name":"Ada Lovelace","email":%s,
				"company":"@analytical","blog":"https://ada.example",
				"location":"London","bio":"notes","twitter_username":"ada",
				"followers":12,"public_repos":7,"type":"User",
				"created_at":"2015-01-02T03:04:05Z"}`, login, email)
		default:
			fmt.Fprint(w, `{"stargazers_count":2,"forks_count":1,"subscribers_count":1}`)
		}
	}))
	t.Cleanup(ps.Close)
	return ps
}

func usersConfig(dir string, srv *profileServer, now time.Time) collect.Config {
	return collect.Config{
		Repo:    "o/r",
		Token:   "t",
		DataDir: dir,
		APIBase: srv.URL,
		Assets:  github.NewAssetNamer("widget"),
		Users:   true,
		Now:     now,
		Log:     io.Discard,
	}
}

func TestUsersBackfillsProfilesAndIsCachedAcrossRuns(t *testing.T) {
	srv := newProfileServer(t, "ada", "grace")
	dir := t.TempDir()
	first := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)

	res, err := collect.Run(context.Background(), usersConfig(dir, srv, first))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Profiles != 2 {
		t.Errorf("Profiles = %d, want 2", res.Profiles)
	}
	if got := srv.profileHits.Load(); got != 2 {
		t.Errorf("profile requests = %d, want 2", got)
	}

	db, err := store.Open(store.DBPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var name, email, company string
	var followers int64
	if err := db.QueryRow(`SELECT name, email, company, followers
		FROM stargazer_profile WHERE login = 'ada'`).
		Scan(&name, &email, &company, &followers); err != nil {
		t.Fatalf("reading the derived profile: %v", err)
	}
	if name != "Ada Lovelace" || email != "ada@example.org" ||
		company != "@analytical" || followers != 12 {
		t.Errorf("profile row = %q/%q/%q/%d", name, email, company, followers)
	}

	// A second run must not pay for the same accounts again: the cache lives
	// under the archive root, not inside the snapshot that wrote it.
	srv.profileHits.Store(0)
	second, err := collect.Run(context.Background(),
		usersConfig(dir, srv, first.Add(24*time.Hour)))
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if got := srv.profileHits.Load(); got != 0 {
		t.Errorf("second run made %d profile requests, want 0", got)
	}
	if second.Profiles != 0 {
		t.Errorf("second run fetched %d profiles, want 0", second.Profiles)
	}
}

func TestUserRefreshRevalidatesAndStaysUnchanged(t *testing.T) {
	srv := newProfileServer(t, "ada")
	dir := t.TempDir()
	start := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)

	if _, err := collect.Run(context.Background(), usersConfig(dir, srv, start)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Aged past the refresh window, so the profile is re-checked — but GitHub
	// answers 304 and charges nothing, which is the whole point of storing the
	// ETag beside the body.
	cfg := usersConfig(dir, srv, start.Add(48*time.Hour))
	cfg.UserRefresh = 24 * time.Hour
	srv.profileHits.Store(0)
	res, err := collect.Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("refresh Run: %v", err)
	}
	if got := srv.profileHits.Load(); got != 1 {
		t.Errorf("refresh made %d profile requests, want 1", got)
	}
	if res.ProfilesUnchanged != 1 || res.Profiles != 0 {
		t.Errorf("refresh = %d unchanged / %d fetched, want 1 / 0",
			res.ProfilesUnchanged, res.Profiles)
	}

	// The freshness stamp must have moved, or the next refresh asks again
	// immediately and the 304 saves nothing but bandwidth.
	snaps, err := store.NewArchive(store.ArchiveDir(dir)).List()
	if err != nil || len(snaps) == 0 {
		t.Fatalf("listing the archive: %v", err)
	}
	rec, found, err := snaps[0].ReadUser("ada")
	if err != nil || !found {
		t.Fatalf("reading the cached record: %v (found %v)", err, found)
	}
	if !rec.FetchedAt.Equal(start.Add(48 * time.Hour)) {
		t.Errorf("FetchedAt = %s, want the revalidation time", rec.FetchedAt)
	}
	if u, err := rec.Decode(); err != nil || u.Name != "Ada Lovelace" {
		t.Errorf("a 304 must keep the cached body: %+v (err %v)", u, err)
	}
}

func TestUsersSurvivesAMissingAccount(t *testing.T) {
	// A star outlives the account that left it. That is not a failure of the
	// run and must not lose the other profiles.
	srv := newProfileServer(t, "ada", "ghost")
	base := srv.Config.Handler
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/users/ghost" {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		base.ServeHTTP(w, r)
	})
	dir := t.TempDir()

	res, err := collect.Run(context.Background(),
		usersConfig(dir, srv, time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("a deleted account must not fail the run: %v", err)
	}
	if res.Profiles != 1 || res.ProfilesMissing != 1 {
		t.Errorf("got %d profiles / %d missing, want 1 / 1", res.Profiles, res.ProfilesMissing)
	}
}

func TestUsersStopsCleanlyOnRateLimitAndResumes(t *testing.T) {
	srv := newProfileServer(t, "ada", "grace", "edsger")
	base := srv.Config.Handler
	// The budget runs out after the first account, as it would part-way through
	// a repository with more stargazers than an hour buys.
	var exhausted atomic.Bool
	exhausted.Store(true)
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if exhausted.Load() && strings.HasPrefix(r.URL.Path, "/users/") &&
			r.URL.Path != "/users/ada" {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", "1786722177")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
			return
		}
		base.ServeHTTP(w, r)
	})
	dir := t.TempDir()

	var log strings.Builder
	cfg := usersConfig(dir, srv, time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC))
	cfg.Log = &log
	res, err := collect.Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("running out of budget is not a failure: %v", err)
	}
	if res.Profiles != 1 {
		t.Errorf("Profiles = %d, want the one fetched before the limit", res.Profiles)
	}
	if res.ProfilesRemaining != 2 {
		t.Errorf("ProfilesRemaining = %d, want 2", res.ProfilesRemaining)
	}
	if !strings.Contains(log.String(), "rate limit") {
		t.Errorf("the warning must name the cause:\n%s", log.String())
	}

	// Once the budget refills, the next run resumes at the first account
	// without a cached profile rather than starting the crawl over.
	exhausted.Store(false)
	srv.profileHits.Store(0)
	res, err = collect.Run(context.Background(),
		usersConfig(dir, srv, time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("resuming: %v", err)
	}
	if res.Profiles != 2 {
		t.Errorf("resumed run fetched %d, want the 2 that were missed", res.Profiles)
	}
	if got := srv.profileHits.Load(); got != 2 {
		t.Errorf("resumed run made %d requests, want 2 — the cached one is not refetched", got)
	}
}

func TestUnauthenticatedProfilesWarnAndKeepNoETag(t *testing.T) {
	// The endpoint answers 200 without a credential and returns the profile
	// with a null email, so nothing about the response says the crawl was
	// pointless. Two things have to happen: say so, and refuse to keep an ETag
	// that stands for the emailless body — revalidating against it later, once
	// a token exists, could confirm a profile the tool never saw in full.
	srv := newProfileServer(t, "ada")
	dir := t.TempDir()

	var log strings.Builder
	cfg := usersConfig(dir, srv, time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC))
	cfg.Token = ""
	cfg.Log = &log
	if _, err := collect.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(log.String(), "without a token") {
		t.Errorf("no warning about the missing token:\n%s", log.String())
	}

	snaps, err := store.NewArchive(store.ArchiveDir(dir)).List()
	if err != nil || len(snaps) == 0 {
		t.Fatalf("listing the archive: %v", err)
	}
	rec, found, err := snaps[0].ReadUser("ada")
	if err != nil || !found {
		t.Fatalf("reading the cached record: %v (found %v)", err, found)
	}
	if rec.ETag != "" {
		t.Errorf("ETag = %q, want none kept for an unauthenticated fetch", rec.ETag)
	}

	// The archived bytes are the response verbatim, so the null email is
	// visible in the archive rather than smoothed into an empty string.
	var body map[string]any
	if err := json.Unmarshal(rec.Profile, &body); err != nil {
		t.Fatal(err)
	}
	if body["email"] != nil {
		t.Errorf("email = %v, want null", body["email"])
	}
}
