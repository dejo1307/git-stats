package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReleasesFollowsPagination(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "", "1":
			// Advertise a second page the way the API does.
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/releases?per_page=100&page=2>; rel="next"`, srv.URL))
			fmt.Fprint(w, `[{"tag_name":"v0.2.0","assets":[{"id":1,"name":"a.tar.gz","download_count":5}]}]`)
		case "2":
			fmt.Fprint(w, `[{"tag_name":"v0.1.0","assets":[{"id":2,"name":"b.tar.gz","download_count":7}]}]`)
		default:
			t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
		}
	}))
	defer srv.Close()

	c := New("o/r", "")
	c.BaseURL = srv.URL

	releases, raw, err := c.Releases(context.Background())
	if err != nil {
		t.Fatalf("Releases: %v", err)
	}
	if len(releases) != 2 {
		t.Fatalf("got %d releases across pages, want 2", len(releases))
	}
	if releases[1].Assets[0].DownloadCount != 7 {
		t.Errorf("second page asset count = %d, want 7", releases[1].Assets[0].DownloadCount)
	}
	// The archived bytes must contain both pages merged, or a rebuild would
	// silently lose everything past page one.
	if len(raw) == 0 {
		t.Fatal("no raw bytes returned for the archive")
	}
	var check []Release
	if err := json.Unmarshal(raw, &check); err != nil || len(check) != 2 {
		t.Errorf("merged archive bytes did not round-trip to 2 releases (err=%v)", err)
	}
}

func TestForbiddenIsDistinguishedFromRateLimit(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		remaining     string
		wantForbidden bool
	}{
		{"permission denied", http.StatusForbidden, "4999", true},
		{"invisible repo", http.StatusNotFound, "4999", true},
		// A 403 with no remaining quota is rate limiting, not a permission
		// problem — retrying later fixes it, minting a new token does not.
		{"rate limited", http.StatusForbidden, "0", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-RateLimit-Remaining", tt.remaining)
				w.Header().Set("X-RateLimit-Reset", "1785827178")
				w.WriteHeader(tt.status)
				fmt.Fprint(w, `{"message":"denied"}`)
			}))
			defer srv.Close()

			c := New("o/r", "t")
			c.BaseURL = srv.URL
			_, _, err := c.Views(context.Background())
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := IsForbidden(err); got != tt.wantForbidden {
				t.Errorf("IsForbidden(%v) = %v, want %v", err, got, tt.wantForbidden)
			}
		})
	}
}

func TestRetriesServerErrors(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, `{"stargazers_count":78,"forks_count":11,"subscribers_count":2}`)
	}))
	defer srv.Close()

	c := New("o/r", "")
	c.BaseURL = srv.URL
	repo, _, err := c.Repository(context.Background())
	if err != nil {
		t.Fatalf("Repository: %v", err)
	}
	if repo.Stars != 78 {
		t.Errorf("stars = %d, want 78", repo.Stars)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2 (one failure then a retry)", calls)
	}
}

func TestRepositoryPrefersSubscribersOverWatchers(t *testing.T) {
	// watchers_count duplicates the star count; subscribers_count is the real
	// watcher figure. Confusing them silently reports stars twice.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"stargazers_count":78,"watchers_count":78,"subscribers_count":2}`)
	}))
	defer srv.Close()

	c := New("o/r", "")
	c.BaseURL = srv.URL
	repo, _, err := c.Repository(context.Background())
	if err != nil {
		t.Fatalf("Repository: %v", err)
	}
	if repo.Subscribers != 2 {
		t.Errorf("watchers = %d, want 2", repo.Subscribers)
	}
}
