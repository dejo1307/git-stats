package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCommitsForPathCarriesItsPath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Query().Get("path")
		fmt.Fprint(w, `[{"sha":"abc","commit":{"message":"Rewrite the pitch\n\nbody",
			"author":{"date":"2026-01-01T00:00:00Z"},
			"committer":{"date":"2026-01-02T00:00:00Z"}}}]`)
	}))
	defer srv.Close()

	c := New("o/r", "")
	c.BaseURL = srv.URL

	commits, raw, err := c.CommitsForPath(context.Background(), "docs/guide.md")
	if err != nil {
		t.Fatalf("CommitsForPath: %v", err)
	}
	if gotPath != "docs/guide.md" {
		t.Errorf("requested path = %q, want docs/guide.md", gotPath)
	}
	if len(commits) != 1 || commits[0].SHA != "abc" {
		t.Fatalf("got %d commits, want 1 with sha abc", len(commits))
	}
	if got := commits[0].Subject(); got != "Rewrite the pitch" {
		t.Errorf("Subject() = %q, want the first line only", got)
	}

	// Replaying an archive must not depend on the tracked-path setting still
	// holding the value it had when the snapshot was taken, so the path travels
	// inside the archived bytes.
	var history PathCommits
	if err := json.Unmarshal(raw, &history); err != nil {
		t.Fatalf("archived bytes: %v", err)
	}
	if history.Path != "docs/guide.md" {
		t.Errorf("archived path = %q, want docs/guide.md", history.Path)
	}
	if len(history.Commits) != 1 {
		t.Errorf("archived %d commits, want 1", len(history.Commits))
	}
}

func TestCommitAtPrefersTheCommitterDate(t *testing.T) {
	// A rebase keeps the author date and rewrites the committer date. What a
	// timeline marker means is "a reader could now see this", which is when the
	// commit landed, not when it was written.
	var c Commit
	if err := json.Unmarshal([]byte(`{"sha":"x","commit":{
		"author":{"date":"2026-01-01T00:00:00Z"},
		"committer":{"date":"2026-03-01T00:00:00Z"}}}`), &c); err != nil {
		t.Fatal(err)
	}
	if got, want := c.At(), time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("At() = %s, want the committer date %s", got, want)
	}

	var authored Commit
	if err := json.Unmarshal([]byte(`{"sha":"y","commit":{
		"author":{"date":"2026-01-01T00:00:00Z"}}}`), &authored); err != nil {
		t.Fatal(err)
	}
	if got, want := authored.At(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("At() with no committer date = %s, want the author date %s", got, want)
	}
}

func TestCommitDetailFileSumsADirectory(t *testing.T) {
	detail := CommitDetail{SHA: "abc", Files: []CommitFile{
		{Filename: "README.md", Additions: 10, Deletions: 2},
		{Filename: "docs/a.md", Additions: 3, Deletions: 1},
		{Filename: "docs/b.md", Additions: 4, Deletions: 0},
		{Filename: "main.go", Additions: 90, Deletions: 90},
	}}

	if f, ok := detail.File("README.md"); !ok || f.Additions != 10 || f.Deletions != 2 {
		t.Errorf("File(README.md) = %+v, %v", f, ok)
	}
	// A tracked directory behaves like a file: every path beneath it is summed.
	f, ok := detail.File("docs/")
	if !ok || f.Additions != 7 || f.Deletions != 1 {
		t.Errorf("File(docs/) = %+v, %v; want +7/-1", f, ok)
	}
	if _, ok := detail.File("LICENSE"); ok {
		t.Error("File(LICENSE) reported a file the commit never touched")
	}
}

func TestForksDecodeCreationDates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("sort"); got != "oldest" {
			t.Errorf("sort = %q, want oldest", got)
		}
		fmt.Fprint(w, `[{"full_name":"a/r","created_at":"2026-02-03T04:05:06Z"}]`)
	}))
	defer srv.Close()

	c := New("o/r", "")
	c.BaseURL = srv.URL

	forks, raw, err := c.Forks(context.Background())
	if err != nil {
		t.Fatalf("Forks: %v", err)
	}
	if len(forks) != 1 || forks[0].FullName != "a/r" {
		t.Fatalf("got %+v, want one fork a/r", forks)
	}
	if forks[0].CreatedAt.Year() != 2026 {
		t.Errorf("created_at = %s, want a parsed 2026 date", forks[0].CreatedAt)
	}
	if len(raw) == 0 {
		t.Error("no raw bytes returned for the archive")
	}
}
