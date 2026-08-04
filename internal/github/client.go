// Package github is a minimal read-only client for the GitHub REST endpoints
// that expose a repository's distribution metrics. Every fetch returns both the
// decoded value and the exact bytes received, so callers can archive the raw
// response verbatim and re-derive from it later.
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultAPIBase is the GitHub REST API host. Tests point Client.BaseURL at an
// httptest server instead.
const DefaultAPIBase = "https://api.github.com"

const (
	userAgent = "git-stats"
	// apiVersion pins the REST schema so field meanings don't shift underneath
	// an archive that spans months.
	apiVersion = "2022-11-28"
	// maxBody bounds a single response read.
	maxBody = 64 << 20 // 64 MiB
	// maxPages bounds pagination so a runaway Link chain can't loop forever.
	maxPages = 100
)

// Client talks to one repository's metrics endpoints.
type Client struct {
	HTTP    *http.Client
	Repo    string // "owner/name"
	BaseURL string
	token   string
}

// New returns a client for repo. An empty token means unauthenticated
// requests, which still work for releases, repo stats and stargazers.
func New(repo, token string) *Client {
	return &Client{
		HTTP:    &http.Client{Timeout: 60 * time.Second},
		Repo:    repo,
		BaseURL: DefaultAPIBase,
		token:   token,
	}
}

// base returns the API host, tolerating a zero-valued Client.
func (c *Client) base() string {
	if c.BaseURL == "" {
		return DefaultAPIBase
	}
	return c.BaseURL
}

// TokenVars are the environment variables consulted for a credential, in
// order. The dedicated one comes first so a stats-only classic token can be
// used without changing whatever GH_TOKEN is scoped to elsewhere.
var TokenVars = []string{"GIT_STATS_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"}

// Token resolves the API token from the environment.
func Token() string {
	token, _ := TokenFrom()
	return token
}

// TokenFrom returns the token and the name of the variable it came from.
// Naming the source matters when diagnosing a 403: the usual cause is that a
// broadly-scoped GH_TOKEN was picked up instead of the dedicated one.
func TokenFrom() (token, source string) {
	for _, key := range TokenVars {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v, key
		}
	}
	return "", ""
}

// Authenticated reports whether the client sends a credential.
func (c *Client) Authenticated() bool { return c.token != "" }

// StatusError is a non-2xx response.
type StatusError struct {
	Code int
	URL  string
	Body string
}

func (e *StatusError) Error() string {
	body := strings.TrimSpace(e.Body)
	if len(body) > 200 {
		body = body[:200] + "…"
	}
	return fmt.Sprintf("GET %s: HTTP %d: %s", e.URL, e.Code, body)
}

// IsForbidden reports whether err means "this credential cannot read this
// endpoint" rather than "try again later". Rate limiting is deliberately
// excluded: it also arrives as 403 but is fixed by waiting, not by minting a
// better token.
//
//	401 — the endpoint requires a credential and none was accepted
//	403 — authenticated but not permitted (traffic needs a classic token)
//	404 — the repository is invisible to this credential
func IsForbidden(err error) bool {
	var se *StatusError
	if !errors.As(err, &se) {
		return false
	}
	switch se.Code {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return true
	default:
		return false
	}
}

// get fetches one URL and returns the raw body exactly as received.
func (c *Client) get(ctx context.Context, url string, accept string) ([]byte, *http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("Accept", accept)
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("X-GitHub-Api-Version", apiVersion)
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}

		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}

		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			return body, resp, nil
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			// Transient: retry.
			lastErr = &StatusError{Code: resp.StatusCode, URL: url, Body: string(body)}
			continue
		case resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0":
			// Rate limit exhaustion also arrives as 403; report when it lifts
			// rather than mislabelling it a permission problem.
			return nil, nil, fmt.Errorf("GET %s: rate limit exhausted, resets at %s",
				url, rateLimitReset(resp))
		default:
			return nil, nil, &StatusError{Code: resp.StatusCode, URL: url, Body: string(body)}
		}
	}
	return nil, nil, fmt.Errorf("GET %s: %w", url, lastErr)
}

func rateLimitReset(resp *http.Response) string {
	sec, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64)
	if err != nil {
		return "unknown"
	}
	return time.Unix(sec, 0).Format(time.RFC3339)
}

// getJSON fetches a single JSON object and returns its raw bytes.
func (c *Client) getJSON(ctx context.Context, path string) ([]byte, error) {
	body, _, err := c.get(ctx, c.base()+path, "application/vnd.github+json")
	return body, err
}

// nextLink extracts the rel="next" URL from a Link header.
var nextLinkRe = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="next"`)

func nextLink(h string) string {
	if m := nextLinkRe.FindStringSubmatch(h); m != nil {
		return m[1]
	}
	return ""
}

// getPaged walks a paginated array endpoint and returns every item's raw JSON.
// Items are kept as RawMessage so the archived merge is byte-faithful per
// element even for fields this package does not model.
func (c *Client) getPaged(ctx context.Context, path, accept string) ([]json.RawMessage, error) {
	url := c.base() + path
	var all []json.RawMessage

	for page := 0; url != "" && page < maxPages; page++ {
		body, resp, err := c.get(ctx, url, accept)
		if err != nil {
			return nil, err
		}
		var items []json.RawMessage
		if err := json.Unmarshal(body, &items); err != nil {
			return nil, fmt.Errorf("decoding %s: %w", url, err)
		}
		all = append(all, items...)
		url = nextLink(resp.Header.Get("Link"))
	}
	return all, nil
}

// mergeRaw renders paged items back into one JSON array for the archive.
func mergeRaw(items []json.RawMessage) ([]byte, error) {
	if items == nil {
		items = []json.RawMessage{}
	}
	return json.Marshal(items)
}
