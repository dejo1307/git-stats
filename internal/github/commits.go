package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Commit is one commit touching a tracked path.
//
// Two dates exist on every commit and they are not interchangeable. The author
// date records when the change was written; the committer date records when the
// object that is actually on the branch was created. A rebase, squash merge or
// cherry-pick rewrites the second and preserves the first, so an author date can
// sit weeks before the change ever appeared on the repository page. What matters
// for a timeline overlay is when a reader could first see the change, so At uses
// the committer date and falls back to the author date only when it is absent.
type Commit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message string `json:"message"`
		Author  struct {
			Date time.Time `json:"date"`
		} `json:"author"`
		Committer struct {
			Date time.Time `json:"date"`
		} `json:"committer"`
	} `json:"commit"`
}

// At returns the time the commit landed.
func (c Commit) At() time.Time {
	if !c.Commit.Committer.Date.IsZero() {
		return c.Commit.Committer.Date
	}
	return c.Commit.Author.Date
}

// Subject is the first line of the commit message, which is what a timeline
// entry has room for.
func (c Commit) Subject() string {
	subject, _, _ := strings.Cut(c.Commit.Message, "\n")
	return strings.TrimSpace(subject)
}

// PathCommits is the commit history of one tracked path.
//
// The path travels with the commits rather than only in the archive filename,
// so replaying an archive does not depend on the tracked-path configuration
// still holding whatever value it had when the snapshot was taken.
type PathCommits struct {
	Path    string            `json:"path"`
	Commits []json.RawMessage `json:"commits"`
}

// CommitsForPath returns every commit touching path, newest first, along with
// the JSON to archive.
//
// Unlike traffic this history has no retention limit, so one fetch is complete
// back to the repository's first commit. path may name a file or a directory
// prefix; a directory on a busy repository can run to thousands of commits, and
// pagination stops at the client's page ceiling.
func (c *Client) CommitsForPath(ctx context.Context, path string) ([]Commit, []byte, error) {
	items, err := c.getPaged(ctx,
		"/repos/"+c.Repo+"/commits?per_page=100&path="+url.QueryEscape(path),
		"application/vnd.github+json")
	if err != nil {
		return nil, nil, err
	}
	if items == nil {
		items = []json.RawMessage{}
	}

	list, err := mergeRaw(items)
	if err != nil {
		return nil, nil, err
	}
	var commits []Commit
	if err := json.Unmarshal(list, &commits); err != nil {
		return nil, nil, fmt.Errorf("decoding commits for %s: %w", path, err)
	}
	raw, err := json.Marshal(PathCommits{Path: path, Commits: items})
	if err != nil {
		return nil, nil, err
	}
	return commits, raw, nil
}

// CommitFile is one file's change size within a commit.
type CommitFile struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int64  `json:"additions"`
	Deletions int64  `json:"deletions"`
}

// CommitDetail is one commit with its per-file diff stats, which the commit
// list endpoint does not carry.
type CommitDetail struct {
	SHA   string       `json:"sha"`
	Files []CommitFile `json:"files"`
}

// File returns the entry for path, if the commit touched it.
func (d CommitDetail) File(path string) (CommitFile, bool) {
	for _, f := range d.Files {
		if f.Filename == path {
			return f, true
		}
	}
	// A tracked directory prefix matches any file beneath it; the sizes of
	// those files are summed into one entry so a directory behaves like a file.
	var sum CommitFile
	var found bool
	for _, f := range d.Files {
		if strings.HasPrefix(f.Filename, strings.TrimSuffix(path, "/")+"/") {
			sum.Filename = path
			sum.Status = "modified"
			sum.Additions += f.Additions
			sum.Deletions += f.Deletions
			found = true
		}
	}
	return sum, found
}

// CommitDetail fetches one commit including its diff stats. A commit is
// immutable, so a response is worth caching forever rather than re-fetching:
// this is one request per commit and the only per-commit cost in the tool.
func (c *Client) CommitDetail(ctx context.Context, sha string) (CommitDetail, []byte, error) {
	raw, err := c.getJSON(ctx, "/repos/"+c.Repo+"/commits/"+url.PathEscape(sha))
	if err != nil {
		return CommitDetail{}, nil, err
	}
	var d CommitDetail
	if err := json.Unmarshal(raw, &d); err != nil {
		return CommitDetail{}, nil, fmt.Errorf("decoding commit %s: %w", sha, err)
	}
	return d, raw, nil
}

// Fork is one fork with the time it was created.
type Fork struct {
	FullName  string    `json:"full_name"`
	CreatedAt time.Time `json:"created_at"`
}

// Forks returns every fork, oldest first. Like the star history this is
// retroactively complete — each fork carries its own created_at — so it is a
// backfill rather than a series that has to be sampled over time.
func (c *Client) Forks(ctx context.Context) ([]Fork, []byte, error) {
	items, err := c.getPaged(ctx, "/repos/"+c.Repo+"/forks?per_page=100&sort=oldest",
		"application/vnd.github+json")
	if err != nil {
		return nil, nil, err
	}
	raw, err := mergeRaw(items)
	if err != nil {
		return nil, nil, err
	}
	var forks []Fork
	if err := json.Unmarshal(raw, &forks); err != nil {
		return nil, nil, fmt.Errorf("decoding forks: %w", err)
	}
	return forks, raw, nil
}
