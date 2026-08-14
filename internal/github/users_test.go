package github_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dejo1307/git-stats/internal/github"
)

func TestUserProfileRevalidatesWithTheStoredETag(t *testing.T) {
	const etag = `"abc123"`
	var conditional int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			conditional++
			w.WriteHeader(http.StatusNotModified)
			return
		}
		fmt.Fprint(w, `{"login":"ada","name":"Ada","email":"ada@example.org"}`)
	}))
	defer srv.Close()

	c := github.New("o/r", "t")
	c.BaseURL = srv.URL

	raw, gotETag, unchanged, err := c.UserProfile(context.Background(), "ada", "")
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if unchanged || gotETag != etag || !strings.Contains(string(raw), "ada@example.org") {
		t.Fatalf("first fetch = %q / %q / unchanged %v", raw, gotETag, unchanged)
	}

	// The point of keeping the ETag: GitHub answers 304 and does not charge the
	// rate limit, so re-checking thousands of profiles costs nothing.
	raw, gotETag, unchanged, err = c.UserProfile(context.Background(), "ada", etag)
	if err != nil {
		t.Fatalf("revalidation: %v", err)
	}
	if !unchanged || raw != nil || gotETag != etag {
		t.Errorf("revalidation = %q / %q / unchanged %v, want a 304", raw, gotETag, unchanged)
	}
	if conditional != 1 {
		t.Errorf("the server saw %d conditional requests, want 1", conditional)
	}
}

func TestUserProfileClassifiesRateLimitApartFromPermission(t *testing.T) {
	// Both arrive as 403. One is fixed by waiting and the other by a different
	// credential, and a crawl priced per account has to tell them apart: the
	// first means "stop and resume", the second means "skip this account".
	for _, tc := range []struct {
		name      string
		remaining string
		limited   bool
	}{
		{"exhausted budget", "0", true},
		{"insufficient permission", "4999", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-RateLimit-Remaining", tc.remaining)
				w.Header().Set("X-RateLimit-Reset", "1786722177")
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `{"message":"Forbidden"}`)
			}))
			defer srv.Close()

			c := github.New("o/r", "t")
			c.BaseURL = srv.URL
			_, _, _, err := c.UserProfile(context.Background(), "ada", "")
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := github.IsRateLimited(err); got != tc.limited {
				t.Errorf("IsRateLimited = %v, want %v (%v)", got, tc.limited, err)
			}
			if got := github.IsForbidden(err); got == tc.limited {
				t.Errorf("IsForbidden = %v, want %v (%v)", got, !tc.limited, err)
			}
		})
	}
}

func TestValidLogin(t *testing.T) {
	for _, ok := range []string{"ada", "Ada-Lovelace", "a", "user99", strings.Repeat("a", 39)} {
		if !github.ValidLogin(ok) {
			t.Errorf("ValidLogin(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "../x", "a/b", "a b", "a_b", "a.b", "a@b",
		strings.Repeat("a", 40)} {
		if github.ValidLogin(bad) {
			t.Errorf("ValidLogin(%q) = true", bad)
		}
	}
}

func TestUserProfileRejectsALoginBeforeSendingIt(t *testing.T) {
	// Nothing must reach the network, because the value is also used as a
	// filename and a rejection after the fetch would be a rejection too late.
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	defer srv.Close()

	c := github.New("o/r", "t")
	c.BaseURL = srv.URL
	if _, _, _, err := c.UserProfile(context.Background(), "../escape", ""); err == nil {
		t.Error("an invalid login was accepted")
	}
	if hits != 0 {
		t.Errorf("the server was contacted %d times", hits)
	}
}
