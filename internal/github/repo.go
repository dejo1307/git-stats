package github

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Repo holds the cumulative repository counters.
type Repo struct {
	FullName string `json:"full_name"`
	Stars    int64  `json:"stargazers_count"`
	Forks    int64  `json:"forks_count"`
	// Subscribers is the real watcher count. The API's "watchers_count" is a
	// duplicate of the star count and is deliberately not used here.
	Subscribers int64 `json:"subscribers_count"`
	OpenIssues  int64 `json:"open_issues_count"`
}

// Repository returns the repository's current counters.
func (c *Client) Repository(ctx context.Context) (Repo, []byte, error) {
	raw, err := c.getJSON(ctx, "/repos/"+c.Repo)
	if err != nil {
		return Repo{}, nil, err
	}
	var r Repo
	if err := json.Unmarshal(raw, &r); err != nil {
		return Repo{}, nil, fmt.Errorf("decoding repository: %w", err)
	}
	return r, raw, nil
}

// Stargazer is one star with the time it was given.
type Stargazer struct {
	StarredAt time.Time `json:"starred_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
}

// Stargazers returns the full star history. Unlike every other metric here
// this one is retroactively complete: each entry carries its own starred_at,
// so it can be backfilled once rather than sampled over time.
func (c *Client) Stargazers(ctx context.Context) ([]Stargazer, []byte, error) {
	// The star+json media type is what adds starred_at to each entry.
	items, err := c.getPaged(ctx, "/repos/"+c.Repo+"/stargazers?per_page=100",
		"application/vnd.github.star+json")
	if err != nil {
		return nil, nil, err
	}
	raw, err := mergeRaw(items)
	if err != nil {
		return nil, nil, err
	}
	var stars []Stargazer
	if err := json.Unmarshal(raw, &stars); err != nil {
		return nil, nil, fmt.Errorf("decoding stargazers: %w", err)
	}
	return stars, raw, nil
}
